package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// Transcoder turns AirPlay audio frames into Opus packets with an ffmpeg
// subprocess: decode, resample to 48 kHz, encode with libopus in low-delay
// mode, one packet per frame, flushed as soon as each is ready. Frames go in
// as timestamped Matroska blocks, so a missing frame becomes silence of the
// right length and the output timeline always matches the input's.
type Transcoder struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *strings.Builder
	mkv    mkvWriter

	sampleRate   int
	frameSamples int // Opus frame at 48 kHz
	headers      int

	// PreSkip is the encoder delay in 48 kHz samples, known after the first
	// ReadPacket.
	PreSkip int
}

// Input describes what WriteFrame receives.
type Input struct {
	Codec      string // "aac" (raw AAC-LC access units), "alac" or "pcm" (s16be)
	SampleRate int
	Channels   int
	BitDepth   int
	// ALACConfig is the 24-byte ALACSpecificConfig (the 'alac' atom body).
	ALACConfig []byte
}

func (in Input) track() (Track, error) {
	t := Track{SampleRate: in.SampleRate, Channels: in.Channels}
	switch in.Codec {
	case "aac":
		t.CodecID, t.CodecPrivate = "A_AAC", aacConfig(in.SampleRate, in.Channels)
	case "alac":
		if len(in.ALACConfig) != 24 {
			return t, errors.New("transcode: ALAC needs its 24-byte config")
		}
		t.CodecID, t.CodecPrivate, t.BitDepth = "A_ALAC", in.ALACConfig, in.BitDepth
	case "pcm":
		t.CodecID, t.BitDepth = "A_PCM/INT/BIG", max(in.BitDepth, 16)
	default:
		return t, fmt.Errorf("transcode: unsupported input codec %q", in.Codec)
	}
	return t, nil
}

func StartOpus(ctx context.Context, in Input, bitrateKbps, frameSamples int) (*Transcoder, error) {
	track, err := in.track()
	if err != nil {
		return nil, err
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0",
		"-f", "matroska", "-i", "pipe:0",
		// Fill timestamp gaps over 1 ms with silence (and drop overlaps) so
		// output sample n is always input time n.
		"-af", "aresample=48000:async=1:min_hard_comp=0.001:first_pts=0", "-ac", "2",
		"-c:a", "libopus", "-application", "lowdelay",
		"-frame_duration", strconv.FormatFloat(float64(frameSamples)/48, 'f', -1, 64),
		"-b:a", strconv.Itoa(bitrateKbps) + "k", "-vbr", "off",
		"-f", "ogg", "-page_duration", strconv.Itoa(frameSamples * 1000 / 48), "-flush_packets", "1", "pipe:1"}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t := &Transcoder{cmd: cmd, in: stdin, out: bufio.NewReader(stdout), stderr: &strings.Builder{},
		sampleRate: in.SampleRate, frameSamples: frameSamples}
	cmd.Stderr = t.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transcode: start ffmpeg: %w", err)
	}
	if _, err := stdin.Write(t.mkv.header(track)); err != nil {
		t.Close()
		return nil, t.wrap(err)
	}
	return t, nil
}

// WriteFrame feeds one AirPlay audio frame that starts at input sample pos
// (counted from the first frame written, which must be at 0).
func (t *Transcoder) WriteFrame(pos int64, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	pts := pos * 1_000_000 / int64(t.sampleRate)
	_, err := t.in.Write(t.mkv.block(pts, payload))
	return t.wrap(err)
}

// ReadPacket returns the next Opus packet. Ogg pages are parsed directly;
// the two header packets (OpusHead, OpusTags) are consumed internally.
func (t *Transcoder) ReadPacket() ([]byte, error) {
	var pending []byte
	for {
		var h [27]byte
		if _, err := io.ReadFull(t.out, h[:]); err != nil {
			return nil, t.wrap(err)
		}
		if string(h[:4]) != "OggS" {
			return nil, errors.New("transcode: lost Ogg sync")
		}
		sizes := make([]byte, h[26])
		if _, err := io.ReadFull(t.out, sizes); err != nil {
			return nil, t.wrap(err)
		}
		// A page can hold several packets; keep any extra ones for later by
		// reading the page fully and splitting it.
		var packets [][]byte
		for _, size := range sizes {
			start := len(pending)
			pending = append(pending, make([]byte, size)...)
			if _, err := io.ReadFull(t.out, pending[start:]); err != nil {
				return nil, t.wrap(err)
			}
			if size < 255 {
				packets = append(packets, pending)
				pending = nil
			}
		}
		for _, p := range packets {
			switch t.headers {
			case 0:
				if len(p) < 19 || string(p[:8]) != "OpusHead" {
					return nil, errors.New("transcode: missing OpusHead")
				}
				t.PreSkip = int(binary.LittleEndian.Uint16(p[10:12]))
				t.headers++
				continue
			case 1:
				t.headers++
				continue
			}
			if len(packets) > 1 {
				// ffmpeg flushes one packet per page in this configuration;
				// anything else would need a queue.
				return nil, errors.New("transcode: multiple packets per Ogg page")
			}
			return p, nil
		}
	}
}

func (t *Transcoder) wrap(err error) error {
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(t.stderr.String()); msg != "" {
		return fmt.Errorf("%w (ffmpeg: %s)", err, msg)
	}
	return err
}

// Close stops ffmpeg without waiting for it to drain.
func (t *Transcoder) Close() {
	t.in.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	t.cmd.Wait()
}
