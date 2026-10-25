package hap

import (
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"math/big"
	"strings"
)

// SRP-6a with the RFC 5054 3072-bit group, g=5 and SHA-512, as HomeKit uses.
// Big integers are hashed in their minimal big-endian form except where the
// protocol pads them to the length of N (k and u).

var (
	srpN = func() *big.Int {
		n, _ := new(big.Int).SetString(strings.Join(strings.Fields(`
			FFFFFFFF FFFFFFFF C90FDAA2 2168C234 C4C6628B 80DC1CD1 29024E08
			8A67CC74 020BBEA6 3B139B22 514A0879 8E3404DD EF9519B3 CD3A431B
			302B0A6D F25F1437 4FE1356D 6D51C245 E485B576 625E7EC6 F44C42E9
			A637ED6B 0BFF5CB6 F406B7ED EE386BFB 5A899FA5 AE9F2411 7C4B1FE6
			49286651 ECE45B3D C2007CB8 A163BF05 98DA4836 1C55D39A 69163FA8
			FD24CF5F 83655D23 DCA3AD96 1C62F356 208552BB 9ED52907 7096966D
			670C354E 4ABC9804 F1746C08 CA18217C 32905E46 2E36CE3B E39E772C
			180E8603 9B2783A2 EC07A28F B5C55DF0 6F4C52C9 DE2BCBF6 95581718
			3995497C EA956AE5 15D22618 98FA0510 15728E5A 8AAAC42D AD33170D
			04507A33 A85521AB DF1CBA64 ECFB8504 58DBEF0A 8AEA7157 5D060C7D
			B3970F85 A6E1E4C7 ABF5AE8C DB0933D7 1E8C94E0 4A25619D CEE3D226
			1AD2EE6B F12FFA06 D98A0864 D8760273 3EC86A64 521F2B18 177B200C
			BBE11757 7A615D6C 770988C0 BAD946E2 08E24FA0 74E5AB31 43DB5BFC
			E0FD108E 4B82D120 A93AD2CA FFFFFFFF FFFFFFFF`), ""), 16)
		return n
	}()
	srpG    = big.NewInt(5)
	srpNLen = (srpN.BitLen() + 7) / 8
)

func hashBytes(parts ...[]byte) []byte {
	h := sha512.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func pad(b []byte) []byte {
	if len(b) >= srpNLen {
		return b
	}
	return append(make([]byte, srpNLen-len(b)), b...)
}

func bytesToInt(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

// srpServer is one Pair-Setup SRP session on the accessory side.
type srpServer struct {
	username []byte
	salt     []byte
	v, b, B  *big.Int
	key      []byte // K, the session key
	m1       []byte
}

func newSRPServer(username, password string) (*srpServer, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	salt[0] |= 0x80 // keep the minimal encoding exactly 16 bytes
	inner := hashBytes([]byte(username + ":" + password))
	x := bytesToInt(hashBytes(salt, inner))
	v := new(big.Int).Exp(srpG, x, srpN)

	bBytes := make([]byte, 64)
	if _, err := rand.Read(bBytes); err != nil {
		return nil, err
	}
	b := bytesToInt(bBytes)
	k := bytesToInt(hashBytes(srpN.Bytes(), pad(srpG.Bytes())))
	B := new(big.Int).Mul(k, v)
	B.Add(B, new(big.Int).Exp(srpG, b, srpN))
	B.Mod(B, srpN)
	return &srpServer{username: []byte(username), salt: salt, v: v, b: b, B: B}, nil
}

func (s *srpServer) publicKey() []byte { return s.B.Bytes() }

// verify takes the client's public key A and proof M1, and returns the
// server proof M2. The shared session key is then available as s.key.
func (s *srpServer) verify(aBytes, m1 []byte) ([]byte, error) {
	A := bytesToInt(aBytes)
	if new(big.Int).Mod(A, srpN).Sign() == 0 {
		return nil, errors.New("srp: invalid client public key")
	}
	u := bytesToInt(hashBytes(pad(A.Bytes()), pad(s.B.Bytes())))
	S := new(big.Int).Exp(s.v, u, srpN)
	S.Mul(S, A)
	S.Exp(S, s.b, srpN)
	s.key = hashBytes(S.Bytes())

	hn, hg := hashBytes(srpN.Bytes()), hashBytes(srpG.Bytes())
	for i := range hn {
		hn[i] ^= hg[i]
	}
	expected := hashBytes(hn, hashBytes(s.username), s.salt, A.Bytes(), s.B.Bytes(), s.key)
	if subtle.ConstantTimeCompare(expected, m1) != 1 {
		return nil, errors.New("srp: client proof mismatch")
	}
	s.m1 = expected
	return hashBytes(A.Bytes(), expected, s.key), nil
}
