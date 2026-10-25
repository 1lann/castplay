package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os/exec"
)

// chunk is one encoded unit of the live stream. start is the signal-timeline
// sample its decoded audio begins at (codec priming already accounted for),
// so pacing and latency maths never need to know which codec is in use.
type chunk struct {
	start int64
	n     int
	data  []byte
}

type source interface {
	contentType() string
	ext() string
	// header is sent to every new HTTP client before the first chunk.
	header() []byte
	next() chunk
}

func newSource(codec string, o options) (source, error) {
	switch codec {
	case "wav":
		return &wavSource{frames: 352}, nil // one AirPlay realtime packet
	case "flac":
		if o.flacBlock < 16 || o.flacBlock > 65535 {
			return nil, fmt.Errorf("flac block size %d out of range", o.flacBlock)
		}
		return &flacSource{block: o.flacBlock}, nil
	case "aac":
		return newAACSource(o)
	}
	return nil, fmt.Errorf("unknown codec %q", codec)
}

// --- WAV ---------------------------------------------------------------------

type wavSource struct {
	frames int
	pos    int64
}

func (*wavSource) contentType() string { return "audio/wav" }
func (*wavSource) ext() string         { return "wav" }

// header uses maximal RIFF/data sizes, the usual trick for an endless WAV.
func (*wavSource) header() []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(0xFFFFFFFF))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(channels))
	w(uint32(sampleRate))
	w(uint32(sampleRate * channels * 2))
	w(uint16(channels * 2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(0xFFFFFFFF - 36))
	return b.Bytes()
}

func (s *wavSource) next() chunk {
	data, _ := pcmS16(s.pos, s.frames)
	c := chunk{start: s.pos, n: s.frames, data: data}
	s.pos += int64(s.frames)
	return c
}

// --- FLAC --------------------------------------------------------------------

// flacSource writes fixed-blocksize FLAC frames using only CONSTANT (silent
// blocks) and VERBATIM subframes. Both are byte aligned, so no bit writer is
// needed, and the decoder-side behaviour (frame parsing, buffering) is the same
// as for a compressed stream.
type flacSource struct {
	block int
	pos   int64
	frame uint64
}

func (*flacSource) contentType() string { return "audio/flac" }
func (*flacSource) ext() string         { return "flac" }

func (s *flacSource) header() []byte {
	b := []byte("fLaC")
	b = append(b, 0x80, 0, 0, 34) // last metadata block, STREAMINFO, 34 bytes
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], uint16(s.block))
	binary.BigEndian.PutUint16(si[2:], uint16(s.block))
	// min/max frame size (si[4:10]) unknown; total samples and MD5 left zero.
	// 20 bits sample rate | 3 bits channels-1 | 5 bits bps-1 | 36 bits total.
	packed := uint64(sampleRate)<<44 | uint64(channels-1)<<41 | uint64(16-1)<<36
	binary.BigEndian.PutUint64(si[10:], packed)
	return append(b, si...)
}

func (s *flacSource) next() chunk {
	pcm, silent := pcmS16(s.pos, s.block)
	f := s.frameHeader()
	for c := 0; c < channels; c++ {
		if silent {
			f = append(f, 0x00, 0, 0) // CONSTANT subframe, value 0
			continue
		}
		f = append(f, 0x02) // VERBATIM subframe
		for j := 0; j < s.block; j++ {
			o := (j*channels + c) * 2
			f = append(f, pcm[o+1], pcm[o]) // big endian
		}
	}
	f = binary.BigEndian.AppendUint16(f, crc16(f))
	c := chunk{start: s.pos, n: s.block, data: f}
	s.pos += int64(s.block)
	s.frame++
	return c
}

func (s *flacSource) frameHeader() []byte {
	h := []byte{0xFF, 0xF8} // sync, fixed blocksize
	code, extra := flacBlockCode(s.block)
	h = append(h, code<<4|0x9)   // 0x9 = 44.1 kHz
	h = append(h, 0x1<<4|0x4<<1) // 2 independent channels, 16 bit
	h = append(h, utf8Num(s.frame)...)
	h = append(h, extra...)
	return append(h, crc8(h))
}

