// Package airplay is an AirPlay 2 audio receiver: mDNS advertisement, the
// RTSP control channel with HomeKit transient pairing, and buffered (TCP) or
// realtime (UDP) audio streams decrypted and handed to a Player.
//
// Protocol details follow openairplay/airplay2-receiver (Python) and
// shairport-sync. Only what an audio speaker needs is implemented: no video,
// screen mirroring, FairPlay or MFi.
package airplay

import (
	"castplay/internal/dacp"

	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"castplay/internal/airplay/hap"

	"github.com/grandcat/zeroconf"
)

const sourceVersion = "366.0"

// Feature bits advertised (airplay2-receiver's GetDefaultAirplayTwoFlags):
// AirPlay audio, "FairPlay" (required by iOS Music even though it isn't
// used with HomeKit pairing), metadata, PCM/ALAC/AAC, unencrypted-audio,
// unified advertising, buffered audio, PTP clock, HomeKit/peer management and
// transient pairing.
const features uint64 = 1<<9 | 1<<14 | 1<<16 | 1<<17 | 1<<18 | 1<<19 | 1<<20 | 1<<22 |
	1<<30 | 1<<40 | 1<<41 | 1<<46 | 1<<47 | 1<<48

// Player receives everything a sender does to one speaker. Calls for a
// stream are serialised, but Packet runs on the stream's own goroutine.
type Player interface {
	// StreamStarted is called when SETUP creates an audio stream.
	StreamStarted(s StreamInfo)
	// Packet delivers one decrypted audio frame in arrival order.
	Packet(p Packet)
	// Anchor delivers SETRATEANCHORTIME: start (rate 1) or pause (rate 0).
	Anchor(a Anchor)
	// Flush discards buffered audio (track skip, seek).
	Flush(f Flush)
	// Volume is the sender's volume in dB: -30 (min) .. 0, or -144 for mute.
	Volume(db float64)
	// TimingPeers lists the PTP peers of the session (SETUP/SETPEERS).
	TimingPeers(addrs []string)
	// StreamStopped is called on TEARDOWN or when the sender goes away.
	StreamStopped()
}

type StreamInfo struct {
	ID             uint64
	Type           int // 96 realtime (UDP), 103 buffered (TCP)
	Format         Format
	Sender         string // sender's name or address
	TimingProtocol string // "PTP" or "NTP"
	// Events is the session's reverse channel, for sending volume changes
	// and play/pause back to the sender.
	Events *EventChannel
	// Remote is the sender's DACP endpoint, the older way to do the same;
	// nil if the sender didn't offer one.
	Remote *dacp.Remote
}

type Packet struct {
	Seq       uint32 // 24-bit for buffered, 16-bit for realtime streams
	Timestamp uint32 // RTP timestamp at the stream's sample rate
	Payload   []byte
	Received  time.Time
}

// Anchor ties an RTP timestamp to a time on the sender's PTP timeline.
type Anchor struct {
	Rate        float64
	RTPTime     uint32
	NetworkTime NetworkTime // zero if absent
	Received    time.Time
}

// NetworkTime is a point on a PTP timeline: seconds plus a 64-bit fraction.
type NetworkTime struct {
	Secs, Frac uint64
	ClockID    uint64
}

func (t NetworkTime) IsZero() bool { return t.Secs == 0 && t.Frac == 0 }

// Nanos is the time in nanoseconds on its timeline.
func (t NetworkTime) Nanos() int64 {
	return int64(t.Secs)*1e9 + int64((t.Frac>>32)*1e9>>32)
}

type Flush struct {
	FromSeq, UntilSeq uint32
	FromTS, UntilTS   uint32
	HasFrom           bool
	All               bool // drop everything (realtime FLUSH)
}

// Config describes one advertised AirPlay speaker.
type Config struct {
	Name          string
	DeviceID      string // MAC-style, e.g. "AA:BB:CC:DD:EE:FF"
	GroupID       string // UUID
	Identity      hap.Identity
	Model         string
	Port          int           // RTSP port (0 = any)
	IP            net.IP        // address advertised and used for timing peers
	OutputLatency time.Duration // reported to senders
	HomeFile      string        // where Home pairings persist ("" = memory only)
	// RemoteControl, if set, advertises the remote control relay status
	// flag and serves the MediaRemote channels senders then open.
	RemoteControl RemoteControl
	Log           *slog.Logger
}

