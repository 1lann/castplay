package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// TestOpusTimeline checks that libopus output, decoded with pre-skip applied,
// keeps beeps at their signal positions after resampling to 48 kHz. Frame j
// then starts at output sample j*N-preskip, which runStreaming relies on.
func TestOpusTimeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	for _, n := range []int{240, 480, 960} {
		frames, preskip, err := opusFrames(6*time.Second, n, "128k")
		if err != nil {
			t.Fatal(err)
		}
		if len(frames) < 5*48000/n || preskip <= 0 || preskip > 960 {
			t.Fatalf("N=%d: %d frames, preskip %d", n, len(frames), preskip)
		}
		// Re-encode identically to Ogg and decode it, as a receiver would.
		cmd := exec.Command("sh", "-c", fmt.Sprintf(
			"ffmpeg -hide_banner -loglevel error -f s16le -ar 44100 -ac 2 -i pipe:0 -ar 48000 -c:a libopus -application lowdelay -frame_duration %g -b:a 128k -vbr off -f ogg pipe:1 | ffmpeg -hide_banner -loglevel error -i pipe:0 -ac 1 -ar 48000 -f s16le pipe:1",
			float64(n)/48))
		pcm, _ := pcmS16(0, 6*sampleRate)
		cmd.Stdin = bytes.NewReader(pcm)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		det := newDetector(48000)
		for _, on := range det.feed(out) {
			k := (on.sample*sampleRate/48000+beepPeriod/2)/beepPeriod - 1
			want := beepStart(k) * 48000 / sampleRate
			errMS := float64(on.sample-want) / 48
			if errMS < -4 || errMS > 4 || int(k%numTones) != on.tone {
				t.Errorf("N=%d beep %d: onset off by %.1f ms (tone %d)", n, k, errMS, on.tone)
			}
		}
		t.Logf("N=%d: %d frames, preskip %d samples", n, len(frames), preskip)
	}
}
