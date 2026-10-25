package airplay

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	streamRealtime = 96
	streamBuffered = 103

	// audioBufferSize bounds how far ahead a buffered sender may run (bytes
	// of encoded audio). airplay2-receiver uses the same 8 MiB.
	audioBufferSize = 8 << 20
)

// stream is one audio stream: a data channel (TCP for buffered, UDP for
// realtime) plus a UDP control channel, which for realtime streams carries
// the timing that buffered streams get from SETRATEANCHORTIME, and resent
// packets.
type stream struct {
	id     uint64
	typ    int
	format Format
	aead   cipher.AEAD
	player Player
	owns   func() bool
	log    *slog.Logger

	// Realtime anchoring: the last anchor handed to the player, and whether
	// the next sync packet must be handed on regardless (after a flush).
	lastAnchor Anchor
	resync     atomic.Bool
	// The sender's control address (where its sync packets come from), for
	// retransmit requests.
	senderCtrl atomic.Pointer[net.UDPAddr]

	tcp  net.Listener
	udp  *net.UDPConn
	ctrl *net.UDPConn

	once sync.Once
	done chan struct{}
}

func (s *session) newStream(desc map[string]any) (*stream, error) {
	typ, _ := pInt(desc, "type")
	if typ != streamBuffered && typ != streamRealtime {
		return nil, fmt.Errorf("unsupported stream type %d", typ)
	}
	af, _ := pUint(desc, "audioFormat")
	spf, _ := pInt(desc, "spf")
	format, err := parseFormat(af, int(spf))
	if err != nil {
		return nil, err
	}
	key := pBytes(desc, "shk")
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("stream key is %d bytes, want 32 (unencrypted or FairPlay streams are not supported)", len(key))
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	s.r.mu.Lock()
	s.r.nextID++
	id := s.r.nextID
	s.r.mu.Unlock()

	st := &stream{id: id, typ: int(typ), format: format, aead: aead, player: s.r.player, owns: s.owns,
		log: s.log.With("stream", id), done: make(chan struct{})}
	st.resync.Store(true)
	if st.ctrl, err = net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
		return nil, err
	}
	go st.serveControl()
	if st.typ == streamBuffered {
		if st.tcp, err = net.Listen("tcp", ":0"); err != nil {
			st.close()
			return nil, err
		}
		go st.serveBuffered()
	} else {
		if st.udp, err = net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
			st.close()
			return nil, err
		}
		go st.serveRealtime()
	}
	return st, nil
}

func (st *stream) dataPort() int {
	if st.tcp != nil {
		return st.tcp.Addr().(*net.TCPAddr).Port
	}
	return st.udp.LocalAddr().(*net.UDPAddr).Port
}

func (st *stream) descriptor() map[string]any {
	return map[string]any{
		"type":            st.typ,
		"dataPort":        st.dataPort(),
		"controlPort":     st.ctrl.LocalAddr().(*net.UDPAddr).Port,
		"audioBufferSize": audioBufferSize,
		"streamID":        st.id,
	}
}

func (st *stream) close() {
	st.once.Do(func() {
		close(st.done)
		if st.tcp != nil {
			st.tcp.Close()
		}
		if st.udp != nil {
			st.udp.Close()
		}
		if st.ctrl != nil {
			st.ctrl.Close()
		}
	})
}

// serveBuffered reads the buffered stream: TCP, each RTP packet prefixed by
// its total length (including the 2-byte prefix) as a big-endian uint16.
func (st *stream) serveBuffered() {
	conn, err := st.tcp.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	go func() {
		<-st.done
		conn.Close()
	}()
	var hdr [2]byte
	buf := make([]byte, 65536)
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				st.log.Debug("buffered stream ended", "err", err)
			}
			return
		}
		n := int(binary.BigEndian.Uint16(hdr[:]))
		if n < 2 {
			continue
		}
		pkt := buf[:n-2]
		if _, err := io.ReadFull(conn, pkt); err != nil {
			return
		}
		st.deliver(pkt, true)
	}
}

// serveRealtime reads realtime RTP packets from UDP, asking the sender to
// resend any it skips; there are 1.5-2 s for the copies to arrive.
func (st *stream) serveRealtime() {
	buf := make([]byte, 65536)
	var next uint16
	started := false
	for {
		n, _, err := st.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pt := buf[1] & 0x7f
		if n < 12 || pt != 96 { // 97 (redundancy) is not advertised
			continue
		}
		seq := binary.BigEndian.Uint16(buf[2:4])
		if gap := seq - next; started && gap != 0 && gap < 0x8000 {
			if gap <= 256 { // a larger jump is a skip, not loss
				st.requestResend(next, gap)
			}
		}
		if !started || seq-next < 0x8000 {
			next, started = seq+1, true
		}
		st.deliver(buf[:n], false)
	}
}

