package cast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Media commands an app supports (MEDIA_STATUS supportedMediaCommands).
const (
	CommandPause     = 1
	CommandSeek      = 2
	CommandQueueNext = 64
	CommandQueuePrev = 128
)

// NowPlaying is what an app on the device reports over the media namespace.
// AppID is "" when no media app is running (or only an ignored one).
type NowPlaying struct {
	AppID, AppName string
	// PlayerState is PLAYING, PAUSED, BUFFERING or IDLE.
	PlayerState string
	ContentID   string
	Title       string
	Artist      string
	Album       string
	ImageURL    string
	Duration    float64 // seconds, 0 if unknown or live
	CurrentTime float64 // seconds into the media at At
	At          time.Time
	Commands    int // supportedMediaCommands bits
}

// Monitor keeps a connection to a Cast device and follows whatever media app
// is running on it: Spotify, YouTube Music, a "Hey Google, play…" session.
type Monitor struct {
	log      *slog.Logger
	ignore   map[string]bool
	onChange func(NowPlaying)

	mu      sync.Mutex
	addr    string
	moved   chan struct{}
	c       *Client
	app     runningApp
	session int             // mediaSessionId
	media   json.RawMessage // last media object, which updates may omit
	np      NowPlaying
}

type runningApp struct {
	id, name, transport string
}

// NewMonitor follows the media apps on the device at addr, skipping the app
// IDs in ignore (castplay's own streaming app). onChange is called with
// every change, from the monitor's goroutines.
func NewMonitor(addr string, ignore []string, onChange func(NowPlaying), log *slog.Logger) *Monitor {
	m := &Monitor{log: log, ignore: map[string]bool{}, onChange: onChange, addr: addr, moved: make(chan struct{}, 1)}
	for _, id := range ignore {
		m.ignore[id] = true
	}
	return m
}

// SetAddr points the monitor at the device's new address.
func (m *Monitor) SetAddr(addr string) {
	m.mu.Lock()
	m.addr = addr
	m.mu.Unlock()
	select {
	case m.moved <- struct{}{}:
	default:
	}
}

// Run keeps the monitor connected until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := m.connect(ctx)
		m.reset()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		m.log.Debug("cast monitor disconnected", "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-m.moved:
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Minute)
	}
}

func (m *Monitor) connect(ctx context.Context) error {
	m.mu.Lock()
	addr := m.addr
	m.mu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	c, err := Dial(dctx, addr, m.log)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()
	c.OnMessage(m.handle)
	m.mu.Lock()
	m.c = c
	m.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = c.Request(rctx, receiverID, NSReceiver, map[string]any{"type": "GET_STATUS"})
	cancel()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Done():
		return c.closedErr()
	case <-m.moved:
		return errors.New("device moved")
	}
}

// reset forgets the connection and reports that nothing is known to play.
func (m *Monitor) reset() {
	m.mu.Lock()
	m.c = nil
	m.app = runningApp{}
	m.session, m.media = 0, nil
	changed := m.np.AppID != ""
	m.np = NowPlaying{}
	m.mu.Unlock()
	if changed {
		m.onChange(NowPlaying{})
	}
}

// handle runs on the client's read loop.
func (m *Monitor) handle(source, ns string, payload json.RawMessage) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &head) != nil {
		return
	}
	switch {
	case ns == NSReceiver && head.Type == "RECEIVER_STATUS":
		m.receiverStatus(payload)
	case ns == NSMedia && head.Type == "MEDIA_STATUS":
		m.mediaStatus(source, payload)
	}
}

