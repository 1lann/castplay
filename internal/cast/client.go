// Package cast is a minimal Google Cast v2 sender: TLS channel, heartbeat,
// receiver control (launch, volume) and the media namespace (load, status,
// stop). It follows the same message flow as aircast's castcore.c.
package cast

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	NSConnection = "urn:x-cast:com.google.cast.tp.connection"
	NSHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	NSReceiver   = "urn:x-cast:com.google.cast.receiver"
	NSMedia      = "urn:x-cast:com.google.cast.media"
	// NSWebRTC carries Cast Streaming (mirroring) OFFER/ANSWER. It correlates
	// replies by "seqNum" rather than "requestId".
	NSWebRTC = "urn:x-cast:com.google.cast.webrtc"

	// DefaultMediaReceiver is Google's stock receiver app (CC1AD845).
	DefaultMediaReceiver = "CC1AD845"
	// AudioMirroringReceiver is the built-in audio-only Cast Streaming
	// receiver app (85CDB22F), as used by shoutout.
	AudioMirroringReceiver = "85CDB22F"

	senderID   = "sender-0"
	receiverID = "receiver-0"

	heartbeatInterval = 5 * time.Second
	heartbeatTimeout  = 3 * heartbeatInterval
)

// MediaStatus is the subset of a MEDIA_STATUS entry the spike cares about.
type MediaStatus struct {
	MediaSessionID int     `json:"mediaSessionId"`
	PlayerState    string  `json:"playerState"`
	IdleReason     string  `json:"idleReason"`
	CurrentTime    float64 `json:"currentTime"`
}

// Reply is a JSON payload received from the device, stamped with its arrival
// time so callers can measure control-plane latency.
type Reply struct {
	Type      string
	RequestID int
	Payload   json.RawMessage
	At        time.Time
}

type Client struct {
	conn net.Conn
	log  *slog.Logger

	writeMu sync.Mutex
	nextID  atomic.Int64

	mu          sync.Mutex
	pending     map[int]chan Reply
	transportID string
	appSession  string // receiver-level sessionId of the launched app
	sessionID   int    // media session
	onMedia     func(MediaStatus, time.Time)
	onVolume    func(level float64, muted bool)

	lastRecv  atomic.Int64 // unix nanos
	done      chan struct{}
	appClosed chan struct{}
	closeOnce sync.Once
	err       error
}

// Dial opens the TLS channel to a Cast device (usually port 8009; groups use
// another port) and starts the read and heartbeat loops. Cast devices present
// self-signed certificates, so verification is skipped as every sender does.
func Dial(ctx context.Context, addr string, log *slog.Logger) (*Client, error) {
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cast: dial %s: %w", addr, err)
	}
	c := &Client{
		conn:      conn,
		log:       log,
		pending:   make(map[int]chan Reply),
		done:      make(chan struct{}),
		appClosed: make(chan struct{}),
	}
	c.lastRecv.Store(time.Now().UnixNano())
	go c.readLoop()
	if err := c.send(receiverID, NSConnection, map[string]any{"type": "CONNECT"}); err != nil {
		conn.Close()
		return nil, err
	}
	go c.heartbeatLoop()
	return c, nil
}

// LocalAddr is the address the device sees us on, which is the address it can
// reach our HTTP server at.
func (c *Client) LocalAddr() net.Addr { return c.conn.LocalAddr() }

// OnMediaStatus registers a callback for every MEDIA_STATUS, solicited or not.
func (c *Client) OnMediaStatus(f func(MediaStatus, time.Time)) {
	c.mu.Lock()
	c.onMedia = f
	c.mu.Unlock()
}

// OnVolume registers a callback for the device volume in every
// RECEIVER_STATUS, including the replies to our own SetVolume/SetMuted.
func (c *Client) OnVolume(f func(level float64, muted bool)) {
	c.mu.Lock()
	c.onVolume = f
	c.mu.Unlock()
}

func (c *Client) Done() <-chan struct{} { return c.done }

// AppClosed is closed when the launched app closes our virtual connection to
// it: the app stopped, whether by StopApp or on the device (its pause button
// or a voice command stops a mirroring session).
func (c *Client) AppClosed() <-chan struct{} { return c.appClosed }

func (c *Client) Close() error {
	c.mu.Lock()
	tid := c.transportID
	c.mu.Unlock()
	if tid != "" {
		_ = c.send(tid, NSConnection, map[string]any{"type": "CLOSE"})
	}
	return c.conn.Close()
}