type Receiver struct {
	cfg    Config
	player Player
	log    *slog.Logger
	ln     net.Listener
	home   *home
	mdns   *zeroconf.Server

	mu     sync.Mutex
	active *session // the session that owns the player
	nextID uint64
}

func NewReceiver(cfg Config, p Player) *Receiver {
	return &Receiver{cfg: cfg, player: p, log: cfg.Log.With("speaker", cfg.Name)}
}

// Listen opens the RTSP port; Port() is valid afterwards.
func (r *Receiver) Listen() error {
	if r.home == nil {
		h, err := loadHome(r.cfg.HomeFile)
		if err != nil {
			return fmt.Errorf("load Home pairings: %w", err)
		}
		r.home = h
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.cfg.Port))
	if err != nil {
		return err
	}
	r.ln = ln
	return nil
}

func (r *Receiver) Port() int { return r.ln.Addr().(*net.TCPAddr).Port }

// Serve accepts sender connections until ctx is done.
func (r *Receiver) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		r.ln.Close()
	}()
	for {
		c, err := r.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go r.handleConn(ctx, c)
	}
}

// session is one control connection from a sender.
type session struct {
	r    *Receiver
	log  *slog.Logger
	raw  net.Conn
	br   *bufio.Reader
	w    io.Writer
	pair *hap.Session

	encrypted bool
	sender    string
	remote    *dacp.Remote
	timing    string
	events    *EventChannel
	streams   map[uint64]*stream
	remotes   map[uint64]*RemoteChannel
	closed    chan struct{}
}

func (r *Receiver) handleConn(ctx context.Context, c net.Conn) {
	s := &session{
		r: r, raw: c, br: bufio.NewReader(c), w: c,
		pair:    hap.NewSession(r.cfg.Identity, ""),
		streams: map[uint64]*stream{},
		remotes: map[uint64]*RemoteChannel{},
		closed:  make(chan struct{}),
		log:     r.log.With("peer", c.RemoteAddr().String()),
	}
	s.log.Debug("control connection opened")
	defer s.close()
	for {
		req, err := readRequest(s.br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Debug("control connection closed", "err", err)
			}
			return
		}
		if name := req.Header.Get("X-Apple-Client-Name"); name != "" {
			s.sender = name
		}
		resp, err := s.handle(req)
		if err != nil {
			s.log.Warn("request failed", "method", req.Method, "path", req.Path, "err", err)
			if resp == nil {
				resp = &response{Status: 500}
			}
		}
		if req.Method != "POST" || (req.Path != "/feedback" && req.Path != "/command") {
			s.log.Debug("rtsp", "method", req.Method, "path", req.Path, "status", resp.Status, "headers", req.Header)
		}
		if err := resp.write(s.w, req); err != nil {
			return
		}
		// Pairing finished: everything after this response is encrypted.
		if !s.encrypted && s.pair.SharedKey != nil {
			hc, err := hap.NewConn(s.raw, s.pair.SharedKey)
			if err != nil {
				s.log.Warn("encrypt control channel", "err", err)
				return
			}
			s.br, s.w, s.encrypted = bufio.NewReader(hc), hc, true
			s.log.Info("paired", "controller", s.pair.ControllerID)
		}
	}
}

