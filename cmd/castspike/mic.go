package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// heard is a beep onset placed on the wall clock.
type heard struct {
	at    time.Time
	tone  int
	snrDB float64
}

// anchor ties a detector sample index to the wall time it was captured.
type anchor struct {
	sample int64
	at     time.Time
}

// mic captures audio and timestamps detected beeps.
//
// On macOS it runs bin/miccap, which stamps every buffer with CoreAudio's
// capture time. ffmpeg's avfoundation input is not usable there: it drops
// ~10% of buffers, so its sample count runs slow against the wall clock.
//
// Elsewhere it uses ffmpeg and bounds the capture start instead: sample n
// cannot have been captured later than the moment the bytes containing it
// arrived, so the minimum of arrival - n/rate over the run converges on the
// true start minus the device's input latency.
type mic struct {
	log  *slog.Logger
	rate int

	mu      sync.Mutex
	anchors []anchor
	onsets  []onset
	samples int64
	err     error
	done    chan struct{}
}

func startMic(ctx context.Context, input string, log *slog.Logger) (*mic, error) {
	if runtime.GOOS == "darwin" {
		return startMiccap(ctx, input, log)
	}
	return startFFmpegMic(ctx, input, log)
}

func (m *mic) fail(err error) {
	m.mu.Lock()
	if m.err == nil {
		m.err = err
	}
	m.mu.Unlock()
}

// --- macOS: miccap -------------------------------------------------------------

func miccapPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(exe), "miccap")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("miccap"); err == nil {
		return p, nil
	}
	return "", errors.New("mic: bin/miccap not found; build it with: swiftc -O tools/miccap/main.swift -o bin/miccap")
}

func startMiccap(ctx context.Context, input string, log *slog.Logger) (*mic, error) {
	path, err := miccapPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, input)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mic: start miccap: %w", err)
	}
	r := bufio.NewReaderSize(stdout, 1<<16)
	var hdr [20]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || string(hdr[:4]) != "MCAP" {
		cmd.Wait()
		return nil, fmt.Errorf("mic: miccap: %s", strings.TrimSpace(stderr.String()))
	}
	rate := math.Float64frombits(binary.LittleEndian.Uint64(hdr[4:]))
	inLat := time.Duration(math.Float64frombits(binary.LittleEndian.Uint64(hdr[12:])) * float64(time.Second))
	m := &mic{log: log, rate: int(rate), done: make(chan struct{})}
	log.Info("mic", "backend", "miccap", "rate", m.rate, "inputLatency", inLat, "note", strings.TrimSpace(stderr.String()))

	go func() {
		defer close(m.done)
		det := newDetector(m.rate)
		var bh [12]byte
		var pcm []byte
		var next time.Time // expected capture time of the next buffer
		for {
			if _, err := io.ReadFull(r, bh[:]); err != nil {
				werr := cmd.Wait()
				if ctx.Err() == nil {
					m.fail(fmt.Errorf("mic: miccap stopped: %v %v %s", err, werr, strings.TrimSpace(stderr.String())))
				}
				return
			}
			at := time.Unix(0, int64(binary.LittleEndian.Uint64(bh[:8]))).Add(-inLat)
			n := int(binary.LittleEndian.Uint32(bh[8:]))
			if cap(pcm) < n*2 {
				pcm = make([]byte, n*2)
			}
			pcm = pcm[:n*2]
			if _, err := io.ReadFull(r, pcm); err != nil {
				m.fail(err)
				return
			}
			// Keep the detector's timeline continuous across dropped buffers.
			if !next.IsZero() {
				if gap := at.Sub(next); gap > time.Millisecond {
					silence := make([]byte, int(gap.Seconds()*rate)*2)
					m.feed(det, silence, time.Time{})
					m.log.Debug("mic gap", "gap", gap)
				}
			}
			next = at.Add(time.Duration(float64(n) / rate * float64(time.Second)))
			m.feed(det, pcm, at)
		}
	}()
	return m, nil
}

// feed runs the detector over pcm. If at is non-zero it is the capture time of
// pcm's first sample and becomes a new anchor.
func (m *mic) feed(det *detector, pcm []byte, at time.Time) {
	ons := det.feed(pcm)
	m.mu.Lock()
	if !at.IsZero() {
		m.anchors = append(m.anchors, anchor{sample: m.samples, at: at})
	}
	m.samples += int64(len(pcm) / 2)
	m.onsets = append(m.onsets, ons...)
	m.mu.Unlock()
}

// --- Linux and others: ffmpeg --------------------------------------------------

const ffmpegMicRate = 48000

func ffmpegMicArgs(input string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer"}
	if runtime.GOOS == "linux" {
		args = append(args, "-f", "pulse")
	}
	return append(args, "-i", input,
		"-ac", "1", "-ar", fmt.Sprint(ffmpegMicRate), "-f", "s16le", "-flush_packets", "1", "pipe:1")
}

func startFFmpegMic(ctx context.Context, input string, log *slog.Logger) (*mic, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegMicArgs(input)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mic: start ffmpeg: %w", err)
	}
	m := &mic{log: log, rate: ffmpegMicRate, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		det := newDetector(m.rate)
		buf := make([]byte, 4096)
		var odd []byte
		for {
			n, err := stdout.Read(buf)
			now := time.Now()
			if n > 0 {
				data := append(odd, buf[:n]...)
				even := len(data) &^ 1
				odd = append([]byte(nil), data[even:]...)
				ons := det.feed(data[:even])
				m.mu.Lock()
				m.samples += int64(even / 2)
				bound := now.Add(-time.Duration(m.samples) * time.Second / time.Duration(m.rate))
				if len(m.anchors) == 0 {
					m.anchors = []anchor{{0, bound}}
				} else if bound.Before(m.anchors[0].at) {
					m.anchors[0].at = bound
				}
				m.onsets = append(m.onsets, ons...)
				m.mu.Unlock()
			}
			if err != nil {
				werr := cmd.Wait()
				if ctx.Err() == nil {
					m.fail(fmt.Errorf("mic: %v %v %s", err, werr, strings.TrimSpace(stderr.String())))
				}
				return
			}
		}
	}()
	return m, nil
}

// --- shared ------------------------------------------------------------------

// waitReady blocks until audio is flowing, so a broken mic fails fast.
func (m *mic) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		n, err := m.samples, m.err
		m.mu.Unlock()
		if err != nil {
			return err
		}
		if n > int64(m.rate/2) {
			return nil
		}
		select {
		case <-m.done:
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.err != nil {
				return m.err
			}
			return errors.New("mic: capture exited before producing audio")
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("mic: no audio after %s (check microphone permission for this terminal)", timeout)
}

// timeOf converts a detector sample index to wall time using the closest
// preceding anchor. Callers hold m.mu.
func (m *mic) timeOf(sample int64) time.Time {
	i := sort.Search(len(m.anchors), func(i int) bool { return m.anchors[i].sample > sample }) - 1
	if i < 0 {
		i = 0
	}
	a := m.anchors[i]
	return a.at.Add(time.Duration(sample-a.sample) * time.Second / time.Duration(m.rate))
}

// heardBetween returns onsets whose wall time falls in [from, to).
func (m *mic) heardBetween(from, to time.Time) []heard {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []heard
	if len(m.anchors) == 0 {
		return nil
	}
	for _, o := range m.onsets {
		at := m.timeOf(o.sample)
		if !at.Before(from) && at.Before(to) {
			out = append(out, heard{at: at, tone: o.tone, snrDB: o.snrDB})
		}
	}
	return out
}
