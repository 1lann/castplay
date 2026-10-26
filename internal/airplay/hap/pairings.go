package hap

import (
	"errors"
	"fmt"
)

// Controller is a HomeKit controller paired with the accessory: Home adds
// its own key with /pair-add when the speaker is added to a Home.
type Controller struct {
	ID        string `json:"id"`
	PublicKey []byte `json:"publicKey"`
	Admin     bool   `json:"admin"`
}

// Pairings is the accessory's persistent controller list.
type Pairings interface {
	AddPairing(c Controller) error
	RemovePairing(id string) error
	ListPairings() []Controller
}

const permissionAdmin = 1

// HandlePairings answers one /pair-add, /pair-remove or /pair-list request
// body (HAP Add, Remove and List Pairings, M1 to M2). Requests arrive on a
// verified, encrypted control channel; as with pair-verify, the requesting
// controller's own admin bit is not checked.
func HandlePairings(p Pairings, body []byte) ([]byte, error) {
	req, err := DecodeTLV(body)
	if err != nil {
		return nil, err
	}
	if byteOf(req[TagState]) != 1 {
		return errorTLV(2, ErrUnknown), fmt.Errorf("hap: unexpected pairings state %d", byteOf(req[TagState]))
	}
	switch method := byteOf(req[TagMethod]); method {
	case methodAddPairing:
		id, pub := string(req[TagIdentifier]), req[TagPublicKey]
		if id == "" || len(pub) != 32 {
			return errorTLV(2, ErrUnknown), errors.New("hap: add pairing without identifier or key")
		}
		c := Controller{ID: id, PublicKey: pub, Admin: byteOf(req[TagPermissions])&permissionAdmin != 0}
		if err := p.AddPairing(c); err != nil {
			return errorTLV(2, ErrUnknown), err
		}
		return stateTLV(2), nil
	case methodRemovePairing:
		if err := p.RemovePairing(string(req[TagIdentifier])); err != nil {
			return errorTLV(2, ErrUnknown), err
		}
		return stateTLV(2), nil
	case methodListPairings:
		var items []TLVItem
		for i, c := range p.ListPairings() {
			if i > 0 {
				items = append(items, TLVItem{TagSeparator, nil})
			}
			perm := byte(0)
			if c.Admin {
				perm = permissionAdmin
			}
			items = append(items, TLVItem{TagIdentifier, []byte(c.ID)}, TLVItem{TagPublicKey, c.PublicKey},
				TLVItem{TagPermissions, []byte{perm}})
		}
		return stateTLV(2, items...), nil
	default:
		return errorTLV(2, ErrUnknown), fmt.Errorf("hap: unsupported pairings method %d", method)
	}
}
