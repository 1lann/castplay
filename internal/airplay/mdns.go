package airplay

import (
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/grandcat/zeroconf"
)

// Advertise publishes the receiver as _airplay._tcp on the given interfaces
// (nil = all). AirPlay 2 senders discover speakers only through this record;
// _raop._tcp is deliberately not published since it would invite AirPlay 1
// (ANNOUNCE) sessions this receiver doesn't support.
func (r *Receiver) Advertise(ifaces []net.Interface) (*zeroconf.Server, error) {
	host := "castplay-" + strings.ToLower(strings.ReplaceAll(r.cfg.DeviceID, ":", ""))
	var ips []string
	if r.cfg.IP != nil {
		ips = []string{r.cfg.IP.String()}
	}
	srv, err := zeroconf.RegisterProxy(r.cfg.Name, "_airplay._tcp", "local.", r.Port(), host, ips, r.txt(), ifaces)
	r.mdns = srv
	return srv, err
}

func (r *Receiver) txt() []string {
	flags, acl := r.home.statusFlags(), r.home.snapshot().ACL
	return []string{
		fmt.Sprintf("acl=%d", acl),
		"deviceid=" + r.cfg.DeviceID,
		"features=" + featureString(features),
		fmt.Sprintf("flags=%#x", flags),
		"gcgl=0",
		"gid=" + r.cfg.GroupID,
		"manufacturer=castplay",
		"model=" + r.cfg.Model,
		"name=" + r.cfg.Name,
		"protovers=1.1",
		"rsf=0x0",
		"serialNumber=" + r.cfg.DeviceID,
		"srcvers=" + sourceVersion,
		"pi=" + r.cfg.Identity.PairingID,
		"pk=" + hex.EncodeToString(r.cfg.Identity.PublicKey()),
		fmt.Sprintf("sf=%#x", flags),
		"ch=2",
		"cn=0,1,2",
	}
}

// featureString formats the 64-bit mask as AirPlay does: "0xLOW,0xHIGH".
func featureString(f uint64) string {
	if f>>32 == 0 {
		return fmt.Sprintf("%#x", f)
	}
	return fmt.Sprintf("%#x,%#x", f&0xffffffff, f>>32)
}