func flacBlockCode(n int) (byte, []byte) {
	switch n {
	case 192:
		return 0x1, nil
	case 576, 1152, 2304, 4608:
		return byte(0x2 + log2(n/576)), nil
	case 256, 512, 1024, 2048, 4096, 8192, 16384, 32768:
		return byte(0x8 + log2(n/256)), nil
	}
	if n <= 256 {
		return 0x6, []byte{byte(n - 1)}
	}
	return 0x7, binary.BigEndian.AppendUint16(nil, uint16(n-1))
}

func log2(n int) int {
	i := 0
	for n > 1 {
		n >>= 1
		i++
	}
	return i
}

// utf8Num encodes a FLAC frame number with the extended UTF-8 scheme.
func utf8Num(v uint64) []byte {
	if v < 0x80 {
		return []byte{byte(v)}
	}
	var n int // continuation bytes
	switch {
	case v < 0x800:
		n = 1
	case v < 0x10000:
		n = 2
	case v < 0x200000:
		n = 3
	case v < 0x4000000:
		n = 4
	default:
		n = 5
	}
	out := make([]byte, n+1)
	for i := n; i > 0; i-- {
		out[i] = 0x80 | byte(v&0x3F)
		v >>= 6
	}
	out[0] = byte(0xFF<<(7-n)) | byte(v)
	return out
}

func crc8(b []byte) byte {
	var c byte
	for _, x := range b {
		c ^= x
		for i := 0; i < 8; i++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func crc16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c ^= uint16(x) << 8
		for i := 0; i < 8; i++ {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// --- AAC ---------------------------------------------------------------------

// aacSource pre-encodes the whole run with ffmpeg and then hands out ADTS
// frames one at a time. Pacing already-encoded frames is exactly what the real
// bridge will do with AirPlay's buffered AAC, so no live encoder sits in the
// measured path.
type aacSource struct {
	frames [][]byte
	i      int
	delay  int64 // encoder priming in samples
}

func (*aacSource) contentType() string { return "audio/aac" }
func (*aacSource) ext() string         { return "aac" }
func (*aacSource) header() []byte      { return nil }

func newAACSource(o options) (*aacSource, error) {
	total := int64((o.duration+o.burst+o.lead).Seconds()*sampleRate) + 10*sampleRate
	delay := int64(1024) // ffmpeg's native encoder
	if o.aacEncoder == "aac_at" {
		delay = 2112 // AudioToolbox
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", fmt.Sprint(channels), "-i", "pipe:0",
		"-c:a", o.aacEncoder, "-b:a", o.aacBitrate, "-f", "adts", "pipe:1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("aac: start ffmpeg: %w", err)
	}
	go func() {
		defer stdin.Close()
		const step = sampleRate
		for pos := int64(0); pos < total; pos += step {
			buf, _ := pcmS16(pos, step)
			if _, err := stdin.Write(buf); err != nil {
				return
			}
		}
	}()
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("aac: ffmpeg: %w: %s", err, stderr.String())
	}
	frames, err := splitADTS(out.Bytes())
	if err != nil {
		return nil, err
	}
	return &aacSource{frames: frames, delay: delay}, nil
}

func splitADTS(b []byte) ([][]byte, error) {
	var frames [][]byte
	for len(b) > 0 {
		if len(b) < 7 || b[0] != 0xFF || b[1]&0xF0 != 0xF0 {
			return nil, fmt.Errorf("aac: lost ADTS sync after %d frames", len(frames))
		}
		if b[6]&0x03 != 0 {
			return nil, fmt.Errorf("aac: multi-block ADTS frames not supported")
		}
		n := int(b[3]&0x03)<<11 | int(b[4])<<3 | int(b[5])>>5
		if n < 7 || n > len(b) {
			return nil, fmt.Errorf("aac: bad ADTS frame length %d", n)
		}
		frames = append(frames, b[:n:n])
		b = b[n:]
	}
	return frames, nil
}

func (s *aacSource) next() chunk {
	// Past the pre-encoded material (margin exhausted), keep the timeline
	// moving by repeating the final frame.
	data := s.frames[min(s.i, len(s.frames)-1)]
	c := chunk{start: int64(s.i)*1024 - s.delay, n: 1024, data: data}
	s.i++
	return c
}
