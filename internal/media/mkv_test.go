package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestMKVDecodes writes AAC frames into the live Matroska format, leaving a
// 0.5 s gap, and checks ffmpeg reads it with the gap filled: the decoded
// length must equal the timeline, not the number of frames.
func TestMKVDecodes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	adts, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=2:sample_rate=44100", "-ac", "2",
		"-c:a", "aac", "-b:a", "256k", "-f", "adts", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	checkGapFilled(t, Track{CodecID: "A_AAC", CodecPrivate: aacConfig(44100, 2), SampleRate: 44100, Channels: 2},
		splitADTS(t, adts), 1024)
}

// TestMKVDecodesALAC does the same for ALAC, with the codec config rebuilt
// the way the receiver does (AirPlay never sends it). ffmpeg's encoder only
// makes 4096-sample frames, not AirPlay's 352, but the config is the same.
func TestMKVDecodesALAC(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	frames := encodeALAC(t, 4)
	frames = frames[:len(frames)-1] // the last one is partial
	checkGapFilled(t, Track{CodecID: "A_ALAC", CodecPrivate: alacConfig(4096, 16, 2, 44100), SampleRate: 44100, Channels: 2, BitDepth: 16},
		frames, 4096)
}

// alacConfig mirrors airplay.Format.ALACCookie()[12:].
func alacConfig(spf, depth, channels, rate int) []byte {
	b := make([]byte, 24)
	binary.BigEndian.PutUint32(b[0:], uint32(spf))
	b[5] = byte(depth)
	b[6], b[7], b[8] = 40, 10, 14
	b[9] = byte(channels)
	binary.BigEndian.PutUint16(b[10:], 255)
	binary.BigEndian.PutUint32(b[20:], uint32(rate))
	return b
}

// encodeALAC returns raw ALAC frames (4096 samples, 16-bit stereo 44.1 kHz)
// of a sine, sliced out of an .m4a using ffprobe's packet offsets.
func encodeALAC(t *testing.T, seconds int) [][]byte {
	path := t.TempDir() + "/x.m4a"
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration="+strconv.Itoa(seconds)+":sample_rate=44100", "-ac", "2",
		"-c:a", "alac", "-sample_fmt", "s16p", path).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "packet=pos,size", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Packets []struct{ Pos, Size string }
	}
	if err := json.Unmarshal(probe, &p); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	for _, pk := range p.Packets {
		pos, _ := strconv.Atoi(pk.Pos)
		size, _ := strconv.Atoi(pk.Size)
		frames = append(frames, file[pos:pos+size])
	}
	return frames
}

// checkGapFilled muxes frames with ~0.5 s missing and checks ffmpeg decodes
// exactly the timeline's length.
func checkGapFilled(t *testing.T, track Track, frames [][]byte, spf int) {
	t.Helper()
	var w mkvWriter
	var b bytes.Buffer
	b.Write(w.header(track))
	n := 0
	skip := track.SampleRate / 2 / spf
	for i, f := range frames {
		if i >= 5 && i < 5+skip {
			continue
		}
		b.Write(w.block(int64(i*spf)*1_000_000/int64(track.SampleRate), f))
		n++
	}
	path := os.Getenv("MKV_OUT")
	if path == "" {
		path = t.TempDir() + "/x.mkv"
	}
	os.WriteFile(path, b.Bytes(), 0o644)
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "matroska", "-i", path,
		"-af", "aresample=48000:async=1:min_hard_comp=0.001:first_pts=0", "-f", "s16le", "-ac", "1", "pipe:1").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, strings.TrimSpace(string(out)))
	}
	got := float64(len(out)/2) / 48000
	want := float64(len(frames)*spf) / float64(track.SampleRate)
	t.Logf("wrote %d of %d frames; decoded %.3f s, timeline %.3f s", n, len(frames), got, want)
	if got < want-0.05 || got > want+0.05 {
		t.Errorf("gap not filled: decoded %.3f s, want %.3f s", got, want)
	}
}
