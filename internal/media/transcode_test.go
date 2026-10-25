package media

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func splitADTS(t *testing.T, b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 7 {
		n := int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5])>>5
		out = append(out, b[7:n]) // raw access unit, as AirPlay sends it
		b = b[n:]
	}
	return out
}

// TestTranscoderLatency feeds AAC frames at real-time pace and measures how
// long each Opus packet takes to come out after the input that completes it.
func TestTranscoderLatency(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	adts, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=3:sample_rate=44100", "-ac", "2",
		"-c:a", "aac", "-b:a", "256k", "-f", "adts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	frames := splitADTS(t, adts)
	tc, err := StartOpus(context.Background(), Input{Codec: "aac", SampleRate: 44100, Channels: 2}, 128, 960)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	start := time.Now()
	frameDur := time.Second * 1024 / 44100
	go func() {
		for i, f := range frames {
			time.Sleep(time.Until(start.Add(time.Duration(i) * frameDur)))
			if err := tc.WriteFrame(int64(i)*1024, f); err != nil {
				return
			}
		}
	}()
	var worst, total time.Duration
	n := 0
	for n < 100 {
		if _, err := tc.ReadPacket(); err != nil {
			t.Fatal(err)
		}
		// Packet n ends at 48 kHz sample (n+1)*960-preskip; the input frame
		// holding that sample was written at that point in time.
		endSample := float64((n+1)*960-tc.PreSkip) / 48000
		lat := time.Since(start) - time.Duration(endSample*float64(time.Second))
		if n > 5 {
			worst = max(worst, lat)
			total += lat
		}
		n++
	}
	avg := total / time.Duration(n-6)
	t.Logf("preskip %d, transcode latency avg %s, worst %s", tc.PreSkip, avg.Round(time.Millisecond), worst.Round(time.Millisecond))
	if avg > 150*time.Millisecond {
		t.Errorf("transcoder too slow: avg latency %s", avg)
	}
}
