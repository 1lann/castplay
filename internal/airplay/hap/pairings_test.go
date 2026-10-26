package hap

import (
	"bytes"
	"slices"
	"testing"
)

type memPairings []Controller

func (m *memPairings) AddPairing(c Controller) error { *m = append(*m, c); return nil }
func (m *memPairings) RemovePairing(id string) error {
	*m = slices.DeleteFunc(*m, func(c Controller) bool { return c.ID == id })
	return nil
}
func (m *memPairings) ListPairings() []Controller { return *m }

func TestPairings(t *testing.T) {
	var p memPairings
	key := bytes.Repeat([]byte{7}, 32)
	add := func(id string) {
		t.Helper()
		resp, err := HandlePairings(&p, TLV{{TagState, []byte{1}}, {TagMethod, []byte{methodAddPairing}},
			{TagIdentifier, []byte(id)}, {TagPublicKey, key}, {TagPermissions, []byte{1}}}.Encode())
		if err != nil || !bytes.Equal(resp, stateTLV(2)) {
			t.Fatalf("add %s: %x %v", id, resp, err)
		}
	}
	add("home-a")
	add("home-b")
	if len(p) != 2 || !p[0].Admin || p[0].ID != "home-a" {
		t.Fatalf("pairings %+v", p)
	}

	resp, err := HandlePairings(&p, TLV{{TagState, []byte{1}}, {TagMethod, []byte{methodListPairings}}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	want := stateTLV(2,
		TLVItem{TagIdentifier, []byte("home-a")}, TLVItem{TagPublicKey, key}, TLVItem{TagPermissions, []byte{1}},
		TLVItem{TagSeparator, nil},
		TLVItem{TagIdentifier, []byte("home-b")}, TLVItem{TagPublicKey, key}, TLVItem{TagPermissions, []byte{1}})
	if !bytes.Equal(resp, want) {
		t.Fatalf("list:\n got %x\nwant %x", resp, want)
	}

	if _, err := HandlePairings(&p, TLV{{TagState, []byte{1}}, {TagMethod, []byte{methodRemovePairing}},
		{TagIdentifier, []byte("home-a")}}.Encode()); err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || p[0].ID != "home-b" {
		t.Fatalf("after remove %+v", p)
	}
}
