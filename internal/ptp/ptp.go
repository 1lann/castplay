// Package ptp follows the PTP clocks of AirPlay 2 senders, the way nqptp
// (shairport-sync's companion) does: listen on UDP 319/320 for the clock
// master's Announce and two-step Sync/Follow_Up messages, and estimate the
// offset between its timeline and ours. It sends nothing except nqptp's
// "awakening" Announce pair, which makes a silent Apple clock start sending
// Sync/Follow_Up. There is no Delay_Req: the one-way delay on a LAN is
// small and is mostly absorbed by taking the best (largest) offset seen.
//
// One Follower serves every speaker in the process, since the ports are
// fixed.
package ptp

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	msgSync      = 0x0
	msgFollowUp  = 0x8
	msgAnnounce  = 0xb
	msgSignaling = 0xc

	headerLen   = 34
	announceLen = 64

	// window over which the maximum offset is taken. Network delay only ever
	// makes an offset sample smaller, so the max is the least-delayed sample;
	// a short window still tracks drift between the clocks.
	window = 4 * time.Second
)

type sample struct {
	at     time.Time
	offset int64 // master ns - local ns
}

type clock struct {
	id          uint64
	grandmaster uint64
	ip          net.IP
	prio1       byte
	prio2       byte

	samples      []sample
	lastFollowUp time.Time
	lastAwaken   time.Time
	announces    int
}

// Follower tracks every PTP clock that talks to us.
type Follower struct {
	log  *slog.Logger
	self uint64
	ev   *net.UDPConn // 319
	gen  *net.UDPConn // 320
	base time.Time    // local timeline origin (monotonic)

	mu     sync.Mutex
	clocks map[uint64]*clock
	peers  map[string]bool
}

// Listen binds UDP 319 and 320 on ip (nil = all addresses). selfID is our
// clock identity, used only in awakening Announces.
func Listen(ip net.IP, selfID uint64, log *slog.Logger) (*Follower, error) {
	ev, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 319})
	if err != nil {
		return nil, fmt.Errorf("ptp: bind 319 (needs root or CAP_NET_BIND_SERVICE on Linux): %w", err)
	}
	gen, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 320})
	if err != nil {
		ev.Close()
		return nil, fmt.Errorf("ptp: bind 320: %w", err)
	}
	f := &Follower{log: log, self: selfID, ev: ev, gen: gen, base: time.Now(),
		clocks: map[uint64]*clock{}, peers: map[string]bool{}}
	go f.read(ev)
	go f.read(gen)
	return f, nil
}

func (f *Follower) Close() {
	f.ev.Close()
	f.gen.Close()
}

// SetPeers records the session's timing peers. Messages from other hosts
// are still accepted; peers only matter for awakening.
func (f *Follower) SetPeers(addrs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			f.peers[ip.String()] = true
		}
	}
}

func (f *Follower) local(t time.Time) int64 { return int64(t.Sub(f.base)) }

func (f *Follower) read(c *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFromUDP(buf)
		now := time.Now()
		if err != nil {
			return
		}
		if n < headerLen || buf[1]&0x0f != 2 {
			continue
		}
		f.handle(buf[:n], from.IP, now)
	}
}

