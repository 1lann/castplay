package hap

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const maxBlock = 1024

// Conn wraps a control connection after pairing. Each direction is a stream
// of blocks: 2-byte little-endian length (also the AAD), ciphertext, 16-byte
// tag, with a 64-bit little-endian counter as nonce.
type Conn struct {
	net.Conn
	read, write cipher.AEAD

	rmu     sync.Mutex
	rcount  uint64
	pending []byte

	wmu    sync.Mutex
	wcount uint64
}

// NewConn derives the control-channel keys from the pairing shared secret.
// The accessory writes with the "Read" key and reads with the "Write" key
// (they are named from the controller's point of view).
func NewConn(c net.Conn, shared []byte) (*Conn, error) {
	return newConn(c, shared, "Control-Salt", "Control-Write-Encryption-Key", "Control-Read-Encryption-Key")
}

func newConn(c net.Conn, shared []byte, salt, readInfo, writeInfo string) (*Conn, error) {
	r, err := chacha20poly1305.New(deriveKey(shared, salt, readInfo))
	if err != nil {
		return nil, err
	}
	w, err := chacha20poly1305.New(deriveKey(shared, salt, writeInfo))
	if err != nil {
		return nil, err
	}
	return &Conn{Conn: c, read: r, write: w}, nil
}

func counterNonce(n uint64) []byte {
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], n)
	return nonce
}

func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(c.pending) == 0 {
		var hdr [2]byte
		if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
			return 0, err
		}
		n := int(binary.LittleEndian.Uint16(hdr[:]))
		if n > maxBlock {
			return 0, errors.New("hap: oversized encrypted block")
		}
		block := make([]byte, n+16)
		if _, err := io.ReadFull(c.Conn, block); err != nil {
			return 0, err
		}
		plain, err := c.read.Open(block[:0], counterNonce(c.rcount), block, hdr[:])
		if err != nil {
			return 0, errors.New("hap: block authentication failed")
		}
		c.rcount++
		c.pending = plain
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var out []byte
	for off := 0; off < len(p); {
		n := min(len(p)-off, maxBlock)
		var hdr [2]byte
		binary.LittleEndian.PutUint16(hdr[:], uint16(n))
		out = append(out, hdr[:]...)
		out = c.write.Seal(out, counterNonce(c.wcount), p[off:off+n], hdr[:])
		c.wcount++
		off += n
	}
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
