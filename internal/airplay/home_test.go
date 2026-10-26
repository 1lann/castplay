package airplay

import (
	"path/filepath"
	"testing"

	"castplay/internal/airplay/hap"
)

func TestHomePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home.json")
	h, err := loadHome(path)
	if err != nil {
		t.Fatal(err)
	}
	if h.statusFlags() != statusAudioLink {
		t.Fatalf("fresh flags %#x", h.statusFlags())
	}
	if err := h.AddPairing(hap.Controller{ID: "home", PublicKey: make([]byte, 32), Admin: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.configure(map[string]any{"Enable_HK_Access_Control": true, "Access_Control_Level": uint64(1)}); err != nil {
		t.Fatal(err)
	}

	h, err = loadHome(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := h.snapshot(); len(st.Controllers) != 1 || st.ACL != 1 {
		t.Fatalf("reloaded %+v", st)
	}
	if h.statusFlags() != statusAudioLink|statusHKAccessControl {
		t.Fatalf("paired flags %#x", h.statusFlags())
	}

	if err := h.RemovePairing("home"); err != nil {
		t.Fatal(err)
	}
	if st := h.snapshot(); st.AccessControl || st.ACL != 0 || h.statusFlags() != statusAudioLink {
		t.Fatalf("after removal %+v", st)
	}
}
