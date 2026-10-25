package hap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// TransientPIN is the fixed setup code AirPlay senders use for transient
// pairing when no password is configured.
const TransientPIN = "3939"

// Identity is the accessory's long-term pairing identity.
type Identity struct {
	PairingID string // the "pi" advertised in mDNS and /info
	Key       ed25519.PrivateKey
}

func (id Identity) PublicKey() ed25519.PublicKey { return id.Key.Public().(ed25519.PublicKey) }

// Session holds pairing state for one control connection. When a step
// completes the connection's shared secret, SharedKey becomes non-nil and the
// caller must switch the connection to encrypted framing after sending the
// response.
type Session struct {
	id  Identity
	pin string

	srp       *srpServer
	transient bool

	verifySecret    []byte
	verifyOurPub    []byte
	verifyTheirPub  []byte
	verifySessionKy []byte

	SharedKey    []byte
	ControllerID string
}

func NewSession(id Identity, pin string) *Session {
	if pin == "" {
		pin = TransientPIN
	}
	return &Session{id: id, pin: pin}
}

// PairSetup handles one /pair-setup request body and returns the response.
// Only transient pair-setup (M1-M4) is supported: senders use it to stream
// to speakers that aren't part of their Home.
func (s *Session) PairSetup(body []byte) ([]byte, error) {
	req, err := DecodeTLV(body)
	if err != nil {
		return nil, err
	}
	state := byteOf(req[TagState])
	switch state {
	case 1:
		method := byteOf(req[TagMethod])
		flags := leUint(req[TagFlags])
		if method != methodPairSetup || flags&flagTransient == 0 {
			// Normal (non-transient) pairing needs persistent controller
			// storage and a displayed setup code; not supported.
			return errorTLV(2, ErrUnavailable), fmt.Errorf("hap: unsupported pair-setup (method %d, flags %#x)", method, flags)
		}
		s.transient = true
		s.srp, err = newSRPServer("Pair-Setup", s.pin)
		if err != nil {
			return nil, err
		}
		return stateTLV(2,
			TLVItem{TagSalt, s.srp.salt},
			TLVItem{TagPublicKey, s.srp.publicKey()},
			TLVItem{TagFlags, req[TagFlags]},
		), nil
	case 3:
		if s.srp == nil {
			return errorTLV(4, ErrAuthentication), errors.New("hap: pair-setup M3 without M1")
		}
		proof, err := s.srp.verify(req[TagPublicKey], req[TagProof])
		if err != nil {
			return errorTLV(4, ErrAuthentication), err
		}
		if s.transient {
			s.SharedKey = s.srp.key
		}
		return stateTLV(4, TLVItem{TagProof, proof}), nil
	}
	return errorTLV(state+1, ErrUnavailable), fmt.Errorf("hap: unexpected pair-setup state %d", state)
}

// PairVerify handles one /pair-verify request body. Controllers are not
// checked against a pairing list: anyone on the network may play to the
// speaker, exactly as with transient pairing.
func (s *Session) PairVerify(body []byte) ([]byte, error) {
	req, err := DecodeTLV(body)
	if err != nil {
		return nil, err
	}
	switch byteOf(req[TagState]) {
	case 1:
		theirPub := req[TagPublicKey]
		if len(theirPub) != 32 {
			return errorTLV(2, ErrAuthentication), errors.New("hap: bad pair-verify public key")
		}
		priv := make([]byte, 32)
		if _, err := rand.Read(priv); err != nil {
			return nil, err
		}
		ourPub, err := curve25519.X25519(priv, curve25519.Basepoint)
		if err != nil {
			return nil, err
		}
		shared, err := curve25519.X25519(priv, theirPub)
		if err != nil {
			return errorTLV(2, ErrAuthentication), err
		}
		info := append(append(append([]byte{}, ourPub...), s.id.PairingID...), theirPub...)
		sig := ed25519.Sign(s.id.Key, info)
		sub := TLV{{TagIdentifier, []byte(s.id.PairingID)}, {TagSignature, sig}}.Encode()
		key := deriveKey(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
		enc, err := seal(key, "PV-Msg02", sub)
		if err != nil {
			return nil, err
		}
		s.verifySecret, s.verifyOurPub, s.verifyTheirPub, s.verifySessionKy = shared, ourPub, theirPub, key
		return stateTLV(2, TLVItem{TagPublicKey, ourPub}, TLVItem{TagEncryptedData, enc}), nil
	case 3:
		if s.verifySessionKy == nil {
			return errorTLV(4, ErrAuthentication), errors.New("hap: pair-verify M3 without M1")
		}
		plain, err := open(s.verifySessionKy, "PV-Msg03", req[TagEncryptedData])
		if err != nil {
			return errorTLV(4, ErrAuthentication), err
		}
		sub, err := DecodeTLV(plain)
		if err != nil {
			return errorTLV(4, ErrAuthentication), err
		}
		s.ControllerID = string(sub[TagIdentifier])
		s.SharedKey = s.verifySecret
		return stateTLV(4), nil
	}
	return nil, fmt.Errorf("hap: unexpected pair-verify state %d", byteOf(req[TagState]))
}

func deriveKey(secret []byte, salt, info string) []byte {
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha512.New, secret, []byte(salt), []byte(info)), key); err != nil {
		panic(err) // 32 bytes of HKDF-SHA512 output cannot fail
	}
	return key
}

// pairingNonce pads an ASCII label such as "PV-Msg02" to the 12-byte nonce.
func pairingNonce(label string) []byte {
	n := make([]byte, 12)
	copy(n[4:], label)
	return n
}

func seal(key []byte, label string, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, pairingNonce(label), plain, nil), nil
}

func open(key []byte, label string, sealed []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, pairingNonce(label), sealed, nil)
}

func byteOf(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// leUint reads a TLV integer, which HAP encodes little-endian.
func leUint(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}
