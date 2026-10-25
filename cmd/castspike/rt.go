package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"castplay/internal/cast"
)

// Cast Streaming ("rt:<ms>" codecs): Opus over encrypted RTP to the built-in
// audio mirroring receiver, with an explicit receiver target delay. This is
// shoutout's transport; see internal/cast/streaming.go.

// rtDelay parses "rt:<ms>" and reports whether codec is a streaming run.
func rtDelay(codec string) (time.Duration, bool, error) {
	ms, ok := strings.CutPrefix(codec, "rt:")
	if !ok {
		return 0, false, nil
	}
	n, err := strconv.Atoi(ms)
	if err != nil || n < 10 || n > 5000 {
		return 0, true, fmt.Errorf("bad receiver target in %q (want rt:<10..5000 ms>)", codec)
	}
	return time.Duration(n) * time.Millisecond, true, nil
}

// rtFrameSamples follows shoutout: shorter Opus frames for tighter targets.
func rtFrameSamples(delay time.Duration) int {
	switch {
	case delay < 40*time.Millisecond:
		return 240 // 5 ms
	case delay < 80*time.Millisecond:
		return 480 // 10 ms
	}
	return 960 // 20 ms
}

// opusFrames pre-encodes the signal (resampled to 48 kHz) with libopus and
// returns the raw packets plus the encoder pre-skip in 48 kHz samples.
func opusFrames(total time.Duration, frameSamples int, bitrate string) ([][]byte, int, error) {
	n := int64(total.Seconds() * sampleRate)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", fmt.Sprint(channels), "-i", "pipe:0",
		"-ar", "48000", "-c:a", "libopus", "-application", "lowdelay",
		"-frame_duration", strconv.FormatFloat(float64(frameSamples)/48, 'f', -1, 64),
		"-b:a", bitrate, "-vbr", "off", "-f", "ogg", "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, 0, err
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	go func() {
		defer stdin.Close()
		for pos := int64(0); pos < n; pos += sampleRate {
			buf, _ := pcmS16(pos, sampleRate)
			if _, err := stdin.Write(buf); err != nil {
				return
			}
		}
	}()
	if err := cmd.Wait(); err != nil {
		return nil, 0, fmt.Errorf("opus: ffmpeg: %w: %s", err, stderr.String())
	}
	return readOgg(&out, frameSamples)
}

// readOgg extracts Opus packets from an Ogg stream (after shoutout's readOpus).
func readOgg(r io.Reader, frameSamples int) ([][]byte, int, error) {
	var packets [][]byte
	var pending []byte
	preskip, headers := 0, 0
	for {
		var h [27]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if err == io.EOF {
				return packets, preskip, nil
			}
			return nil, 0, err
		}
		if string(h[:4]) != "OggS" {
			return nil, 0, errors.New("opus: bad Ogg page")
		}
		sizes := make([]byte, h[26])
		if _, err := io.ReadFull(r, sizes); err != nil {
			return nil, 0, err
		}
		for _, size := range sizes {
			start := len(pending)
			pending = append(pending, make([]byte, size)...)
			if _, err := io.ReadFull(r, pending[start:]); err != nil {
				return nil, 0, err
			}
			if size == 255 {
				continue
			}
			switch headers {
			case 0:
				if len(pending) < 19 || string(pending[:8]) != "OpusHead" {
					return nil, 0, errors.New("opus: missing OpusHead")
				}
				preskip = int(binary.LittleEndian.Uint16(pending[10:12]))
			case 1:
			default:
				if opusSamples(pending) != frameSamples {
					return nil, 0, fmt.Errorf("opus: unexpected frame of %d samples", opusSamples(pending))
				}
				packets = append(packets, append([]byte(nil), pending...))
			}
			headers++
			pending = pending[:0]
		}
	}
}

func opusSamples(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	config := p[0] >> 3
	var samples int
	switch {
	case config < 12:
		samples = []int{480, 960, 1920, 2880}[config&3]
	case config < 16:
		samples = []int{480, 960}[config&1]
	default:
		samples = []int{120, 240, 480, 960}[config&3]
	}
	switch p[0] & 3 {
	case 1, 2:
		return samples * 2
	case 3:
		if len(p) < 2 {
			return 0
		}
		return samples * int(p[1]&63)
	}
	return samples
}

