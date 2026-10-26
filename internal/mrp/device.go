package mrp

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Channel is one sender's remote control channel to the speaker.
type Channel interface {
	// Send delivers a frame of messages to the sender.
	Send(ctx context.Context, frame []byte) error
	String() string
}

type Config struct {
	Device DeviceInfo
	// Command carries out a sender's command; nil rejects them all.
	Command func(ctx context.Context, c SendCommand) error
	// SetVolume sets the speaker's volume, 0..1; nil leaves volume control
	// unavailable.
	SetVolume func(ctx context.Context, v float64) error
	// Artwork fetches the image at url scaled to about width pixels.
	Artwork func(ctx context.Context, url string, width int) (*Artwork, error)
	Log     *slog.Logger
}

// Server is the speaker's MediaRemote device, shared by every sender's
// channel: they all see the same now-playing state.
type Server struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	np    NowPlaying
	vol   float64 // speaker volume 0..1, -1 if unknown
	peers map[Channel]*peer
	art   struct {
		id    string
		width int
		a     *Artwork
	}
}

type peer struct {
	ch   Channel
	log  *slog.Logger
	out  chan []byte
	done chan struct{}

	// Set once the sender subscribes (CLIENT_UPDATES_CONFIG), or shortly
	// after the handshake if it never does; guarded by Server.mu.
	subscribed bool
	app        App // the client last announced to it
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, log: cfg.Log, vol: -1, peers: map[Channel]*peer{}}
}

// Open starts serving a new channel.
func (s *Server) Open(ch Channel) {
	p := &peer{ch: ch, log: s.log.With("channel", ch.String()), out: make(chan []byte, 64), done: make(chan struct{})}
	s.mu.Lock()
	s.peers[ch] = p
	s.mu.Unlock()
	go p.sendLoop()
	p.log.Info("remote control channel opened")
}

// Close stops serving a channel.
func (s *Server) Close(ch Channel) {
	s.mu.Lock()
	p := s.peers[ch]
	delete(s.peers, ch)
	s.mu.Unlock()
	if p != nil {
		close(p.done)
		p.log.Info("remote control channel closed")
	}
}

// Data handles a frame the sender sent on ch. It doesn't block on the
// network: replies are queued.
func (s *Server) Data(ch Channel, frame []byte) {
	s.mu.Lock()
	p := s.peers[ch]
	s.mu.Unlock()
	if p == nil {
		s.log.Warn("remote control data on unknown channel", "channel", ch.String())
		return
	}
	msgs, err := Split(frame)
	if err != nil {
		p.log.Warn("remote control: bad frame", "err", err, "len", len(frame))
	}
	for _, m := range msgs {
		s.handle(p, m)
	}
}

func (s *Server) handle(p *peer, m *Message) {
	p.log.Debug("mrp recv", "type", m.Type, "id", m.Identifier, "fields", m.Describe())
	switch m.Type {
	case TypeDeviceInfo:
		pi := parseDeviceInfo(m)
		p.log.Info("remote control: sender hello", "name", pi.Name, "model", pi.Model, "build", pi.Build)
		p.send(deviceInfoMessage(m.Identifier, s.cfg.Device))
		time.AfterFunc(2*time.Second, func() { s.subscribe(p, "no CLIENT_UPDATES_CONFIG") })
	case TypeClientUpdatesConfig:
		if m.Identifier != "" {
			p.send(genericMessage(m.Identifier))
		}
		s.subscribe(p, "CLIENT_UPDATES_CONFIG")
	case TypeGetState:
		s.mu.Lock()
		np := s.np
		s.mu.Unlock()
		p.send(setState(m.Identifier, np, nil))
	case TypePlaybackQueueRequest:
		go s.answerQueue(p, m)
	case TypeSendCommand:
		go s.command(p, m)
	case TypeGetVolume:
		s.mu.Lock()
		v := max(s.vol, 0)
		s.mu.Unlock()
		p.send(getVolumeResult(m.Identifier, v))
	case TypeSetVolume:
		go s.setVolume(p, m)
	default:
		if m.Identifier != "" {
			p.send(genericMessage(m.Identifier))
		}
	}
}

// subscribe starts pushing now-playing updates to p, beginning with the
// current state.
func (s *Server) subscribe(p *peer, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.subscribed || s.peers[p.ch] != p {
		return
	}
	p.subscribed = true
	p.log.Debug("remote control: subscribed", "why", why)
	if s.cfg.SetVolume != nil {
		d := s.cfg.Device
		p.send(volumeAvailability())
		p.send(updateOutputDevice(d, s.vol))
		for _, uid := range d.outputDeviceUIDs() {
			p.send(volumeCapabilities(uid))
			if s.vol >= 0 {
				p.send(volumeDidChange(s.vol, uid))
			}
		}
	}
	s.pushLocked(p)
}

