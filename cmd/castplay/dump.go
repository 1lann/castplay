package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"castplay/internal/airplay"
	"castplay/internal/media"
	"castplay/internal/ptp"
)

// dumpPlayer writes each stream's audio to a file instead of a speaker:
// AAC as .aac (ADTS), ALAC as a .caf-less raw dump with frame sizes. It is
// for checking the AirPlay side on its own (-dump).
type dumpPlayer struct {
	dir string
	log *slog.Logger
	ptp *ptp.Follower // may be nil

	mu      sync.Mutex
	f       *os.File
	format  airplay.Format
	packets int
	first   time.Time
}

func (d *dumpPlayer) StreamStarted(s airplay.StreamInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
	ext := "raw"
	if s.Format.Codec == airplay.CodecAAC {
		ext = "aac"
	}
	name := filepath.Join(d.dir, fmt.Sprintf("airplay-%s-%d.%s", time.Now().Format("150405"), s.Type, ext))
	f, err := os.Create(name)
	if err != nil {
		d.log.Error("dump", "err", err)
		return
	}
	d.f, d.format, d.packets = f, s.Format, 0
	d.log.Info("dumping stream", "file", name, "format", s.Format.String(), "type", s.Type, "sender", s.Sender, "timing", s.TimingProtocol)
}

func (d *dumpPlayer) Packet(p airplay.Packet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return
	}
	if d.packets == 0 {
		d.first = time.Now()
	}
	d.packets++
	if d.packets%500 == 1 {
		d.log.Info("audio", "packets", d.packets, "seq", p.Seq, "ts", p.Timestamp, "bytes", len(p.Payload))
	}
	switch d.format.Codec {
	case airplay.CodecAAC:
		d.f.Write(media.ADTSHeader(len(p.Payload), d.format.SampleRate, d.format.Channels))
		d.f.Write(p.Payload)
	default:
		binary.Write(d.f, binary.BigEndian, uint32(len(p.Payload)))
		d.f.Write(p.Payload)
	}
}

func (d *dumpPlayer) Anchor(a airplay.Anchor) {
	args := []any{"rate", a.Rate, "rtpTime", a.RTPTime, "clock", fmt.Sprintf("%016x", a.NetworkTime.ClockID)}
	if d.ptp != nil && !a.NetworkTime.IsZero() {
		if at, ok := d.ptp.LocalTime(a.NetworkTime.ClockID, a.NetworkTime.Nanos()); ok {
			args = append(args, "playsIn", time.Until(at).Round(time.Millisecond))
		} else {
			args = append(args, "playsIn", "unknown (no PTP sync)")
		}
	}
	d.log.Info("anchor", args...)
}

func (d *dumpPlayer) Flush(f airplay.Flush) {
	d.log.Info("flush", "fromSeq", f.FromSeq, "untilSeq", f.UntilSeq, "untilTS", f.UntilTS)
}

func (d *dumpPlayer) Volume(db float64) { d.log.Info("volume", "dB", db) }

func (d *dumpPlayer) TimingPeers(addrs []string) {
	d.log.Info("timing peers", "addrs", addrs)
	if d.ptp != nil {
		d.ptp.SetPeers(addrs)
	}
}

func (d *dumpPlayer) StreamStopped() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
}

func (d *dumpPlayer) closeLocked() {
	if d.f != nil {
		d.log.Info("stream stopped", "packets", d.packets, "file", d.f.Name())
		d.f.Close()
		d.f = nil
	}
}