func (s *session) handle(req *request) (*response, error) {
	if id, ar := req.Header.Get("DACP-ID"), req.Header.Get("Active-Remote"); id != "" && ar != "" &&
		(s.remote == nil || s.remote.ID != id || s.remote.ActiveRemote != ar) {
		s.remote = &dacp.Remote{ID: id, ActiveRemote: ar, Speaker: dacp.SpeakerNumber(s.r.cfg.DeviceID), Log: s.log}
		s.log.Debug("sender remote control", "dacpID", id)
	}
	switch req.Method {
	case "GET":
		if req.Path == "/info" {
			return plistResponse(s.r.info())
		}
	case "OPTIONS":
		return &response{Status: 200, Header: map[string]string{"Public": "ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, FLUSHBUFFERED, TEARDOWN, OPTIONS, POST, GET, PUT, GET_PARAMETER, SET_PARAMETER, SETRATEANCHORTIME, SETPEERS, SETPEERSX"}}, nil
	case "POST":
		switch req.Path {
		case "/pair-setup":
			body, err := s.pair.PairSetup(req.Body)
			return &response{Status: 200, ContentType: "application/octet-stream", Body: body}, err
		case "/pair-verify":
			body, err := s.pair.PairVerify(req.Body)
			return &response{Status: 200, ContentType: "application/octet-stream", Body: body}, err
		case "/fp-setup":
			body, err := fairplaySetup(req.Body)
			if err != nil {
				return &response{Status: 400}, err
			}
			return &response{Status: 200, ContentType: "application/octet-stream", Body: body}, nil
		case "/pair-add", "/pair-remove", "/pair-list":
			body, err := hap.HandlePairings(s.r.home, req.Body)
			if req.Path != "/pair-list" {
				s.r.homeChanged(req.Path)
			}
			return &response{Status: 200, ContentType: "application/octet-stream", Body: body}, err
		case "/configure":
			return s.configure(req)
		case "/command":
			if s.r.cfg.RemoteControl != nil {
				s.remoteCommand(req)
			}
			return &response{Status: 200}, nil
		case "/feedback", "/audioMode", "/pair-pin-start":
			return &response{Status: 200}, nil
		}
	case "SETUP":
		return s.setup(req)
	case "RECORD":
		return &response{Status: 200, Header: map[string]string{"Audio-Latency": "0"}}, nil
	case "SETRATEANCHORTIME":
		return s.setRateAnchorTime(req)
	case "FLUSHBUFFERED":
		return s.flushBuffered(req)
	case "FLUSH":
		return s.flush()
	case "SET_PARAMETER":
		return s.setParameter(req)
	case "GET_PARAMETER":
		if strings.Contains(string(req.Body), "volume") {
			return &response{Status: 200, ContentType: "text/parameters", Body: []byte("volume: 0.000000\r\n")}, nil
		}
		return &response{Status: 200}, nil
	case "SETPEERS", "SETPEERSX":
		return s.setPeers(req)
	case "TEARDOWN":
		return s.teardown(req)
	case "SETMAGICCOOKIE":
		return &response{Status: 200}, nil
	}
	return &response{Status: 404}, fmt.Errorf("unhandled %s %s", req.Method, req.Path)
}

// info is the GET /info plist: device capabilities and identity.
func (r *Receiver) info() map[string]any {
	return map[string]any{
		"audioLatencies": []any{map[string]any{
			"inputLatencyMicros":  0,
			"outputLatencyMicros": r.cfg.OutputLatency.Microseconds(),
		}},
		"deviceID":                 r.cfg.DeviceID,
		"features":                 features,
		"keepAliveLowPower":        true,
		"keepAliveSendStatsAsBody": true,
		"manufacturer":             "castplay",
		"model":                    r.cfg.Model,
		"name":                     r.cfg.Name,
		"nameIsFactoryDefault":     false,
		"pi":                       r.cfg.Identity.PairingID,
		"pk":                       []byte(r.cfg.Identity.PublicKey()),
		"protocolVersion":          "1.1",
		"sdk":                      "AirPlay;2.0.2",
		"sourceVersion":            sourceVersion,
		"statusFlags":              r.statusFlags(),
	}
}

// configure handles /configure, which Home sends after /pair-add with the
// speaker's Home settings. The reply confirms them along with the
// accessory's pairing identity.
func (s *session) configure(req *request) (*response, error) {
	body, err := req.plistBody()
	if err != nil {
		return &response{Status: 400}, err
	}
	cd, _ := body["ConfigurationDictionary"].(map[string]any)
	if err := s.r.home.configure(cd); err != nil {
		return &response{Status: 500}, err
	}
	s.r.homeChanged("/configure")
	st := s.r.home.snapshot()
	name := pString(cd, "Device_Name")
	if name == "" {
		name = s.r.cfg.Name
	}
	out := map[string]any{
		"Identifier":               s.r.cfg.Identity.PairingID,
		"PublicKey":                []byte(s.r.cfg.Identity.PublicKey()),
		"Device_Name":              name,
		"Enable_HK_Access_Control": st.AccessControl,
		"Access_Control_Level":     st.ACL,
	}
	if st.Password != "" {
		out["Password"] = st.Password
	}
	return plistResponse(out)
}