// Launch starts appID (or reuses it if already running) and connects to its
// transport so media commands can be sent.
func (c *Client) Launch(ctx context.Context, appID string) error {
	r, err := c.request(ctx, receiverID, NSReceiver, map[string]any{"type": "LAUNCH", "appId": appID})
	if err != nil {
		return err
	}
	var st struct {
		Status struct {
			Applications []struct {
				AppID       string `json:"appId"`
				TransportID string `json:"transportId"`
				SessionID   string `json:"sessionId"`
			} `json:"applications"`
		} `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(r.Payload, &st); err != nil {
		return fmt.Errorf("cast: launch reply: %w", err)
	}
	if r.Type != "RECEIVER_STATUS" {
		return fmt.Errorf("cast: launch failed: %s %s", r.Type, st.Reason)
	}
	for _, a := range st.Status.Applications {
		if a.AppID == appID {
			c.mu.Lock()
			c.transportID = a.TransportID
			c.appSession = a.SessionID
			c.sessionID = 0
			c.mu.Unlock()
			return c.send(a.TransportID, NSConnection, map[string]any{"type": "CONNECT", "origin": map[string]any{}})
		}
	}
	return fmt.Errorf("cast: app %s not running after launch", appID)
}

// StopApp stops the launched receiver app, ending any mirroring session.
func (c *Client) StopApp(ctx context.Context) error {
	c.mu.Lock()
	sid := c.appSession
	c.mu.Unlock()
	if sid == "" {
		return nil
	}
	_, err := c.request(ctx, receiverID, NSReceiver, map[string]any{"type": "STOP", "sessionId": sid})
	c.mu.Lock()
	c.transportID, c.appSession, c.sessionID = "", "", 0
	c.mu.Unlock()
	return err
}

// SetVolume sets the device (not stream) volume, 0..1.
func (c *Client) SetVolume(ctx context.Context, level float64) error {
	_, err := c.request(ctx, receiverID, NSReceiver, map[string]any{
		"type": "SET_VOLUME", "volume": map[string]any{"level": level},
	})
	return err
}

// SetMuted mutes or unmutes the device.
func (c *Client) SetMuted(ctx context.Context, muted bool) error {
	_, err := c.request(ctx, receiverID, NSReceiver, map[string]any{
		"type": "SET_VOLUME", "volume": map[string]any{"muted": muted},
	})
	return err
}

// LoadRequest describes a media LOAD. StreamType is "LIVE" or "BUFFERED".
type LoadRequest struct {
	URL         string
	ContentType string
	StreamType  string
	Title       string
}

// Load sends LOAD and waits for the device's reply. Note that receivers often
// hold the reply until enough media has buffered, so the reply time is itself
// a useful startup measurement.
func (c *Client) Load(ctx context.Context, lr LoadRequest) (MediaStatus, Reply, error) {
	tid, err := c.transport()
	if err != nil {
		return MediaStatus{}, Reply{}, err
	}
	media := map[string]any{
		"contentId":   lr.URL,
		"contentType": lr.ContentType,
		"streamType":  lr.StreamType,
	}
	if lr.Title != "" {
		media["metadata"] = map[string]any{"metadataType": 3, "title": lr.Title}
	}
	r, err := c.request(ctx, tid, NSMedia, map[string]any{
		"type": "LOAD", "media": media, "autoplay": true, "currentTime": 0,
	})
	if err != nil {
		return MediaStatus{}, r, err
	}
	if r.Type != "MEDIA_STATUS" {
		return MediaStatus{}, r, fmt.Errorf("cast: load failed: %s %s", r.Type, r.Payload)
	}
	st, _ := parseMediaStatus(r.Payload)
	return st, r, nil
}

// Status asks the media session for a fresh MEDIA_STATUS.
func (c *Client) Status(ctx context.Context) (MediaStatus, Reply, error) {
	tid, err := c.transport()
	if err != nil {
		return MediaStatus{}, Reply{}, err
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	msg := map[string]any{"type": "GET_STATUS"}
	if sid != 0 {
		msg["mediaSessionId"] = sid
	}
	r, err := c.request(ctx, tid, NSMedia, msg)
	if err != nil {
		return MediaStatus{}, r, err
	}
	st, ok := parseMediaStatus(r.Payload)
	if !ok {
		return st, r, errors.New("cast: no media session")
	}
	return st, r, nil
}

// Stop ends the current media session, leaving the receiver app running.
func (c *Client) Stop(ctx context.Context) error {
	tid, err := c.transport()
	if err != nil {
		return err
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid == 0 {
		return nil
	}
	_, err = c.request(ctx, tid, NSMedia, map[string]any{"type": "STOP", "mediaSessionId": sid})
	return err
}

func (c *Client) transport() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.transportID == "" {
		return "", errors.New("cast: no app launched")
	}
	return c.transportID, nil
}

func (c *Client) request(ctx context.Context, dest, ns string, payload map[string]any) (Reply, error) {
	id := int(c.nextID.Add(1))
	if ns == NSWebRTC {
		payload["seqNum"] = id
	} else {
		payload["requestId"] = id
	}
	ch := make(chan Reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(dest, ns, payload); err != nil {
		return Reply{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	case <-c.done:
		return Reply{}, c.closedErr()
	}
}

func (c *Client) send(dest, ns string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if ns != NSHeartbeat {
		c.log.Debug("cast send", "dest", dest, "ns", ns, "payload", string(b))
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeMessage(c.conn, &Message{SourceID: senderID, DestinationID: dest, Namespace: ns, PayloadUTF8: string(b)})
}

func (c *Client) closedErr() error {
	if c.err != nil {
		return c.err
	}
	return errors.New("cast: connection closed")
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		m, err := readMessage(c.conn)
		if err != nil {
			c.err = fmt.Errorf("cast: read: %w", err)
			return
		}
		now := time.Now()
		c.lastRecv.Store(now.UnixNano())
		if m.PayloadUTF8 == "" {
			continue
		}
		var head struct {
			Type      string `json:"type"`
			RequestID int    `json:"requestId"`
			SeqNum    int    `json:"seqNum"`
		}
		if json.Unmarshal([]byte(m.PayloadUTF8), &head) != nil {
			continue
		}
		if m.Namespace == NSWebRTC {
			head.RequestID = head.SeqNum
		}
		switch {
		case m.Namespace == NSHeartbeat && head.Type == "PING":
			_ = c.send(m.SourceID, NSHeartbeat, map[string]any{"type": "PONG"})
			continue
		case m.Namespace == NSHeartbeat:
			continue
		case m.Namespace == NSConnection && head.Type == "CLOSE":
			c.log.Info("cast: device closed virtual connection", "source", m.SourceID)
			if m.SourceID != receiverID {
				c.closeOnce.Do(func() { close(c.appClosed) })
			}
		}
		c.log.Debug("cast recv", "source", m.SourceID, "ns", m.Namespace, "payload", m.PayloadUTF8)

		r := Reply{Type: head.Type, RequestID: head.RequestID, Payload: json.RawMessage(m.PayloadUTF8), At: now}
		c.mu.Lock()
		if head.Type == "MEDIA_STATUS" {
			if st, ok := parseMediaStatus(r.Payload); ok {
				c.sessionID = st.MediaSessionID
				if c.onMedia != nil {
					f := c.onMedia
					c.mu.Unlock()
					f(st, now)
					c.mu.Lock()
				}
			}
		}
		if head.Type == "RECEIVER_STATUS" && c.onVolume != nil {
			var st struct {
				Status struct {
					Volume *struct {
						Level float64 `json:"level"`
						Muted bool    `json:"muted"`
					} `json:"volume"`
				} `json:"status"`
			}
			if json.Unmarshal(r.Payload, &st) == nil && st.Status.Volume != nil {
				f := c.onVolume
				c.mu.Unlock()
				f(st.Status.Volume.Level, st.Status.Volume.Muted)
				c.mu.Lock()
			}
		}
		ch := c.pending[head.RequestID]
		c.mu.Unlock()
		if ch != nil && head.RequestID != 0 {
			select {
			case ch <- r:
			default:
			}
		}
	}
}

func (c *Client) heartbeatLoop() {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastRecv.Load())) > heartbeatTimeout {
				c.log.Warn("cast: heartbeat timeout, closing")
				c.conn.Close()
				return
			}
			if err := c.send(receiverID, NSHeartbeat, map[string]any{"type": "PING"}); err != nil {
				return
			}
		}
	}
}

func parseMediaStatus(payload json.RawMessage) (MediaStatus, bool) {
	var p struct {
		Status []MediaStatus `json:"status"`
	}
	if json.Unmarshal(payload, &p) != nil || len(p.Status) == 0 {
		return MediaStatus{}, false
	}
	return p.Status[0], true
}
