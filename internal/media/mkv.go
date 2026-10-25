package media

import (
	"bytes"
	"encoding/binary"
	"math"
)

// A minimal live Matroska writer: one audio track, an unknown-size Segment
// and Clusters, and timestamped SimpleBlocks. It lets ffmpeg decode any
// AirPlay payload (ALAC needs its codec cookie, which no raw stream format
// carries) and see each frame's timestamp, so gaps from lost packets come
// out as silence instead of shifting everything after them.

// Track describes the audio being muxed.
type Track struct {
	CodecID      string // "A_AAC", "A_ALAC", "A_PCM/INT/BIG"
	CodecPrivate []byte
	SampleRate   int
	Channels     int
	BitDepth     int
}

const (
	idEBML           = 0x1A45DFA3
	idEBMLVersion    = 0x4286
	idEBMLReadVer    = 0x42F7
	idEBMLMaxIDLen   = 0x42F2
	idEBMLMaxSizeLen = 0x42F3
	idDocType        = 0x4282
	idDocTypeVer     = 0x4287
	idDocTypeReadVer = 0x4285
	idSegment        = 0x18538067
	idInfo           = 0x1549A966
	idTimecodeScale  = 0x2AD7B1
	idMuxingApp      = 0x4D80
	idWritingApp     = 0x5741
	idTracks         = 0x1654AE6B
	idTrackEntry     = 0xAE
	idTrackNumber    = 0xD7
	idTrackUID       = 0x73C5
	idTrackType      = 0x83
	idCodecID        = 0x86
	idCodecPrivate   = 0x63A2
	idAudio          = 0xE1
	idSamplingFreq   = 0xB5
	idChannels       = 0x9F
	idBitDepth       = 0x6264
	idCluster        = 0x1F43B675
	idTimecode       = 0xE7
	idSimpleBlock    = 0xA3

	// Timestamps are in microseconds for sample-accurate gaps. A block's
	// timecode is a signed 16-bit offset from its cluster's, so clusters can
	// span only ~32 ms; a new one every frame or two costs ~20 bytes.
	timecodeScale  = 1000
	clusterSpanMax = 32_000
)

func ebmlID(b *bytes.Buffer, id uint32) {
	switch {
	case id >= 1<<24:
		b.Write([]byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)})
	case id >= 1<<16:
		b.Write([]byte{byte(id >> 16), byte(id >> 8), byte(id)})
	case id >= 1<<8:
		b.Write([]byte{byte(id >> 8), byte(id)})
	default:
		b.WriteByte(byte(id))
	}
}

// ebmlSize writes an 8-byte size (simple and always valid).
func ebmlSize(b *bytes.Buffer, n uint64) {
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], n|1<<56)
	b.Write(s[:])
}

var unknownSize = []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

func ebmlElem(b *bytes.Buffer, id uint32, body []byte) {
	ebmlID(b, id)
	ebmlSize(b, uint64(len(body)))
	b.Write(body)
}

func ebmlUint(b *bytes.Buffer, id uint32, v uint64) {
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], v)
	ebmlElem(b, id, s[:])
}

func ebmlFloat(b *bytes.Buffer, id uint32, v float64) {
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], math.Float64bits(v))
	ebmlElem(b, id, s[:])
}

func ebmlString(b *bytes.Buffer, id uint32, s string) { ebmlElem(b, id, []byte(s)) }

func nested(fill func(*bytes.Buffer)) []byte {
	var b bytes.Buffer
	fill(&b)
	return b.Bytes()
}

// mkvWriter produces the byte stream; each method returns what to write.
type mkvWriter struct {
	clusterStart int64
	inCluster    bool
}

func (w *mkvWriter) header(t Track) []byte {
	var b bytes.Buffer
	ebmlElem(&b, idEBML, nested(func(b *bytes.Buffer) {
		ebmlUint(b, idEBMLVersion, 1)
		ebmlUint(b, idEBMLReadVer, 1)
		ebmlUint(b, idEBMLMaxIDLen, 4)
		ebmlUint(b, idEBMLMaxSizeLen, 8)
		ebmlString(b, idDocType, "matroska")
		ebmlUint(b, idDocTypeVer, 4)
		ebmlUint(b, idDocTypeReadVer, 2)
	}))
	ebmlID(&b, idSegment)
	b.Write(unknownSize)
	ebmlElem(&b, idInfo, nested(func(b *bytes.Buffer) {
		ebmlUint(b, idTimecodeScale, timecodeScale)
		ebmlString(b, idMuxingApp, "castplay")
		ebmlString(b, idWritingApp, "castplay")
	}))
	ebmlElem(&b, idTracks, nested(func(b *bytes.Buffer) {
		ebmlElem(b, idTrackEntry, nested(func(b *bytes.Buffer) {
			ebmlUint(b, idTrackNumber, 1)
			ebmlUint(b, idTrackUID, 1)
			ebmlUint(b, idTrackType, 2) // audio
			ebmlString(b, idCodecID, t.CodecID)
			if len(t.CodecPrivate) > 0 {
				ebmlElem(b, idCodecPrivate, t.CodecPrivate)
			}
			ebmlElem(b, idAudio, nested(func(b *bytes.Buffer) {
				ebmlFloat(b, idSamplingFreq, float64(t.SampleRate))
				ebmlUint(b, idChannels, uint64(t.Channels))
				if t.BitDepth > 0 {
					ebmlUint(b, idBitDepth, uint64(t.BitDepth))
				}
			}))
		}))
	}))
	return b.Bytes()
}

// block wraps one frame with presentation time pts (µs), opening a new
// cluster when needed.
func (w *mkvWriter) block(pts int64, frame []byte) []byte {
	var b bytes.Buffer
	if !w.inCluster || pts-w.clusterStart >= clusterSpanMax || pts < w.clusterStart {
		ebmlID(&b, idCluster)
		b.Write(unknownSize)
		ebmlUint(&b, idTimecode, uint64(max(pts, 0)))
		w.clusterStart, w.inCluster = max(pts, 0), true
	}
	ebmlID(&b, idSimpleBlock)
	ebmlSize(&b, uint64(4+len(frame)))
	rel := int16(pts - w.clusterStart)
	b.Write([]byte{0x81, byte(uint16(rel) >> 8), byte(rel), 0x80}) // track 1, keyframe
	b.Write(frame)
	return b.Bytes()
}

// aacConfig returns the 2-byte AudioSpecificConfig for AAC-LC.
func aacConfig(sampleRate, channels int) []byte {
	idx := 4
	for i, r := range adtsRates {
		if r == sampleRate {
			idx = i
		}
	}
	return []byte{byte(2<<3 | idx>>1), byte(idx&1<<7 | channels<<3)}
}
