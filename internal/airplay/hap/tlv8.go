// Package hap implements the HomeKit pairing pieces AirPlay 2 receivers need:
// TLV8 encoding, SRP-6a (3072-bit, SHA-512), transient Pair-Setup, Pair-Verify,
// the Home controller list (Add, Remove and List Pairings)
// and the ChaCha20-Poly1305 framing used on the control channel afterwards.
//
// The flows follow the HomeKit Accessory Protocol spec and the Python
// receiver in openairplay/airplay2-receiver (ap2/pairing/hap.py).
package hap

import "errors"

// TLV8 types used by pairing.
const (
	TagMethod        = 0
	TagIdentifier    = 1
	TagSalt          = 2
	TagPublicKey     = 3
	TagProof         = 4
	TagEncryptedData = 5
	TagState         = 6
	TagError         = 7
	TagSignature     = 10
	TagPermissions   = 11
	TagFlags         = 19
	TagSeparator     = 255
)

const (
	ErrUnknown        = 1
	ErrAuthentication = 2
	ErrUnavailable    = 6
)

const (
	methodPairSetup         = 0
	methodPairSetupWithAuth = 1
	methodAddPairing        = 3
	methodRemovePairing     = 4
	methodListPairings      = 5
	flagTransient           = 1 << 4
)

// TLV is an ordered list of type/value items.
type TLV []TLVItem

type TLVItem struct {
	Type  byte
	Value []byte
}

// DecodeTLV parses TLV8, joining fragments of consecutive items of the same
// type (values over 255 bytes are split across several items).
func DecodeTLV(b []byte) (map[byte][]byte, error) {
	out := map[byte][]byte{}
	var last byte
	first := true
	for len(b) > 0 {
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			return nil, errors.New("hap: truncated TLV8")
		}
		t, v := b[0], b[2:2+int(b[1])]
		if !first && t == last {
			out[t] = append(out[t], v...)
		} else {
			out[t] = append([]byte(nil), v...)
		}
		first, last = false, t
		b = b[2+int(b[1]):]
	}
	return out, nil
}

// Encode serialises the items, fragmenting values longer than 255 bytes.
func (t TLV) Encode() []byte {
	var out []byte
	for _, it := range t {
		v := it.Value
		if len(v) == 0 {
			out = append(out, it.Type, 0)
			continue
		}
		for len(v) > 0 {
			n := min(len(v), 255)
			out = append(out, it.Type, byte(n))
			out = append(out, v[:n]...)
			v = v[n:]
		}
	}
	return out
}

func stateTLV(state byte, items ...TLVItem) []byte {
	return append(TLV{{TagState, []byte{state}}}, items...).Encode()
}

func errorTLV(state, code byte) []byte {
	return stateTLV(state, TLVItem{TagError, []byte{code}})
}