// requestResend asks for count packets from seq first: 0x80 0xD5, our
// request number, first, count (shairport-sync's rtp_request_resend). The
// copies come back on the control channel as 0xD6.
func (st *stream) requestResend(first, count uint16) {
	addr := st.senderCtrl.Load()
	if addr == nil {
		return
	}
	req := []byte{0x80, 0xD5, 0, 1, byte(first >> 8), byte(first), byte(count >> 8), byte(count)}
	if _, err := st.ctrl.WriteToUDP(req, addr); err != nil {
		st.log.Debug("resend request", "err", err)
		return
	}
	st.log.Debug("requested resend", "first", first, "count", count)
}

// deliver decrypts one RTP packet. Layout: 12-byte RTP header | ciphertext |
// 16-byte tag | 8-byte nonce. ChaCha20-Poly1305 with the stream key, the
// nonce left-padded to 12 bytes and the timestamp+SSRC as AAD.
func (st *stream) deliver(pkt []byte, buffered bool) {
	if len(pkt) < 12+16+8 {
		return
	}
	var seq uint32
	if buffered {
		seq = uint32(pkt[1])<<16 | uint32(pkt[2])<<8 | uint32(pkt[3])
	} else {
		seq = uint32(binary.BigEndian.Uint16(pkt[2:4]))
	}
	ts := binary.BigEndian.Uint32(pkt[4:8])
	nonce := make([]byte, 12)
	copy(nonce[4:], pkt[len(pkt)-8:])
	sealed := pkt[12 : len(pkt)-8]
	plain, err := st.aead.Open(nil, nonce, sealed, pkt[4:12])
	if err != nil {
		st.log.Debug("dropping undecryptable packet", "seq", seq)
		return
	}
	st.player.Packet(Packet{Seq: seq, Timestamp: ts, Payload: plain, Received: time.Now()})
}

// realtimeSyncFudge is how far before the packet's PTP time its "now" RTP
// frame actually plays, in seconds. shairport-sync found 11025 frames at
// 44.1 kHz "seems to bring everything into sync".
const realtimeSyncFudge = 0.25

// serveControl reads the control channel. Only the PTP time announcement
// (0xD7, about once a second) matters: 28 bytes of RTP header byte, type,
// length; then the RTP frame being sent now (4-7), the sender's PTP time in
// ns (8-15), the frame due to play then (16-19, about 1.75 s later), and the
// PTP clock ID (20-27). Retransmitted packets come back as 0xD6.
func (st *stream) serveControl() {
	buf := make([]byte, 2048)
	for {
		n, from, err := st.ctrl.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if st.typ != streamRealtime {
			continue
		}
		if buf[1] == 0xD6 && n > 4+12 {
			// A resent packet: the original RTP packet after 4 bytes.
			st.deliver(buf[4:n], false)
			continue
		}
		if n < 28 || buf[1] != 0xD7 {
			continue
		}
		st.senderCtrl.Store(from)
		now := binary.BigEndian.Uint32(buf[4:8])
		ns := binary.BigEndian.Uint64(buf[8:16])
		ns += uint64(realtimeSyncFudge * 1e9)
		a := Anchor{Rate: 1, RTPTime: now, Received: time.Now(), NetworkTime: NetworkTime{
			Secs: ns / 1e9, Frac: (ns % 1e9) << 32 / 1e9 << 32, ClockID: binary.BigEndian.Uint64(buf[20:28])}}
		if st.anchorChanged(a) && st.owns() {
			st.log.Info("realtime anchor", "rtpTime", a.RTPTime, "latency", binary.BigEndian.Uint32(buf[16:20])-now)
			st.player.Anchor(a)
		}
	}
}

// anchorChanged reports whether a sync packet's anchor needs handing on:
// the first one, the first after a flush, or one that moved the timeline by
// more than 5 ms (a clock change or sender skip). Most only restate it.
func (st *stream) anchorChanged(a Anchor) bool {
	prev := st.lastAnchor
	st.lastAnchor = a
	if st.resync.Swap(false) || prev.Rate == 0 || prev.NetworkTime.ClockID != a.NetworkTime.ClockID {
		return true
	}
	rate := float64(max(st.format.SampleRate, 1))
	expect := prev.NetworkTime.Nanos() + int64(float64(int32(a.RTPTime-prev.RTPTime))/rate*1e9)
	drift := time.Duration(a.NetworkTime.Nanos() - expect)
	return drift > 5*time.Millisecond || drift < -5*time.Millisecond
}
