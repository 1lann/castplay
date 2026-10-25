package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// selftest plays the receiver's role with ffmpeg: it pulls each codec's live
// stream over HTTP, decodes it, and runs the beep detector on the result.
// Because the decoded audio starts at a known signal sample, every onset has
// an exact expected position, which checks the encoders, the codec delay
// bookkeeping, and the detector's own timing bias in one go.
func selftest(ctx context.Context, o options, log *slog.Logger) error {
	srv, err := startServer(0)
	if err != nil {
		return err
	}
	const listen = 6 * time.Second
	o.duration = listen + 4*time.Second
	failed := false
	for _, codec := range o.codecs {
		src, err := newSource(codec, o)
		if err != nil {
			return err
		}
		ls := newLiveStream(src, o, log)
		srv.cur.Store(ls)
		runCtx, stopRun := context.WithCancel(ctx)
		go ls.run(runCtx)

		url := fmt.Sprintf("http://127.0.0.1:%d%s", srv.port(), ls.path)
		cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
			"-i", url, "-t", fmt.Sprint(listen.Seconds()), "-ac", "1", "-ar", fmt.Sprint(sampleRate), "-f", "s16le", "pipe:1")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		began := time.Now()
		err = cmd.Run()
		took := time.Since(began)
		stopRun()
		if err != nil {
			fmt.Printf("%-5s FAIL: ffmpeg could not decode the stream: %v %s\n", codec, err, strings.TrimSpace(stderr.String()))
			failed = true
			continue
		}
		first, ok := ls.activeStart()
		if !ok {
			fmt.Printf("%-5s FAIL: no audio was served\n", codec)
			failed = true
			continue
		}

		det := newDetector(sampleRate)
		ons := det.feed(out.Bytes())
		var errs []float64
		wrongTone := 0
		for _, on := range ons {
			p := first + on.sample // signal sample where the onset was found
			k := (p+beepPeriod/2)/beepPeriod - 1
			if k < 0 {
				continue
			}
			if int(k%numTones) != on.tone {
				wrongTone++
			}
			errs = append(errs, float64(p-beepStart(k))*1000/sampleRate)
			log.Debug("selftest onset", "decodedSample", on.sample, "signalSample", p, "beep", k, "tone", on.tone, "snr", on.snrDB)
		}
		decoded := float64(out.Len()/2) / sampleRate
		status := "ok"
		var mean, spread float64
		if len(errs) == 0 {
			status = "FAIL: no beeps detected"
			failed = true
		} else {
			lo, hi := errs[0], errs[0]
			for _, e := range errs {
				mean += e
				lo, hi = min(lo, e), max(hi, e)
			}
			mean /= float64(len(errs))
			spread = hi - lo
			if wrongTone > 0 || spread > 6 || mean < -2 || mean > 15 {
				status = "FAIL"
				failed = true
			}
		}
		// A live stream decoded in real time should take about as long as
		// the audio it produced; much faster means pacing is broken.
		fmt.Printf("%-5s %s: decoded %.2fs of audio in %.2fs, %d beeps, onset bias %+.1f ms (spread %.1f ms), wrong tone %d\n",
			codec, status, decoded, took.Seconds(), len(errs), mean, spread, wrongTone)
	}
	if failed {
		return fmt.Errorf("selftest failed")
	}
	fmt.Println("\nonset bias is the detector's constant timing offset (it fires on a frame centre); mic measurements carry the same offset.")
	return nil
}