// pushLocked sends p the current state; called with mu held.
func (s *Server) pushLocked(p *peer) {
	if s.np.App != p.app && s.np.App.BundleID != "" {
		p.app = s.np.App
		p.send(setNowPlayingClient(s.np.App))
	}
	p.send(setState("", s.np, nil))
}

// SetNowPlaying replaces the now-playing state and pushes it to every
// subscribed sender.
func (s *Server) SetNowPlaying(np NowPlaying) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.np = np
	for _, p := range s.peers {
		if p.subscribed {
			s.pushLocked(p)
		}
	}
}

// SetVolume reports the speaker's volume (0..1) to every subscribed sender.
func (s *Server) SetVolume(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == s.vol {
		return
	}
	s.vol = v
	if s.cfg.SetVolume == nil {
		return
	}
	for _, p := range s.peers {
		if p.subscribed {
			p.send(updateOutputDevice(s.cfg.Device, v))
			for _, uid := range s.cfg.Device.outputDeviceUIDs() {
				p.send(volumeDidChange(v, uid))
			}
		}
	}
}

func (s *Server) setVolume(p *peer, m *Message) {
	v := min(max(parseSetVolume(m), 0), 1)
	var err error = errNoCommands
	if s.cfg.SetVolume != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = s.cfg.SetVolume(ctx, v)
		cancel()
	}
	p.log.Debug("remote control: set volume", "volume", v, "err", err)
	if m.Identifier != "" {
		p.send(genericMessage(m.Identifier))
	}
}

func (s *Server) answerQueue(p *peer, m *Message) {
	q := parseQueueRequest(m)
	s.mu.Lock()
	np := s.np
	s.mu.Unlock()
	var art *Artwork
	if np.hasArtwork() && q.ArtworkWidth != 0 {
		width := int(q.ArtworkWidth)
		if width <= 0 || width > 600 {
			width = 600
		}
		var err error
		if art, err = s.artwork(np, width); err != nil {
			p.log.Warn("remote control: artwork", "url", np.ArtworkURL, "err", err)
		}
	}
	p.log.Debug("remote control: queue request", "location", q.Location, "length", q.Length,
		"artworkWidth", q.ArtworkWidth, "artwork", art != nil)
	p.send(setState(m.Identifier, np, art))
}

func (s *Server) artwork(np NowPlaying, width int) (*Artwork, error) {
	id := np.artworkID()
	s.mu.Lock()
	if s.art.id == id && s.art.width == width {
		a := s.art.a
		s.mu.Unlock()
		return a, nil
	}
	s.mu.Unlock()
	var a *Artwork
	var err error
	switch {
	case len(np.ArtworkData) > 0:
		a, err = ScaleArtwork(np.ArtworkData, width)
	case s.cfg.Artwork != nil:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a, err = s.cfg.Artwork(ctx, np.ArtworkURL, width)
		cancel()
	}
	if err != nil || a == nil {
		return nil, err
	}
	s.mu.Lock()
	s.art.id, s.art.width, s.art.a = id, width, a
	s.mu.Unlock()
	return a, nil
}

func (s *Server) command(p *peer, m *Message) {
	c := parseSendCommand(m)
	s.mu.Lock()
	path := s.np.App.playerPath()
	s.mu.Unlock()
	var err error
	if s.cfg.Command == nil {
		err = errNoCommands
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = s.cfg.Command(ctx, c)
		cancel()
	}
	p.log.Info("remote control: command", "command", c.Command, "position", c.Position, "err", err)
	p.send(sendCommandResult(m.Identifier, path, err == nil))
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errNoCommands = constErr("mrp: commands not supported")

// send queues a message for the sender, dropping it if the sender is not
// keeping up.
func (p *peer) send(m enc) {
	if len(m) > 1 && m[0] == 0x08 {
		p.log.Debug("mrp send", "type", m[1])
	}
	select {
	case p.out <- Frame(m):
	case <-p.done:
	default:
		p.log.Warn("remote control: send queue full, dropping message")
	}
}

func (p *peer) sendLoop() {
	for {
		select {
		case <-p.done:
			return
		case f := <-p.out:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.ch.Send(ctx, f)
			cancel()
			if err != nil {
				p.log.Warn("remote control: send", "err", err)
			}
		}
	}
}

// NowPlaying returns the current state.
func (s *Server) NowPlaying() NowPlaying {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.np
}