func (r *Receiver) statusFlags() int {
	f := r.home.statusFlags()
	if r.cfg.RemoteControl != nil {
		f |= statusRemoteControlRelay
	}
	return f
}

// homeChanged logs a Home pairing change and re-advertises the status flags.
func (r *Receiver) homeChanged(what string) {
	st := r.home.snapshot()
	r.log.Info("home pairing changed", "request", what, "controllers", len(st.Controllers),
		"accessControl", st.AccessControl, "acl", st.ACL)
	if r.mdns != nil {
		r.mdns.SetText(r.txt())
	}
}

func (s *session) setup(req *request) (*response, error) {
	body, err := req.plistBody()
	if err != nil {
		return &response{Status: 400}, err
	}
	rawStreams, hasStreams := body["streams"].([]any)
	if !hasStreams {
		// Phase 1: session-wide setup (timing, events).
		s.timing = pString(body, "timingProtocol")
		if peers := peerAddresses(body["timingPeerInfo"]); len(peers) > 0 {
			s.r.player.TimingPeers(peers)
		}
		if s.events == nil {
			if s.events, err = newEventChannel(s); err != nil {
				return nil, err
			}
		}
		s.log.Info("session setup", "sender", s.sender, "timing", s.timing,
			"client", req.Header.Get("User-Agent"), "remoteControl", s.remote != nil)
		return plistResponse(map[string]any{
			"eventPort":  s.events.port(),
			"timingPort": 0,
			"timingPeerInfo": map[string]any{
				"Addresses": []any{s.r.cfg.IP.String()},
				"ID":        s.r.cfg.DeviceID,
			},
		})
	}

	// Phase 2: audio streams. This sender now owns the speaker.
	var out []any
	tookOver := false
	for _, rs := range rawStreams {
		desc, _ := rs.(map[string]any)
		if typ, _ := pInt(desc, "type"); typ == streamRemoteControl && s.r.cfg.RemoteControl != nil {
			out = append(out, s.openRemote(desc))
			continue
		}
		if !tookOver {
			s.r.takeOver(s)
			tookOver = true
		}
		st, err := s.newStream(desc)
		if err != nil {
			return &response{Status: 400}, err
		}
		s.streams[st.id] = st
		out = append(out, st.descriptor())
		s.r.player.StreamStarted(StreamInfo{ID: st.id, Type: st.typ, Format: st.format, Sender: s.sender, TimingProtocol: s.timing, Events: s.events, Remote: s.remote})
		s.log.Info("stream setup", "type", st.typ, "format", st.format.String(), "dataPort", st.dataPort(), "id", st.id)
	}
	return plistResponse(map[string]any{"streams": out})
}

func (s *session) setRateAnchorTime(req *request) (*response, error) {
	body, err := req.plistBody()
	if err != nil {
		return &response{Status: 400}, err
	}
	a := Anchor{Received: time.Now()}
	a.Rate, _ = pFloat(body, "rate")
	rtp, _ := pUint(body, "rtpTime")
	a.RTPTime = uint32(rtp)
	a.NetworkTime.Secs, _ = pUint(body, "networkTimeSecs")
	a.NetworkTime.Frac, _ = pUint(body, "networkTimeFrac")
	a.NetworkTime.ClockID, _ = pUint(body, "networkTimeTimelineID")
	if s.owns() {
		s.r.player.Anchor(a)
	}
	s.log.Debug("anchor", "rate", a.Rate, "rtpTime", a.RTPTime, "networkTime", a.NetworkTime)
	return &response{Status: 200}, nil
}

