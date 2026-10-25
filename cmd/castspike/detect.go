package main

import "math"

// onset is a detected beep: the input sample index where it was first heard
// and which tone it was (beep index mod numTones).
type onset struct {
	sample int64
	tone   int
	snrDB  float64
}

// detector finds beep onsets in a mono s16 stream using a bank of Goertzel
// filters over Hann-windowed 10 ms frames with a 2.5 ms hop. The onset time is
// the centre of the first frame whose tone power clears the adaptive noise
// floor, so it carries a small, constant bias that -selftest measures.
type detector struct {
	sr      int
	win     int
	hop     int
	hann    []float64
	coef    [numTones]float64
	floor   [numTones]float64
	primed  int
	buf     []float64
	bufBase int64 // input sample index of buf[0]

	// pending onset being identified over the following frames
	pending    bool
	pendStart  int64
	pendFrames int
	pendPower  [numTones]float64
	lastOnset  int64
	prevOnset  int64

	out []onset
}

const (
	detThresholdDB = 15
	detConfirmDB   = 10    // mean SNR required over the identification window
	detMinAmp      = 0.002 // absolute floor (fraction of full scale)
	detRefractory  = 0.4   // seconds between onsets
	detIdentFrames = 8     // frames (20 ms) used to identify the tone
)

func newDetector(sr int) *detector {
	d := &detector{sr: sr, win: sr / 100, hop: sr / 400, lastOnset: math.MinInt64 / 2, prevOnset: math.MinInt64 / 2}
	d.hann = make([]float64, d.win)
	for i := range d.hann {
		d.hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(d.win-1))
	}
	for t := 0; t < numTones; t++ {
		d.coef[t] = 2 * math.Cos(2*math.Pi*toneFreq(t)/float64(sr))
	}
	return d
}

// feed consumes little-endian mono s16 samples and returns any onsets that
// were completed.
func (d *detector) feed(pcm []byte) []onset {
	for i := 0; i+1 < len(pcm); i += 2 {
		d.buf = append(d.buf, float64(int16(uint16(pcm[i])|uint16(pcm[i+1])<<8))/32768)
	}
	d.out = d.out[:0]
	for len(d.buf) >= d.win {
		d.frame(d.buf[:d.win], d.bufBase)
		d.buf = d.buf[d.hop:]
		d.bufBase += int64(d.hop)
	}
	// Compact occasionally so the slice doesn't grow without bound.
	if cap(d.buf) > 1<<16 {
		d.buf = append([]float64(nil), d.buf...)
	}
	return d.out
}

func (d *detector) frame(x []float64, base int64) {
	var p [numTones]float64
	for t := 0; t < numTones; t++ {
		var s1, s2 float64
		for i, v := range x {
			s := v*d.hann[i] + d.coef[t]*s1 - s2
			s2, s1 = s1, s
		}
		// Normalise so a full-scale sine reads ~1 (Hann coherent gain 0.5).
		mag := math.Sqrt(s1*s1+s2*s2-d.coef[t]*s1*s2) / (float64(d.win) * 0.25)
		p[t] = mag * mag
	}
	centre := base + int64(d.win/2)

	if d.pending {
		for t := range p {
			d.pendPower[t] += p[t]
		}
		d.pendFrames++
		if d.pendFrames >= detIdentFrames {
			best, second := 0, -1
			for t := 1; t < numTones; t++ {
				if d.pendPower[t] > d.pendPower[best] {
					second, best = best, t
				} else if second < 0 || d.pendPower[t] > d.pendPower[second] {
					second = t
				}
			}
			snr := 10 * math.Log10(d.pendPower[best]/float64(d.pendFrames)/math.Max(d.floor[best], 1e-12))
			d.pending = false
			// A noise spike can clear the threshold for one frame; a real beep
			// stays well above the floor for the whole identification window.
			if snr < detConfirmDB {
				d.lastOnset = d.prevOnset
				return
			}
			d.out = append(d.out, onset{sample: d.pendStart, tone: best, snrDB: snr})
		}
		return
	}

	// Prime the noise floor with the first 200 ms before detecting.
	if d.primed < d.sr/5/d.hop {
		for t := range p {
			d.floor[t] += (p[t] - d.floor[t]) / float64(d.primed+1)
		}
		d.primed++
		return
	}

	hit := -1
	minP := detMinAmp * detMinAmp
	for t := range p {
		if p[t] > minP && p[t] > d.floor[t]*math.Pow(10, detThresholdDB/10) {
			if hit < 0 || p[t] > p[hit] {
				hit = t
			}
		}
	}
	if hit >= 0 && centre-d.lastOnset > int64(detRefractory*float64(d.sr)) {
		d.pending = true
		d.prevOnset = d.lastOnset
		d.pendStart = centre
		d.pendFrames = 1
		d.pendPower = p
		d.lastOnset = centre
		return
	}
	if hit < 0 && centre-d.lastOnset > int64(detRefractory*float64(d.sr)) {
		// Track the noise floor only away from beeps and their reverb tails.
		for t := range p {
			d.floor[t] += (p[t] - d.floor[t]) * 0.02
		}
	}
}
