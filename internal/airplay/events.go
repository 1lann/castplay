package airplay

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"castplay/internal/airplay/hap"
)

// MediaCommand is a MediaRemote command number for the sender's player.
type MediaCommand int

const (
	CommandPlay      MediaCommand = 0
	CommandPause     MediaCommand = 1
	CommandPlayPause MediaCommand = 2
	CommandStop      MediaCommand = 3
	CommandNext      MediaCommand = 4
	CommandPrevious  MediaCommand = 5
)

// EventChannel is the reverse channel of a session: the sender connects to
// the eventPort from SETUP, and the receiver sends it RTSP requests there.
// It is how a speaker moves the sender's volume slider or asks it to pause
// or skip (shairport-sync's remote.c and ap2_event_receiver.c).
type EventChannel struct {
	s  *session
	ln net.Listener

	mu     sync.Mutex // one request/reply exchange at a time
	conn   net.Conn
	br     *bufio.Reader
	cseq   int
	volume float64 // last unit volume sent, -1 if none
}

func newEventChannel(s *session) (*EventChannel, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	e := &EventChannel{s: s, ln: ln, volume: -1}
	go e.accept()
	return e, nil
}

func (e *EventChannel) port() int { return e.ln.Addr().(*net.TCPAddr).Port }

func (e *EventChannel) close() {
	e.ln.Close()
	e.mu.Lock()
	if e.conn != nil {
		e.conn.Close()
	}
	e.mu.Unlock()
}

func (e *EventChannel) accept() {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		key := e.s.pair.SharedKey
		if key == nil {
			e.s.log.Warn("event channel opened before pairing")
			c.Close()
			continue
		}
		hc, err := hap.NewEventConn(c, key)
		if err != nil {
			c.Close()
			continue
		}
		e.mu.Lock()
		if e.conn != nil {
			e.conn.Close()
		}
		e.conn, e.br = hc, bufio.NewReader(hc)
		e.mu.Unlock()
		e.s.log.Debug("event channel connected", "from", c.RemoteAddr())
		// The sender expects the receiver's info first, as GET /info plus
		// the advertised TXT record.
		info := e.s.r.info()
		info["txtAirPlay"] = txtBytes(e.s.r.txt())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := e.post(ctx, map[string]any{"type": "updateInfo", "value": info}); err != nil {
			e.s.log.Warn("event channel updateInfo", "err", err)
		}
		cancel()
	}
}

// SetVolume moves the sender's slider for this speaker to unit volume v
// (0..1, the slider position; the sender converts to dB). Large changes are
// stepped: a big jump makes Apple Music on macOS jump its local volume
// (shairport-sync, ap2_event_send_unit_volume_notification).
func (e *EventChannel) SetVolume(ctx context.Context, from, to float64) error {
	to = math.Max(0, math.Min(1, to))
	if from < 0 {
		from = to
	}
	const step = 0.09
	for v := from; ; {
		switch {
		case to > v:
			v = math.Min(to, v+step)
		case to < v:
			v = math.Max(to, v-step)
		}
		if err := e.post(ctx, map[string]any{
			"type": "sendMediaRemoteCommand", "value": "dvlc",
			"volume": v, "params": map[string]any{"volume": v},
		}); err != nil {
			return err
		}
		if v == to {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Command asks the sender's player to play, pause, skip, etc.
func (e *EventChannel) Command(ctx context.Context, c MediaCommand) error {
	id := make([]byte, 16)
	rand.Read(id)
	return e.post(ctx, map[string]any{
		"type":                     "sendMediaRemoteCommand",
		"modernMediaRemoteCommand": strconv.Itoa(int(c)),
		"params": map[string]any{
			"kMRMediaRemoteOptionSendOptionsNumber":    0,
			"kMRMediaRemoteOptionCommandID":            fmt.Sprintf("%X-%X-%X-%X-%X", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]),
			"kMRMediaRemoteOptionIsRedirectingCommand": true,
		},
	})
}

var errNoEventChannel = errors.New("sender has not opened the event channel")

// post sends POST /command with a binary plist body and waits for the reply.
func (e *EventChannel) post(ctx context.Context, body map[string]any) error {
	return e.postHeader(ctx, body)
}

// postHeader is post with extra headers, given as name, value pairs.
func (e *EventChannel) postHeader(ctx context.Context, body map[string]any, hdr ...string) error {
	resp, err := plistResponse(body)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil {
		return errNoEventChannel
	}
	if dl, ok := ctx.Deadline(); ok {
		e.conn.SetDeadline(dl)
		defer e.conn.SetDeadline(time.Time{})
	}
	e.cseq++
	msg := fmt.Sprintf("POST /command RTSP/1.0\r\nCSeq: %d\r\nContent-Length: %d\r\nContent-Type: application/x-apple-binary-plist\r\n",
		e.cseq, len(resp.Body))
	for i := 0; i+1 < len(hdr); i += 2 {
		msg += hdr[i] + ": " + hdr[i+1] + "\r\n"
	}
	msg += "\r\n"
	if _, err := e.conn.Write(append([]byte(msg), resp.Body...)); err != nil {
		return e.fail(err)
	}
	status, err := readStatus(e.br)
	if err != nil {
		return e.fail(err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("sender replied %d to %v", status, cmp.Or(body["type"], any("remote control data")))
	}
	return nil
}

// fail drops a broken connection; called with mu held.
func (e *EventChannel) fail(err error) error {
	e.conn.Close()
	e.conn, e.br = nil, nil
	return err
}

// readStatus reads one RTSP reply, discarding its body.
func readStatus(r *bufio.Reader) (int, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return 0, err
	}
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[0], "RTSP/") {
		return 0, fmt.Errorf("event channel: bad reply %q", line)
	}
	status, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, fmt.Errorf("event channel: bad reply %q", line)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return 0, err
	}
	if n, _ := strconv.Atoi(hdr.Get("Content-Length")); n > 0 {
		if _, err := r.Discard(n); err != nil {
			return 0, err
		}
	}
	return status, nil
}

// txtBytes encodes TXT strings as DNS TXT record data.
func txtBytes(txt []string) []byte {
	var b []byte
	for _, s := range txt {
		b = append(b, byte(len(s)))
		b = append(b, s...)
	}
	return b
}
