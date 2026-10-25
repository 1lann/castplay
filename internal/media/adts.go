// Package media holds small container helpers for the transcoding pipeline.
package media

var adtsRates = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// ADTSHeader returns the 7-byte ADTS header (AAC-LC, no CRC) for one raw
// AAC access unit of n bytes, so AirPlay's raw frames can be fed to a
// decoder as a plain .aac stream.
func ADTSHeader(n, sampleRate, channels int) []byte {
	idx := 4 // 44.1 kHz
	for i, r := range adtsRates {
		if r == sampleRate {
			idx = i
		}
	}
	size := n + 7
	const profile = 1 // AAC-LC (object type 2) minus one
	return []byte{
		0xFF, 0xF1, // sync, MPEG-4, layer 0, no CRC
		byte(profile<<6 | idx<<2 | channels>>2),
		byte(channels&3<<6 | size>>11),
		byte(size >> 3),
		byte(size&7<<5 | 0x1F),
		0xFC, // buffer fullness 0x7FF, one raw data block
	}
}
