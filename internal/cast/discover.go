package cast

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

// Device is a Cast endpoint found over mDNS (_googlecast._tcp). Groups are
// advertised the same way, with model "Google Cast Group" and a port other
// than 8009.
type Device struct {
	Name  string // friendly name (TXT fn)
	Model string // TXT md
	ID    string // TXT id
	Addr  string // host:port
}

func (d Device) IsGroup() bool { return d.Model == "Google Cast Group" }

func (d Device) String() string {
	kind := "device"
	if d.IsGroup() {
		kind = "group"
	}
	return fmt.Sprintf("%-28s %-22s %-6s %s", d.Name, d.Model, kind, d.Addr)
}

// Discover browses for Cast devices until ctx is done and returns them sorted
// by name.
func Discover(ctx context.Context) ([]Device, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry)
	found := map[string]Device{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range entries {
			d := Device{Name: e.Instance}
			for _, kv := range e.Text {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "fn":
					d.Name = v
				case "md":
					d.Model = v
				case "id":
					d.ID = v
				}
			}
			var ip net.IP
			if len(e.AddrIPv4) > 0 {
				ip = e.AddrIPv4[0]
			} else if len(e.AddrIPv6) > 0 {
				ip = e.AddrIPv6[0]
			} else {
				continue
			}
			d.Addr = net.JoinHostPort(ip.String(), strconv.Itoa(e.Port))
			key := d.ID
			if key == "" {
				key = d.Addr
			}
			found[key] = d
		}
	}()
	if err := resolver.Browse(ctx, "_googlecast._tcp", "local.", entries); err != nil {
		return nil, err
	}
	<-ctx.Done()
	<-done
	out := make([]Device, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