func (s *session) flushBuffered(req *request) (*response, error) {
	body, err := req.plistBody()
	if err != nil {
		return &response{Status: 400}, err
	}
	var f Flush
	if v, ok := pUint(body, "flushFromSeq"); ok {
		f.FromSeq, f.HasFrom = uint32(v), true
		v, _ = pUint(body, "flushFromTS")
		f.FromTS = uint32(v)
	}
	v, _ := pUint(body, "flushUntilSeq")
	f.UntilSeq = uint32(v)
	v, _ = pUint(body, "flushUntilTS")
	f.UntilTS = uint32(v)
	if s.owns() {
		s.r.player.Flush(f)
	}
	s.log.Debug("flush", "from", f.FromSeq, "until", f.UntilSeq, "hasFrom", f.HasFrom)
	return &response{Status: 200}, nil
}

// flush handles a realtime stream's FLUSH (pause or skip): everything
// queued is stale, and playback resumes on the next sync packet.
func (s *session) flush() (*response, error) {
	for _, st := range s.streams {
		st.resync.Store(true)
	}
	if s.owns() {
		s.r.player.Flush(Flush{All: true})
	}
	s.log.Debug("flush (realtime)")
	return &response{Status: 200}, nil
}

func (s *session) setParameter(req *request) (*response, error) {
	if strings.HasPrefix(req.Header.Get("Content-Type"), "text/parameters") {
		for _, line := range strings.Split(string(req.Body), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok && strings.TrimSpace(k) == "volume" {
				db, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err == nil && s.owns() {
					s.r.player.Volume(db)
				}
			}
		}
	}
	return &response{Status: 200}, nil
}

func (s *session) setPeers(req *request) (*response, error) {
	body := req.Body
	var addrs []string
	if req.Method == "SETPEERS" {
		var list []any
		if _, err := plistUnmarshal(body, &list); err == nil {
			for _, a := range list {
				if s, ok := a.(string); ok {
					addrs = append(addrs, s)
				}
			}
		}
	} else {
		var list []any
		if _, err := plistUnmarshal(body, &list); err == nil {
			for _, p := range list {
				addrs = append(addrs, peerAddresses(p)...)
			}
		}
	}
	if len(addrs) > 0 && s.owns() {
		s.r.player.TimingPeers(addrs)
	}
	s.log.Debug("timing peers", "method", req.Method, "addrs", addrs)
	return &response{Status: 200}, nil
}

func (s *session) teardown(req *request) (*response, error) {
	body, _ := req.plistBody()
	if rs, ok := body["streams"].([]any); ok && len(rs) > 0 {
		audio := false
		for _, x := range rs {
			d, _ := x.(map[string]any)
			id, _ := pUint(d, "streamID")
			if st := s.streams[id]; st != nil {
				st.close()
				delete(s.streams, id)
				audio = true
			}
			s.closeRemote(id)
		}
		if audio && len(s.streams) == 0 && s.owns() {
			s.r.player.StreamStopped()
		}
		return &response{Status: 200}, nil
	}
	s.stopStreams()
	s.closeRemotes()
	return &response{Status: 200}, nil
}

func peerAddresses(v any) []string {
	m, _ := v.(map[string]any)
	list, _ := m["Addresses"].([]any)
	var out []string
	for _, a := range list {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// takeOver makes s the session that drives the player, stopping any other.
func (r *Receiver) takeOver(s *session) {
	r.mu.Lock()
	prev := r.active
	r.active = s
	r.mu.Unlock()
	if prev != nil && prev != s {
		r.log.Info("new sender took over", "from", prev.sender, "to", s.sender)
		prev.raw.Close()
	}
}

func (s *session) owns() bool {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	return s.r.active == s
}

func (s *session) stopStreams() {
	for id, st := range s.streams {
		st.close()
		delete(s.streams, id)
	}
	s.r.mu.Lock()
	owner := s.r.active == s
	if owner {
		s.r.active = nil
	}
	s.r.mu.Unlock()
	if owner {
		s.r.player.StreamStopped()
	}
}

func (s *session) closeRemotes() {
	for id := range s.remotes {
		s.closeRemote(id)
	}
}

func (s *session) close() {
	s.stopStreams()
	s.closeRemotes()
	if s.events != nil {
		s.events.close()
	}
	s.raw.Close()
	close(s.closed)
}
