package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// published is a chunk plus the wall time it became available to clients.
type published struct {
	chunk
	at time.Time
}

// connInfo records one HTTP request from the receiver.
type connInfo struct {
	at         time.Time
	method     string
	rangeHdr   string
	userAgent  string
	firstStart int64 // signal sample of the first chunk sent; -1 if none
	firstByte  time.Time
	bytes      int64
	closedAt   time.Time
}

// liveStream paces a source in real time and fans it out to HTTP clients the
// way an internet radio server does: a new client joins at the live edge, or
// `burst` behind it if a backlog is configured.
type liveStream struct {
	src     source
	path    string
	lead    time.Duration
	burst   time.Duration
	httpLen string
	log     *slog.Logger

	mu      sync.Mutex
	cond    *sync.Cond
	chunks  []published
	base    int // absolute index of chunks[0]
	t0      time.Time
	closed  bool
	conns   []*connInfo
	maxLate time.Duration
}

func newLiveStream(src source, o options, log *slog.Logger) *liveStream {
	s := &liveStream{
		src:     src,
		path:    fmt.Sprintf("/stream-%d.%s", time.Now().UnixNano()%1e6, src.ext()),
		lead:    o.lead,
		burst:   o.burst,
		httpLen: o.httpLen,
		log:     log,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// run publishes chunks until ctx is done. Signal sample p is due at
// t0 + p/sampleRate - lead.
func (s *liveStream) run(ctx context.Context) {
	s.mu.Lock()
	s.t0 = time.Now()
	t0 := s.t0
	s.mu.Unlock()
	keep := s.burst + 2*time.Second
	for ctx.Err() == nil {
		c := s.src.next()
		due := t0.Add(samplesToDur(c.start) - s.lead)
		if d := time.Until(due); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
			}
		}
		now := time.Now()
		s.mu.Lock()
		if late := now.Sub(due); late > s.maxLate && c.start >= 0 {
			s.maxLate = late
		}
		s.chunks = append(s.chunks, published{chunk: c, at: now})
		// Trim history older than the burst window.
		cut := 0
		for cut < len(s.chunks)-1 && now.Sub(s.chunks[cut].at) > keep {
			cut++
		}
		if cut > 0 {
			s.chunks = append(s.chunks[:0:0], s.chunks[cut:]...)
			s.base += cut
		}
		s.cond.Broadcast()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *liveStream) start() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t0
}

func (s *liveStream) connections() []connInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]connInfo, len(s.conns))
	for i, c := range s.conns {
		out[i] = *c
	}
	return out
}

// activeStart is the first signal sample of the most recent connection that
// actually received audio, used to turn MEDIA_STATUS.currentTime into a signal
// position.
func (s *liveStream) activeStart() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.conns) - 1; i >= 0; i-- {
		if s.conns[i].firstStart >= 0 {
			return s.conns[i].firstStart, true
		}
	}
	return 0, false
}

func (s *liveStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.path {
		http.NotFound(w, r)
		return
	}
	info := &connInfo{at: time.Now(), method: r.Method, rangeHdr: r.Header.Get("Range"),
		userAgent: r.UserAgent(), firstStart: -1}
	s.mu.Lock()
	s.conns = append(s.conns, info)
	s.mu.Unlock()
	s.log.Info("http request", "method", r.Method, "range", info.rangeHdr, "ua", info.userAgent, "remote", r.RemoteAddr)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, bw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() {
		conn.Close()
		s.mu.Lock()
		info.closedAt = time.Now()
		s.mu.Unlock()
		s.log.Info("http closed", "remote", r.RemoteAddr, "bytes", info.bytes)
	}()

	hdr := "HTTP/1.1 200 OK\r\nContent-Type: " + s.src.contentType() + "\r\nCache-Control: no-cache\r\nConnection: close\r\n"
	switch s.httpLen {
	case "chunked":
		hdr += "Transfer-Encoding: chunked\r\n"
	case "fixed":
		hdr += "Content-Length: 2147483647\r\n"
	}
	hdr += "\r\n"
	if _, err := bw.WriteString(hdr); err != nil || bw.Flush() != nil || r.Method == http.MethodHead {
		return
	}

	write := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if s.httpLen == "chunked" {
			fmt.Fprintf(bw, "%x\r\n", len(b))
		}
		bw.Write(b)
		if s.httpLen == "chunked" {
			bw.WriteString("\r\n")
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		s.mu.Lock()
		info.bytes += int64(len(b))
		s.mu.Unlock()
		return nil
	}
	if err := write(s.src.header()); err != nil {
		return
	}

	// Join at the live edge, minus the configured burst backlog.
	s.mu.Lock()
	idx := s.base + len(s.chunks)
	if s.burst > 0 {
		now := time.Now()
		for i := len(s.chunks) - 1; i >= 0 && now.Sub(s.chunks[i].at) <= s.burst; i-- {
			idx = s.base + i
		}
	}
	s.mu.Unlock()

	go drain(conn) // notice client hang-ups promptly
	for {
		s.mu.Lock()
		for !s.closed && idx >= s.base+len(s.chunks) {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		if idx < s.base { // fell behind the retained window
			idx = s.base
		}
		batch := append([]published(nil), s.chunks[idx-s.base:]...)
		idx = s.base + len(s.chunks)
		if info.firstStart < 0 && len(batch) > 0 {
			info.firstStart = batch[0].start
			info.firstByte = time.Now()
		}
		s.mu.Unlock()
		for _, p := range batch {
			if err := write(p.data); err != nil {
				return
			}
		}
	}
}

func drain(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		if _, err := r.ReadByte(); err != nil {
			c.Close()
			return
		}
	}
}

func samplesToDur(n int64) time.Duration {
	return time.Duration(n) * time.Second / sampleRate
}
