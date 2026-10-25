package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// loopback checks the microphone path on its own: it plays the test signal on
// this machine's speaker with afplay and measures it like a Cast run. The
// absolute latency includes afplay start-up, but the spacing must be exact:
// drift should be ~0 ms/min and p90-p10 a few ms. Anything else means the mic
// timestamps can't be trusted.
func loopback(ctx context.Context, o options, log *slog.Logger) error {
	if o.micInput == "" {
		return fmt.Errorf("loopback needs -mic")
	}
	secs := int(o.duration.Seconds())
	n := int64(secs) * sampleRate
	pcm, _ := pcmS16(0, int(n))
	hdr := (&wavSource{}).header()
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(pcm)))
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))
	path := filepath.Join(os.TempDir(), "castspike-loopback.wav")
	if err := os.WriteFile(path, append(hdr, pcm...), 0o644); err != nil {
		return err
	}
	defer os.Remove(path)

	m, err := startMic(ctx, o.micInput, log)
	if err != nil {
		return err
	}
	if err := m.waitReady(5 * time.Second); err != nil {
		return err
	}
	r := result{codec: "loopback", t0: time.Now()}
	r.loadSent = r.t0
	if err := exec.CommandContext(ctx, "afplay", path).Run(); err != nil {
		return fmt.Errorf("afplay: %w", err)
	}
	r.stopped = time.Now()
	time.Sleep(300 * time.Millisecond)
	r.conns = []connInfo{{at: r.t0, firstStart: 0}}
	r.mic = true
	r.heard = m.heardBetween(r.t0, r.stopped.Add(time.Second))
	r.analyse()
	r.printDetail()
	if len(r.matches) > 0 {
		fmt.Printf("   latency median %d ms, p10..p90 %d..%d ms, drift %+.1f ms/min\n",
			r.latMed.Milliseconds(), r.latP10.Milliseconds(), r.latP90.Milliseconds(), r.driftMsPerMin)
	}
	return nil
}