// runStreaming plays one Cast Streaming run. Frame j carries 48 kHz output
// samples [j*N-preskip, (j+1)*N-preskip), so its RTP timestamp j*N maps to
// wall time t0 + (j*N-preskip)/48000: the "capture" time of that audio. Each
// frame is sent once fully captured, minus -lead. Latency is measured from
// capture time, so a receiver that honours sender timestamps shows the same
// latency whatever -lead is, while one that plays on arrival gets faster.
func runStreaming(ctx context.Context, c *cast.Client, dev cast.Device, delay time.Duration, o options, log *slog.Logger) (result, error) {
	r := result{codec: fmt.Sprintf("rt:%d", delay.Milliseconds()), streaming: true, target: delay}
	N := rtFrameSamples(delay)
	frames, preskip, err := opusFrames(o.duration+o.lead+10*time.Second, N, o.opusBitrate)
	if err != nil {
		return r, err
	}

	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = c.Launch(lctx, cast.AudioMirroringReceiver)
	cancel()
	if err != nil {
		return r, fmt.Errorf("launch mirroring receiver: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.StopApp(sctx)
		cancel()
	}()

	r.loadSent = time.Now()
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sess, err := c.NegotiateAudio(nctx, o.opusBitrateBits(), delay, N)
	cancel()
	if err != nil {
		return r, err
	}
	r.loadAck = time.Now()
	host, _, _ := net.SplitHostPort(dev.Addr)
	tx, err := cast.NewStreamingSender(host, sess)
	if err != nil {
		return r, err
	}
	defer tx.Close()
	rctx, stopRecv := context.WithCancel(ctx)
	defer stopRecv()
	go tx.Receive(rctx)

	frameDur := time.Duration(N) * time.Second / cast.StreamingRate
	t0 := time.Now().Add(100*time.Millisecond + frameDur) // first frame fully "captured" ~100 ms from now
	wallOf := func(q int64) time.Time {                   // 48 kHz output sample -> capture wall time
		return t0.Add(time.Duration(q) * time.Second / cast.StreamingRate)
	}
	rtpAt := func(at time.Time) uint32 {
		return uint32(int64(at.Sub(t0))*cast.StreamingRate/int64(time.Second) + int64(preskip))
	}
	end := r.loadSent.Add(o.duration)
	var lastReport time.Time
	var maxLate time.Duration
	var delays []time.Duration
	lastDelaySample := time.Now()
	for j := 0; ctx.Err() == nil && j < len(frames); j++ {
		capEnd := wallOf(int64((j+1)*N - preskip))
		due := capEnd.Add(-o.lead)
		if !due.Before(end) {
			break
		}
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		now := time.Now()
		if late := now.Sub(due); late > maxLate {
			maxLate = late
		}
		if now.Sub(lastReport) >= 250*time.Millisecond {
			if err := tx.SendReport(now, rtpAt(now)); err != nil {
				return r, err
			}
			lastReport = now
		}
		if err := tx.SendFrame(uint32(j*N), frames[j]); err != nil {
			return r, err
		}
		if now.Sub(lastDelaySample) >= time.Second {
			if st := tx.Stats(); st.Feedback > 0 {
				delays = append(delays, st.ReceiverDelay)
			}
			lastDelaySample = now
		}
	}
	r.stopped = time.Now()
	st := tx.Stats()
	r.rt = &st
	r.rxDelays = delays
	r.maxLate = maxLate
	// Signal timeline at 44.1 kHz: sample p is captured at t0 + p/44100.
	r.t0 = t0
	r.conns = []connInfo{{at: r.loadAck, firstStart: 0}}
	return r, nil
}

func (o options) opusBitrateBits() int {
	s := strings.TrimSuffix(strings.ToLower(o.opusBitrate), "k")
	n, _ := strconv.Atoi(s)
	return n * 1000
}
