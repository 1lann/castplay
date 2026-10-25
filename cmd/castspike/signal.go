package main

import "math"

// The test signal is silence with a short tone burst every second. Beep k
// starts at sample sampleRate*(k+1) and uses tone k%numTones, so a heard beep
// identifies itself modulo numTones seconds. That keeps matching unambiguous
// for latencies up to ~16 s without relying on catching the first beep.
const (
	sampleRate = 44100
	channels   = 2

	beepPeriod = sampleRate // one beep per second
	beepLen    = sampleRate * 60 / 1000
	beepRamp   = sampleRate * 5 / 1000
	beepAmp    = 0.5
	numTones   = 16
	baseTone   = 1000.0
	toneStep   = 250.0
)

func toneFreq(i int) float64 { return baseTone + toneStep*float64(i) }

// beepStart is the signal-timeline sample at which beep k begins.
func beepStart(k int64) int64 { return beepPeriod * (k + 1) }

// noiseAmp is the level of a white-noise bed under the beeps. Digital silence
// compresses to almost nothing, which starves receivers that buffer by bytes;
// a noise bed gives every codec a realistic bitrate. Set from -noise.
var noiseAmp float64

// noise is deterministic in i so every codec renders the same signal.
func noise(i int64) float64 {
	x := uint64(i)*0x9E3779B97F4A7C15 + 0x632BE59BD9B4E019
	x ^= x >> 31
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 29
	return (float64(x>>11)/float64(1<<53)*2 - 1) * noiseAmp
}

// sampleAt returns the mono value of the signal at sample i, in [-1, 1].
func sampleAt(i int64) float64 {
	k := i/beepPeriod - 1
	if k < 0 {
		return noise(i)
	}
	off := i - beepStart(k)
	if off >= beepLen {
		return noise(i)
	}
	env := 1.0
	switch {
	case off < beepRamp:
		env = 0.5 - 0.5*math.Cos(math.Pi*float64(off)/beepRamp)
	case off >= beepLen-beepRamp:
		env = 0.5 - 0.5*math.Cos(math.Pi*float64(beepLen-off)/beepRamp)
	}
	f := toneFreq(int(k % numTones))
	return beepAmp*env*math.Sin(2*math.Pi*f*float64(off)/sampleRate) + noise(i)
}

// pcmS16 renders n stereo frames starting at sample start as interleaved
// little-endian s16 and reports whether the whole range is silent.
func pcmS16(start int64, n int) (buf []byte, silent bool) {
	buf = make([]byte, n*channels*2)
	silent = true
	for j := 0; j < n; j++ {
		v := int16(math.Round(sampleAt(start+int64(j)) * 32767))
		if v != 0 {
			silent = false
		}
		for c := 0; c < channels; c++ {
			o := (j*channels + c) * 2
			buf[o] = byte(v)
			buf[o+1] = byte(v >> 8)
		}
	}
	return buf, silent
}