func (f *Follower) handle(b []byte, from net.IP, now time.Time) {
	typ := b[0] & 0x0f
	id := binary.BigEndian.Uint64(b[20:28])
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.clocks[id]
	switch typ {
	case msgAnnounce:
		if len(b) < announceLen {
			return
		}
		if c == nil {
			c = &clock{id: id}
			f.clocks[id] = c
			f.log.Info("ptp clock seen", "clock", fmt.Sprintf("%016x", id), "ip", from)
		}
		c.ip = from
		c.prio1, c.prio2 = b[47], b[52]
		gm := binary.BigEndian.Uint64(b[53:61])
		if gm != c.grandmaster {
			f.log.Info("ptp grandmaster", "clock", fmt.Sprintf("%016x", id), "grandmaster", fmt.Sprintf("%016x", gm))
			c.grandmaster = gm
			c.samples = nil
		}
		c.announces++
		// A clock that announces but sends no Follow_Ups is asleep; nqptp
		// wakes it with a better-then-worse Announce pair.
		if now.Sub(c.lastFollowUp) > 2*time.Second && c.announces >= 3 && now.Sub(c.lastAwaken) > 5*time.Second {
			c.lastAwaken = now
			go f.awaken(from, c.prio1, c.prio2)
		}
	case msgFollowUp:
		if c == nil || len(b) < headerLen+10 {
			return // need an Announce first to know the grandmaster
		}
		secs := uint64(binary.BigEndian.Uint16(b[34:36]))<<32 | uint64(binary.BigEndian.Uint32(b[36:40]))
		nanos := uint64(binary.BigEndian.Uint32(b[40:44]))
		correction := int64(binary.BigEndian.Uint64(b[8:16])) / 65536
		origin := int64(secs*1e9+nanos) + correction
		c.lastFollowUp = now
		c.samples = append(c.samples, sample{at: now, offset: origin - f.local(now)})
		cut := 0
		for cut < len(c.samples) && now.Sub(c.samples[cut].at) > window {
			cut++
		}
		c.samples = c.samples[cut:]
	}
}

// awaken sends nqptp's Announce pair: first claiming a slightly better
// priority than the clock, then 150 ms later a slightly worse one.
func (f *Follower) awaken(to net.IP, prio1, prio2 byte) {
	f.log.Debug("ptp awakening clock", "ip", to)
	msg := make([]byte, announceLen)
	msg[0] = 0x10 | msgAnnounce
	msg[1] = 0x02
	binary.BigEndian.PutUint16(msg[2:], announceLen)
	binary.BigEndian.PutUint16(msg[6:], 0x0408)
	binary.BigEndian.PutUint64(msg[20:], f.self)
	binary.BigEndian.PutUint16(msg[28:], 32776)
	msg[32], msg[33] = 0x05, 0xFE
	binary.BigEndian.PutUint16(msg[44:], 37) // UTC offset
	binary.BigEndian.PutUint32(msg[48:], 0xf8fe436a)
	msg[52] = prio2
	binary.BigEndian.PutUint64(msg[53:], f.self)
	msg[63] = 160 // internal oscillator
	dst := &net.UDPAddr{IP: to, Port: 320}
	msg[47] = max(prio1, 3) - 1
	f.gen.WriteToUDP(msg, dst)
	time.Sleep(150 * time.Millisecond)
	msg[47] = min(prio1, 253) + 1
	f.gen.WriteToUDP(msg, dst)
}

// Offset returns master-minus-local nanoseconds for the given timeline (a
// grandmaster or clock ID, as in SETRATEANCHORTIME's networkTimeTimelineID),
// or false if that clock isn't synchronised yet.
func (f *Follower) offset(timeline uint64) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *clock
	for _, c := range f.clocks {
		if (c.grandmaster == timeline || c.id == timeline) && len(c.samples) > 0 {
			if best == nil || c.lastFollowUp.After(best.lastFollowUp) {
				best = c
			}
		}
	}
	if best == nil || time.Since(best.lastFollowUp) > 3*time.Second {
		return 0, false
	}
	off := best.samples[0].offset
	for _, s := range best.samples[1:] {
		off = max(off, s.offset)
	}
	return off, true
}

// LocalTime converts a time on a PTP timeline (nanoseconds) to local time.
func (f *Follower) LocalTime(timeline uint64, ptpNanos int64) (time.Time, bool) {
	off, ok := f.offset(timeline)
	if !ok {
		return time.Time{}, false
	}
	return f.base.Add(time.Duration(ptpNanos - off)), true
}

// Synced reports whether any clock is delivering Follow_Ups.
func (f *Follower) Synced() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clocks {
		if time.Since(c.lastFollowUp) < 3*time.Second {
			return true
		}
	}
	return false
}
