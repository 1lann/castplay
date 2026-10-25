package airplay

import (
	"encoding/binary"
	"fmt"
)

// Codec is the payload of an audio stream.
type Codec string

const (
	CodecPCM    Codec = "pcm"
	CodecALAC   Codec = "alac"
	CodecAAC    Codec = "aac"
	CodecAACELD Codec = "aac-eld"
	CodecOpus   Codec = "opus"
)

// Format describes an audio stream as negotiated in SETUP.
type Format struct {
	Codec           Codec
	SampleRate      int
	BitDepth        int
	Channels        int
	SamplesPerFrame int // "spf"
}

func (f Format) String() string {
	return fmt.Sprintf("%s %d Hz %d-bit %dch, %d samples/frame", f.Codec, f.SampleRate, f.BitDepth, f.Channels, f.SamplesPerFrame)
}

// audioFormats maps the SETUP "audioFormat" bit to a format (codec, rate,
// depth, channels). From airplay2-receiver's AirplayAudFmt.
var audioFormats = map[uint]Format{
	2: {CodecPCM, 8000, 16, 1, 0}, 3: {CodecPCM, 8000, 16, 2, 0},
	4: {CodecPCM, 16000, 16, 1, 0}, 5: {CodecPCM, 16000, 16, 2, 0},
	6: {CodecPCM, 24000, 16, 1, 0}, 7: {CodecPCM, 24000, 16, 2, 0},
	8: {CodecPCM, 32000, 16, 1, 0}, 9: {CodecPCM, 32000, 16, 2, 0},
	10: {CodecPCM, 44100, 16, 1, 0}, 11: {CodecPCM, 44100, 16, 2, 0},
	12: {CodecPCM, 44100, 24, 1, 0}, 13: {CodecPCM, 44100, 24, 2, 0},
	14: {CodecPCM, 48000, 16, 1, 0}, 15: {CodecPCM, 48000, 16, 2, 0},
	16: {CodecPCM, 48000, 24, 1, 0}, 17: {CodecPCM, 48000, 24, 2, 0},
	18: {CodecALAC, 44100, 16, 2, 0}, 19: {CodecALAC, 44100, 24, 2, 0},
	20: {CodecALAC, 48000, 16, 2, 0}, 21: {CodecALAC, 48000, 24, 2, 0},
	22: {CodecAAC, 44100, 16, 2, 0}, 23: {CodecAAC, 48000, 16, 2, 0},
	24: {CodecAACELD, 44100, 16, 2, 0}, 25: {CodecAACELD, 48000, 16, 2, 0},
	26: {CodecAACELD, 16000, 16, 1, 0}, 27: {CodecAACELD, 24000, 16, 1, 0},
	28: {CodecOpus, 16000, 16, 1, 0}, 29: {CodecOpus, 24000, 16, 1, 0},
	30: {CodecOpus, 48000, 16, 1, 0},
	31: {CodecAACELD, 44100, 16, 1, 0}, 32: {CodecAACELD, 48000, 16, 1, 0},
}

func parseFormat(audioFormat uint64, spf int) (Format, error) {
	for bit := uint(0); bit < 64; bit++ {
		if audioFormat == 1<<bit {
			if f, ok := audioFormats[bit]; ok {
				f.SamplesPerFrame = spf
				return f, nil
			}
		}
	}
	return Format{}, fmt.Errorf("airplay: unknown audioFormat %#x", audioFormat)
}

// ALACCookie is the 36-byte 'alac' atom decoders take as extradata; AirPlay
// doesn't transmit it, so it is rebuilt from the negotiated format.
func (f Format) ALACCookie() []byte {
	b := make([]byte, 36)
	binary.BigEndian.PutUint32(b[0:], 36)
	copy(b[4:], "alac")
	binary.BigEndian.PutUint32(b[12:], uint32(f.SamplesPerFrame))
	b[17] = byte(f.BitDepth)
	b[18], b[19], b[20] = 40, 10, 14 // history mult, initial history, rice limit
	b[21] = byte(f.Channels)
	binary.BigEndian.PutUint16(b[22:], 255)
	binary.BigEndian.PutUint32(b[32:], uint32(f.SampleRate))
	return b
}
