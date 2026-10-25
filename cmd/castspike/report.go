package main

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"castplay/internal/cast"
)

// match is a heard beep paired with the beep that produced it.
type match struct {
	k       int64
	due     time.Time // when the beep's first sample was due on the live stream
	heard   time.Time
	latency time.Duration
	snrDB   float64
}

type result struct {
	codec    string
	err      error
	t0       time.Time
	lead     time.Duration
	loadSent time.Time
	loadAck  time.Time
	stopped  time.Time
	conns    []connInfo
	events   []statusSample // unsolicited and solicited MEDIA_STATUS
	status   []statusSample // our 1 Hz polls
	maxLate  time.Duration
	mic      bool
	heard    []heard

	streaming bool // Cast Streaming run
	target    time.Duration
	rt        *cast.StreamingStats
	rxDelays  []time.Duration // receiver-reported playout delay, ~1 Hz

	firstStart int64
	matches    []match
	unmatched  int

	firstReq, firstPlaying, firstSound time.Duration
	latMed, latP10, latP90, latMin     time.Duration
	driftMsPerMin                      float64
	statusEst                          time.Duration
	statusOK                           bool
	expected                           int
}

// due returns the wall time signal sample p was scheduled onto the stream.
func (r *result) due(p int64) time.Time { return r.t0.Add(samplesToDur(p) - r.lead) }

func (r *result) analyse() {
	r.firstStart = -1
	for _, c := range r.conns {
		if c.firstStart >= 0 && r.firstReq == 0 && !r.streaming {
			r.firstReq = c.at.Sub(r.loadSent)
		}
		if c.firstStart >= 0 {
			r.firstStart = c.firstStart // last connection that got audio
		}
	}
	for _, e := range r.events {
		if e.state == "PLAYING" {
			r.firstPlaying = e.at.Sub(r.loadSent)
			break
		}
	}

	if r.firstStart >= 0 {
		// Beeps whose start was on the stream before we stopped.
		for k := int64(0); ; k++ {
			p := beepStart(k)
			if p < r.firstStart {
				continue
			}
			if !r.due(p).Before(r.stopped) {
				break
			}
			r.expected++
		}
	}

	// Pair each heard onset with the latest beep of the same tone that was
	// due before it and was actually served.
	for _, h := range r.heard {
		var best *match
		for k := int64(h.tone); ; k += numTones {
			p := beepStart(k)
			d := r.due(p)
			if d.After(h.at.Add(-10 * time.Millisecond)) {
				break
			}
			if r.firstStart >= 0 && p < r.firstStart {
				continue
			}
			best = &match{k: k, due: d, heard: h.at, latency: h.at.Sub(d), snrDB: h.snrDB}
		}
		if best == nil {
			r.unmatched++
			continue
		}
		r.matches = append(r.matches, *best)
	}
	if len(r.matches) > 0 {
		m := r.matches[0]
		// When would the first served sample have been heard?
		r.firstSound = m.heard.Add(-samplesToDur(beepStart(m.k) - r.firstStart)).Sub(r.loadSent)
		lats := make([]time.Duration, len(r.matches))
		for i, m := range r.matches {
			lats[i] = m.latency
		}
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		r.latMin = lats[0]
		r.latMed = percentile(lats, 0.5)
		r.latP10 = percentile(lats, 0.1)
		r.latP90 = percentile(lats, 0.9)
		r.driftMsPerMin = slope(r.matches)
	}

	// MEDIA_STATUS-based estimate: playhead = firstStart + currentTime.
	var ests []time.Duration
	for _, s := range r.status {
		if s.state != "PLAYING" || r.firstStart < 0 {
			continue
		}
		p := r.firstStart + int64(s.ct*sampleRate)
		ests = append(ests, s.at.Sub(r.due(p)))
	}
	if len(ests) > 0 {
		sort.Slice(ests, func(i, j int) bool { return ests[i] < ests[j] })
		r.statusEst = percentile(ests, 0.5)
		r.statusOK = true
	}
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	i := int(math.Round(q * float64(len(sorted)-1)))
	return sorted[i]
}

// slope fits latency against elapsed time; a non-zero slope means the
// receiver's clock runs at a different rate from ours.
func slope(ms []match) float64 {
	if len(ms) < 5 {
		return math.NaN()
	}
	var sx, sy, sxx, sxy float64
	t0 := ms[0].due
	for _, m := range ms {
		x := m.due.Sub(t0).Minutes()
		y := float64(m.latency) / float64(time.Millisecond)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	n := float64(len(ms))
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN()
	}
	return (n*sxy - sx*sy) / den
}

func ms(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", d.Milliseconds())
}

