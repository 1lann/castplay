package airplay

import (
	"encoding/binary"
	"log/slog"
	"net"
	"testing"
	"time"
)

type anchorPlayer struct {
	Player
	anchors chan Anchor
}

func (p anchorPlayer) Anchor(a Anchor) { p.anchors <- a }

// TestRealtimeSync sends 0xD7 packets to a realtime stream's control port
// and checks only the first and a timeline jump become anchors, with the
// PTP time decoded and shifted by the fudge factor.
func TestRealtimeSync(t *testing.T) {
	ctrl, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := anchorPlayer{anchors: make(chan Anchor, 10)}
	st := &stream{typ: streamRealtime, format: Format{Codec: CodecALAC, SampleRate: 44100},
		player: p, owns: func() bool { return true }, ctrl: ctrl, done: make(chan struct{}),
		log: slog.New(slog.DiscardHandler)}
	st.resync.Store(true)
	go st.serveControl()
	defer st.close()

	c, err := net.DialUDP("udp", nil, ctrl.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	send := func(rtp uint32, ns uint64) {
		b := make([]byte, 28)
		b[0], b[1] = 0x80, 0xD7
		binary.BigEndian.PutUint32(b[4:], rtp)
		binary.BigEndian.PutUint64(b[8:], ns)
		binary.BigEndian.PutUint32(b[16:], rtp+77175)
		binary.BigEndian.PutUint64(b[20:], 0x0102030405060708)
		c.Write(b)
	}
	const base = 461_600_000_123
	send(1000, base)
	send(1000+44100, base+1e9)           // restates the anchor
	send(1000+88200, base+2e9+20e6)      // 20 ms jump
	send(1000+132300, base+3e9+20e6+1e3) // restates the new one

	want := []struct {
		rtp uint32
		ns  int64
	}{{1000, base + 250e6}, {1000 + 88200, base + 2e9 + 20e6 + 250e6}}
	for _, w := range want {
		select {
		case a := <-p.anchors:
			if a.RTPTime != w.rtp || a.NetworkTime.ClockID != 0x0102030405060708 {
				t.Errorf("anchor %+v, want rtp %d", a, w.rtp)
			}
			if d := a.NetworkTime.Nanos() - w.ns; d < -1 || d > 1 {
				t.Errorf("anchor time %d, want %d", a.NetworkTime.Nanos(), w.ns)
			}
		case <-time.After(time.Second):
			t.Fatal("missing anchor")
		}
	}
	select {
	case a := <-p.anchors:
		t.Errorf("unexpected anchor %+v", a)
	case <-time.After(100 * time.Millisecond):
	}
}