func (m *Monitor) receiverStatus(payload json.RawMessage) {
	var st struct {
		Status struct {
			Applications []struct {
				AppID        string `json:"appId"`
				DisplayName  string `json:"displayName"`
				TransportID  string `json:"transportId"`
				IsIdleScreen bool   `json:"isIdleScreen"`
				Namespaces   []struct {
					Name string `json:"name"`
				} `json:"namespaces"`
			} `json:"applications"`
		} `json:"status"`
	}
	if json.Unmarshal(payload, &st) != nil {
		return
	}
	var app runningApp
	for _, a := range st.Status.Applications {
		if a.IsIdleScreen || m.ignore[a.AppID] {
			continue
		}
		for _, n := range a.Namespaces {
			if n.Name == NSMedia {
				app = runningApp{id: a.AppID, name: a.DisplayName, transport: a.TransportID}
			}
		}
	}
	m.mu.Lock()
	if app == m.app {
		m.mu.Unlock()
		return
	}
	m.app = app
	m.session, m.media = 0, nil
	c := m.c
	np := NowPlaying{AppID: app.id, AppName: app.name, PlayerState: "IDLE"}
	if app.id == "" {
		np = NowPlaying{}
	}
	m.np = np
	m.mu.Unlock()
	m.log.Info("cast app", "app", app.name, "appID", app.id)
	m.onChange(np)
	if app.transport == "" || c == nil {
		return
	}
	go func() {
		if err := c.Connect(app.transport); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := c.Request(ctx, app.transport, NSMedia, map[string]any{"type": "GET_STATUS"}); err != nil {
			m.log.Debug("cast monitor: media status", "app", app.name, "err", err)
		}
	}()
}

type mediaStatus struct {
	MediaSessionID         int             `json:"mediaSessionId"`
	PlayerState            string          `json:"playerState"`
	CurrentTime            float64         `json:"currentTime"`
	SupportedMediaCommands int             `json:"supportedMediaCommands"`
	Media                  json.RawMessage `json:"media"`
}

type mediaInfo struct {
	ContentID string  `json:"contentId"`
	Duration  float64 `json:"duration"`
	Metadata  struct {
		Title     string `json:"title"`
		Subtitle  string `json:"subtitle"`
		Artist    string `json:"artist"`
		AlbumName string `json:"albumName"`
		Images    []struct {
			URL string `json:"url"`
		} `json:"images"`
	} `json:"metadata"`
}

func (m *Monitor) mediaStatus(source string, payload json.RawMessage) {
	var p struct {
		Status []mediaStatus `json:"status"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	if m.app.transport == "" || source != m.app.transport {
		m.mu.Unlock()
		return
	}
	np := NowPlaying{AppID: m.app.id, AppName: m.app.name, PlayerState: "IDLE"}
	if len(p.Status) > 0 {
		st := p.Status[0]
		if st.MediaSessionID != m.session {
			m.session, m.media = st.MediaSessionID, nil
		}
		if len(st.Media) > 0 {
			m.media = st.Media
		}
		np.PlayerState = st.PlayerState
		np.CurrentTime, np.At = st.CurrentTime, now
		np.Commands = st.SupportedMediaCommands
		var mi mediaInfo
		if m.media != nil && json.Unmarshal(m.media, &mi) == nil {
			md := mi.Metadata
			np.ContentID, np.Duration = mi.ContentID, mi.Duration
			np.Title, np.Album = md.Title, md.AlbumName
			np.Artist = md.Artist
			if np.Artist == "" {
				np.Artist = md.Subtitle
			}
			if len(md.Images) > 0 {
				np.ImageURL = md.Images[0].URL
			}
		}
	}
	changed := np != m.np
	m.np = np
	m.mu.Unlock()
	if changed {
		m.log.Debug("cast now playing", "app", np.AppName, "state", np.PlayerState, "title", np.Title,
			"artist", np.Artist, "position", np.CurrentTime)
		m.onChange(np)
	}
}

// Media sends a media command (PLAY, PAUSE, STOP, SEEK, QUEUE_UPDATE) to the
// running app's media session; fields are merged into the message.
func (m *Monitor) Media(ctx context.Context, typ string, fields map[string]any) error {
	m.mu.Lock()
	c, transport, session := m.c, m.app.transport, m.session
	m.mu.Unlock()
	if c == nil || transport == "" || session == 0 {
		return errors.New("cast: no media session")
	}
	msg := map[string]any{"type": typ, "mediaSessionId": session}
	for k, v := range fields {
		msg[k] = v
	}
	r, err := c.Request(ctx, transport, NSMedia, msg)
	if err != nil {
		return err
	}
	if r.Type != "MEDIA_STATUS" {
		return fmt.Errorf("cast: %s: %s %s", typ, r.Type, r.Payload)
	}
	return nil
}
