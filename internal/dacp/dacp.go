// Package dacp sends remote-control commands back to an AirPlay sender over
// DACP, the HTTP interface iTunes/Music expose for remotes. castplay uses it
// to tell the sender about volume changes made on the Cast side, the way
// shairport-sync does (dacp.c).
package dacp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// Remote is one sender's DACP endpoint, identified by the DACP-ID and
// Active-Remote headers it sends with its RTSP requests.
type Remote struct {
	ID           string // DACP-ID, hex
	ActiveRemote string
	// Speaker is our machine number in the sender's speaker list: the
	// receiver's 6-byte device ID as an integer.
	Speaker uint64
	Log     *slog.Logger

	mu   sync.Mutex
	addr string // host:port, once resolved
}

// SpeakerNumber turns a MAC-style device ID ("AA:BB:CC:DD:EE:FF") into the
// machine number senders list it under.
func SpeakerNumber(deviceID string) uint64 {
	var n uint64
	for _, part := range strings.Split(deviceID, ":") {
		b, _ := strconv.ParseUint(part, 16, 8)
		n = n<<8 | b
	}
	return n
}

// resolve finds the sender's DACP service, advertised over mDNS as
// iTunes_Ctrl_<DACP-ID>._dacp._tcp.
func (r *Remote) resolve(ctx context.Context) (string, error) {
	r.mu.Lock()
	addr := r.addr
	r.mu.Unlock()
	if addr != "" {
		return addr, nil
	}
	res, err := zeroconf.NewResolver(nil)
	if err != nil {
		return "", err
	}
	entries := make(chan *zeroconf.ServiceEntry, 4)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := res.Lookup(ctx, "iTunes_Ctrl_"+r.ID, "_dacp._tcp", "local.", entries); err != nil {
		return "", err
	}
	for {
		select {
		case e := <-entries:
			var ip net.IP
			switch {
			case len(e.AddrIPv4) > 0:
				ip = e.AddrIPv4[0]
			case len(e.AddrIPv6) > 0:
				ip = e.AddrIPv6[0]
			default:
				continue
			}
			addr = net.JoinHostPort(ip.String(), strconv.Itoa(e.Port))
			r.mu.Lock()
			r.addr = addr
			r.mu.Unlock()
			return addr, nil
		case <-ctx.Done():
			return "", fmt.Errorf("dacp: iTunes_Ctrl_%s not found", r.ID)
		}
	}
}

var httpClient = &http.Client{Timeout: 3 * time.Second}

// command sends GET /ctrl-int/1/<cmd> and returns the (DMAP) reply body.
func (r *Remote) command(ctx context.Context, cmd string) ([]byte, error) {
	addr, err := r.resolve(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/ctrl-int/1/"+cmd, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Active-Remote", r.ActiveRemote)
	resp, err := httpClient.Do(req)
	if err != nil {
		// The sender may have moved ports; look it up again next time.
		r.mu.Lock()
		r.addr = ""
		r.mu.Unlock()
		return nil, fmt.Errorf("dacp: %s: %w", cmd, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return nil, fmt.Errorf("dacp: %s: %s", cmd, resp.Status)
	}
	r.Log.Debug("dacp", "cmd", cmd, "status", resp.StatusCode, "bytes", len(body))
	return body, err
}

type speaker struct {
	name   string
	id     uint64
	volume int // percent of the overall volume
	active bool
}

// SetVolume sets our speaker's volume to vo (0-100, absolute) on the
// sender. The sender's overall volume is the loudest active speaker's, and
// each speaker's volume is relative to it, so lowering us below another
// speaker must be done as a relative change (shairport-sync's
// dacp_set_volume).
func (r *Remote) SetVolume(ctx context.Context, vo int) error {
	vo = max(0, min(100, vo))
	body, err := r.command(ctx, "getproperty?properties=dmcp.volume")
	if err != nil {
		return err
	}
	overall, ok := dmapInt(body, "cmgt", "cmvo")
	if !ok {
		return errors.New("dacp: no dmcp.volume in reply")
	}
	body, err = r.command(ctx, "getspeakers")
	if err != nil {
		return err
	}
	highestOther := 0
	for _, sp := range parseSpeakers(body) {
		r.Log.Debug("dacp speaker", "name", sp.name, "id", fmt.Sprintf("%x", sp.id), "volume", sp.volume, "active", sp.active)
		if sp.active && sp.id != r.Speaker {
			highestOther = max(highestOther, sp.volume*overall/100)
		}
	}
	absolute := fmt.Sprintf("setproperty?include-speaker-id=%d&dmcp.volume=%d", r.Speaker, vo)
	if vo >= overall || highestOther == 0 || highestOther <= vo {
		_, err = r.command(ctx, absolute)
		return err
	}
	// Another speaker is louder than our new volume: pin the overall volume
	// to it, then set ours relative to that.
	if overall > highestOther {
		if _, err := r.command(ctx, fmt.Sprintf("setproperty?include-speaker-id=%d&dmcp.volume=%d", r.Speaker, highestOther)); err != nil {
			return err
		}
	}
	_, err = r.command(ctx, fmt.Sprintf("setproperty?speaker-id=%d&dmcp.volume=%d", r.Speaker, vo*100/highestOther))
	return err
}

// DMAP is a tree of tag(4) | length(4, BE) | value items.
func dmapItems(b []byte, f func(tag string, v []byte)) {
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if n > len(b)-8 {
			return
		}
		f(string(b[:4]), b[8:8+n])
		b = b[8+n:]
	}
}

func dmapUint(v []byte) uint64 {
	var n uint64
	for _, c := range v {
		n = n<<8 | uint64(c)
	}
	return n
}

// dmapInt finds container/tag and returns it as an integer.
func dmapInt(b []byte, container, tag string) (int, bool) {
	var out int
	found := false
	dmapItems(b, func(t string, v []byte) {
		if t == container {
			dmapItems(v, func(t string, v []byte) {
				if t == tag {
					out, found = int(int32(dmapUint(v))), true
				}
			})
		}
	})
	return out, found
}

func parseSpeakers(b []byte) []speaker {
	var out []speaker
	dmapItems(b, func(t string, v []byte) {
		if t != "casp" {
			return
		}
		dmapItems(v, func(t string, v []byte) {
			if t != "mdcl" {
				return
			}
			var sp speaker
			dmapItems(v, func(t string, v []byte) {
				switch t {
				case "minm":
					sp.name = string(v)
				case "msma":
					sp.id = dmapUint(v)
				case "cmvo":
					sp.volume = int(dmapUint(v))
				case "caia":
					sp.active = dmapUint(v) != 0
				}
			})
			out = append(out, sp)
		})
	})
	return out
}
