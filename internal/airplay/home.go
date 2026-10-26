package airplay

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"

	"castplay/internal/airplay/hap"
)

// Status flags (airplay2-receiver's StatusFlags).
const (
	statusAudioLink       = 1 << 2
	statusHKAccessControl = 1 << 10 // in a Home: iOS stops offering "Add to Home"
)

// home is a speaker's Home membership: the controllers Home paired with
// /pair-add and the settings it pushed with /configure. It is persisted so the
// speaker stays in the Home across restarts.
//
// Access control is recorded and echoed back but not enforced: pair-verify
// accepts any controller, so anyone on the LAN can still play.
type home struct {
	path string // "" keeps it in memory only

	mu    sync.Mutex
	state homeState
}

type homeState struct {
	Controllers   []hap.Controller `json:"controllers"`
	AccessControl bool             `json:"accessControl"` // Enable_HK_Access_Control
	ACL           int64            `json:"acl"`           // 0 everyone, 1 Home members, 2 admins
	Password      string           `json:"password,omitempty"`
}

func loadHome(path string) (*home, error) {
	h := &home{path: path}
	if path == "" {
		return h, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	return h, json.Unmarshal(b, &h.state)
}

// save writes the state; called with mu held.
func (h *home) save() error {
	if h.path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(h.state, "", "  ")
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

func (h *home) AddPairing(c hap.Controller) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Controllers = slices.DeleteFunc(h.state.Controllers, func(o hap.Controller) bool { return o.ID == c.ID })
	h.state.Controllers = append(h.state.Controllers, c)
	return h.save()
}

// RemovePairing forgets a controller. Once the last one is gone the speaker
// has been removed from the Home, so its Home settings go too.
func (h *home) RemovePairing(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state.Controllers = slices.DeleteFunc(h.state.Controllers, func(o hap.Controller) bool { return o.ID == id })
	if len(h.state.Controllers) == 0 {
		h.state = homeState{}
	}
	return h.save()
}

func (h *home) ListPairings() []hap.Controller {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.state.Controllers)
}

// configure applies a /configure ConfigurationDictionary.
func (h *home) configure(cd map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if acl, ok := pInt(cd, "Access_Control_Level"); ok {
		h.state.ACL = acl
	}
	if v, ok := cd["Enable_HK_Access_Control"].(bool); ok {
		h.state.AccessControl = v
	}
	if pw, ok := cd["Password"].(string); ok {
		h.state.Password = pw
	}
	return h.save()
}

func (h *home) snapshot() homeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func (h *home) statusFlags() int {
	st := h.snapshot()
	f := statusAudioLink
	if st.AccessControl && len(st.Controllers) > 0 {
		f |= statusHKAccessControl
	}
	return f
}
