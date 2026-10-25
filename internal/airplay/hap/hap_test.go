package hap

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"math/big"
	"net"
	"testing"
)

// srpClient is the controller side of SRP-6a, written straight from RFC 5054
// so the server is checked against an independent implementation.
func srpClient(t *testing.T, salt, B []byte, user, pass string) (A, m1, key []byte) {
	a := bytesToInt(randBytes(t, 32))
	Ai := new(big.Int).Exp(srpG, a, srpN)
	Bi := bytesToInt(B)
	u := bytesToInt(hashBytes(pad(Ai.Bytes()), pad(B)))
	k := bytesToInt(hashBytes(srpN.Bytes(), pad(srpG.Bytes())))
	x := bytesToInt(hashBytes(salt, hashBytes([]byte(user+":"+pass))))
	// S = (B - k*g^x) ^ (a + u*x) mod N
	base := new(big.Int).Sub(Bi, new(big.Int).Mul(k, new(big.Int).Exp(srpG, x, srpN)))
	base.Mod(base, srpN)
	exp := new(big.Int).Add(a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, srpN)
	key = hashBytes(S.Bytes())
	hn, hg := hashBytes(srpN.Bytes()), hashBytes(srpG.Bytes())
	for i := range hn {
		hn[i] ^= hg[i]
	}
	m1 = hashBytes(hn, hashBytes([]byte(user)), salt, Ai.Bytes(), B, key)
	return Ai.Bytes(), m1, key
}

func randBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func testIdentity(t *testing.T) Identity {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{PairingID: "11111111-2222-3333-4444-555555555555", Key: priv}
}

func TestTransientPairSetupAndConn(t *testing.T) {
	s := NewSession(testIdentity(t), "")
	m2, err := s.PairSetup(TLV{{TagMethod, []byte{0}}, {TagState, []byte{1}}, {TagFlags, []byte{0x10}}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := DecodeTLV(m2)
	if len(r2[TagPublicKey]) < 380 || len(r2[TagSalt]) != 16 {
		t.Fatalf("bad M2: pk %d bytes, salt %d bytes", len(r2[TagPublicKey]), len(r2[TagSalt]))
	}
	A, m1, ckey := srpClient(t, r2[TagSalt], r2[TagPublicKey], "Pair-Setup", TransientPIN)
	m4, err := s.PairSetup(TLV{{TagState, []byte{3}}, {TagPublicKey, A}, {TagProof, m1}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	r4, _ := DecodeTLV(m4)
	if _, ok := r4[TagError]; ok {
		t.Fatalf("M4 error %v", r4[TagError])
	}
	if !bytes.Equal(s.SharedKey, ckey) {
		t.Fatal("session keys differ")
	}

	// Wrong PIN must fail.
	s2 := NewSession(testIdentity(t), "")
	m2b, _ := s2.PairSetup(TLV{{TagMethod, []byte{0}}, {TagState, []byte{1}}, {TagFlags, []byte{0x10}}}.Encode())
	r2b, _ := DecodeTLV(m2b)
	A2, m1b, _ := srpClient(t, r2b[TagSalt], r2b[TagPublicKey], "Pair-Setup", "0000")
	if _, err := s2.PairSetup(TLV{{TagState, []byte{3}}, {TagPublicKey, A2}, {TagProof, m1b}}.Encode()); err == nil {
		t.Fatal("wrong PIN accepted")
	}

	// Encrypted framing: the controller side swaps the key names.
	a, b := net.Pipe()
	acc, _ := NewConn(a, s.SharedKey)
	ctl, _ := newConn(b, ckey, "Control-Salt", "Control-Read-Encryption-Key", "Control-Write-Encryption-Key")
	msg := bytes.Repeat([]byte("RTSP/1.0 200 OK\r\n"), 200) // spans several blocks
	go func() { ctl.Write(msg) }()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(acc, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("controller->accessory: %v", err)
	}
	go func() { acc.Write([]byte("hello")) }()
	got = make([]byte, 5)
	if _, err := io.ReadFull(ctl, got); err != nil || string(got) != "hello" {
		t.Fatalf("accessory->controller: %v %q", err, got)
	}
}

func TestTLVFragments(t *testing.T) {
	v := bytes.Repeat([]byte{7}, 600)
	enc := TLV{{TagPublicKey, v}, {TagState, []byte{2}}}.Encode()
	dec, err := DecodeTLV(enc)
	if err != nil || !bytes.Equal(dec[TagPublicKey], v) || dec[TagState][0] != 2 {
		t.Fatalf("round trip failed: %v", err)
	}
}