func (r *result) printDetail() {
	if r.codec == "loopback" {
		r.printHeard()
		return
	}
	if r.streaming {
		fmt.Printf("   negotiated in %s ms; frames %d, packets %d, retransmits %d, feedback %d, receiver delay %s\n",
			ms(r.loadAck.Sub(r.loadSent)), r.rt.Frames, r.rt.Packets, r.rt.Retransmits, r.rt.Feedback, r.rxDelayRange())
		if r.maxLate > 5*time.Millisecond {
			fmt.Printf("   warning: pacing ran up to %s late\n", r.maxLate.Round(time.Millisecond))
		}
		r.printHeard()
		return
	}
	fmt.Printf("   LOAD ack after %s ms; first HTTP request %s ms; first PLAYING %s ms\n",
		ms(r.loadAck.Sub(r.loadSent)), ms(r.firstReq), ms(r.firstPlaying))
	for _, c := range r.conns {
		fmt.Printf("   http %s at +%d ms range=%q bytes=%d ua=%q\n", c.method, c.at.Sub(r.loadSent).Milliseconds(), c.rangeHdr, c.bytes, c.userAgent)
	}
	var st []string
	for _, s := range r.status {
		st = append(st, fmt.Sprintf("%.0fs:%s/%.1f", s.at.Sub(r.loadSent).Seconds(), strings.ToLower(s.state[:min(4, len(s.state))]), s.ct))
	}
	if len(st) > 0 {
		fmt.Printf("   status (t:state/currentTime): %s\n", strings.Join(st, " "))
	}
	if r.maxLate > 5*time.Millisecond {
		fmt.Printf("   warning: pacing ran up to %s late\n", r.maxLate.Round(time.Millisecond))
	}
	r.printHeard()
}

func (r *result) printHeard() {
	if !r.mic {
		return
	}
	fmt.Printf("   beeps heard %d/%d (unmatched onsets: %d)\n", len(r.matches), r.expected, r.unmatched)
	if len(r.matches) == 0 {
		fmt.Println("   no beeps detected: is the speaker audible from this mic? try -volume 0.4")
		return
	}
	var lines []string
	for _, m := range r.matches {
		lines = append(lines, fmt.Sprintf("%d", m.latency.Milliseconds()))
	}
	fmt.Printf("   per-beep latency ms: %s\n", strings.Join(lines, " "))
}

func (r *result) rxDelayRange() string {
	if len(r.rxDelays) == 0 {
		return "-"
	}
	lo, hi := r.rxDelays[0], r.rxDelays[0]
	for _, d := range r.rxDelays {
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo == hi {
		return fmt.Sprintf("%d", lo.Milliseconds())
	}
	return fmt.Sprintf("%d-%d", lo.Milliseconds(), hi.Milliseconds())
}

func printSummary(o options, dev cast.Device, rs []result) {
	fmt.Printf("\n== summary: %s (%s), app %s, streamType %s, http %s, lead %s, burst %s, noise %v dBFS\n",
		dev.Name, dev.Model, o.appID, o.streamType, o.httpLen, o.lead, o.burst, o.noiseDB)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "run\tload ack\t1st req\tplaying\t1st sound\tlat med\tp10\tp90\tmin\tdrift ms/min\tstatus est\trx delay\tretx\theard\t")
	for _, r := range rs {
		if r.err != nil {
			fmt.Fprintf(w, "%s\tfailed: %v\t\n", r.codec, r.err)
			continue
		}
		drift, est, heard := "-", "-", "-"
		if !math.IsNaN(r.driftMsPerMin) && len(r.matches) > 0 {
			drift = fmt.Sprintf("%+.1f", r.driftMsPerMin)
		}
		if r.statusOK {
			est = ms(r.statusEst)
		}
		if r.mic {
			heard = fmt.Sprintf("%d/%d", len(r.matches), r.expected)
		}
		retx := "-"
		if r.rt != nil {
			retx = fmt.Sprint(r.rt.Retransmits)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", r.codec,
			ms(r.loadAck.Sub(r.loadSent)), ms(r.firstReq), ms(r.firstPlaying), ms(r.firstSound),
			ms(r.latMed), ms(r.latP10), ms(r.latP90), ms(r.latMin), drift, est, r.rxDelayRange(), retx, heard)
	}
	w.Flush()
	fmt.Println(`
All times in ms. load ack / 1st req / playing / 1st sound are measured from sending LOAD.
lat = time from a sample going out on the live stream to it being heard (acoustic).
status est = the same latency inferred from MEDIA_STATUS.currentTime, for comparison (HTTP runs).
rt:<ms> runs: load ack = OFFER/ANSWER time, lat is measured from the audio's capture time on the
RTP timeline, rx delay = playout delay the receiver reports, retx = packets retransmitted.`)
}

func (r *result) writeCSV(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := filepath.Join(dir, fmt.Sprintf("%s-%s.csv", r.loadSent.Format("20060102-150405"), r.codec))
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"beep", "due_s", "heard_s", "latency_ms", "snr_db"})
	for _, m := range r.matches {
		w.Write([]string{
			fmt.Sprint(m.k),
			fmt.Sprintf("%.4f", m.due.Sub(r.loadSent).Seconds()),
			fmt.Sprintf("%.4f", m.heard.Sub(r.loadSent).Seconds()),
			fmt.Sprintf("%.1f", float64(m.latency)/float64(time.Millisecond)),
			fmt.Sprintf("%.1f", m.snrDB),
		})
	}
	w.Flush()
	fmt.Printf("   wrote %s\n", name)
	return w.Error()
}
