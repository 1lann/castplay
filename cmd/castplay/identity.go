package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"castplay/internal/airplay/hap"
)

// identity is what makes one bridged speaker look like the same AirPlay
// device across restarts. It is derived from the Cast device's ID where
// possible and persisted in the state directory.
type identity struct {
	DeviceID  string `json:"deviceID"`  // MAC-style
	PairingID string `json:"pairingID"` // UUID
	GroupID   string `json:"groupID"`   // UUID
	Seed      []byte `json:"ed25519Seed"`
}

func (id identity) hap() hap.Identity {
	return hap.Identity{PairingID: id.PairingID, Key: ed25519.NewKeyFromSeed(id.Seed)}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// loadIdentity returns the persisted identity for key, creating it on first
// use. key is the Cast device ID (or name for manual targets).
func loadIdentity(dir, key string) (identity, error) {
	path := filepath.Join(dir, "speaker-"+unsafeChars.ReplaceAllString(key, "_")+".json")
	var id identity
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &id); err == nil && len(id.Seed) == ed25519.SeedSize {
			return id, nil
		}
	}
	h := sha256.Sum256([]byte("castplay:" + key))
	// Locally administered, unicast MAC derived from the key.
	mac := h[:6]
	mac[0] = mac[0]&0xfc | 0x02
	parts := make([]string, 6)
	for i, b := range mac {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	id.DeviceID = strings.Join(parts, ":")
	id.PairingID = uuidFrom(h[6:22])
	id.GroupID = uuidFrom(sha256Sum("castplay-group:" + key)[:16])
	id.Seed = make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(id.Seed); err != nil {
		return id, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return id, err
	}
	b, _ := json.MarshalIndent(id, "", "  ")
	return id, os.WriteFile(path, b, 0o600)
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func uuidFrom(b []byte) string {
	b = append([]byte(nil), b[:16]...)
	b[6] = b[6]&0x0f | 0x40 // version 4 layout
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
