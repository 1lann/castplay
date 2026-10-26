package mrp

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSplitFrame(t *testing.T) {
	d := DeviceInfo{UniqueIdentifier: "ABC", Name: "Kitchen", ModelName: "Google Home Max"}
	frame := Frame(deviceInfoMessage("id-1", d), genericMessage("id-2"))
	msgs, err := Split(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[0].Type != TypeDeviceInfo || msgs[0].Identifier != "id-1" {
		t.Fatalf("first: type %d id %q", msgs[0].Type, msgs[0].Identifier)
	}
	if pi := parseDeviceInfo(msgs[0]); pi.Name != "Kitchen" {
		t.Fatalf("name %q", pi.Name)
	}
	if msgs[1].Type != TypeGeneric || msgs[1].Identifier != "id-2" {
		t.Fatalf("second: type %d id %q", msgs[1].Type, msgs[1].Identifier)
	}
}

func TestSetState(t *testing.T) {
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	np := NowPlaying{
		App: App{BundleID: "com.spotify.client", Name: "Spotify", PID: 7}, State: StatePlaying,
		ItemID: "t1", Title: "Song", Artist: "Artist", Album: "Album",
		Duration: 200, Elapsed: 12.5, At: at, Rate: 1, ArtworkURL: "http://x/a.jpg",
		Commands: []Command{CommandPlay, CommandPause},
	}
	msgs, err := Split(Frame(setState("", np, &Artwork{Data: []byte{1, 2, 3}, Width: 3, Height: 1})))
	if err != nil {
		t.Fatal(err)
	}
	st := msgs[0].Ext(extSetState)
	if PlaybackState(st.uint(6)) != StatePlaying {
		t.Fatalf("state %d", st.uint(6))
	}
	item := st.msg(3).msg(2)
	md := item.msg(2)
	if md.str(1) != "Song" || md.str(7) != "Artist" || md.str(6) != "Album" {
		t.Fatalf("metadata %v", md)
	}
	if md.double(35) != 12.5 || md.double(14) != 200 || md.float(39) != 1 {
		t.Fatalf("times %v %v %v", md.double(35), md.double(14), md.float(39))
	}
	if got := md.double(74); got != at.Sub(cocoaEpoch).Seconds() {
		t.Fatalf("timestamp %v", got)
	}
	if string(item[3][0].([]byte)) != "\x01\x02\x03" {
		t.Fatal("artwork data")
	}
	if n := len(st.msg(2)[1]); n != 2 {
		t.Fatalf("%d commands", n)
	}
	if st.msg(9).msg(2).str(2) != "com.spotify.client" {
		t.Fatal("player path")
	}
}

type fakeChannel struct {
	mu   sync.Mutex
	msgs []*Message
	got  chan struct{}
}

func (f *fakeChannel) Send(_ context.Context, frame []byte) error {
	msgs, err := Split(frame)
	f.mu.Lock()
	f.msgs = append(f.msgs, msgs...)
	f.mu.Unlock()
	f.got <- struct{}{}
	return err
}

func (f *fakeChannel) String() string { return "fake" }

func (f *fakeChannel) wait(t *testing.T, n int) []*Message {
	t.Helper()
	for {
		f.mu.Lock()
		if len(f.msgs) >= n {
			defer f.mu.Unlock()
			return f.msgs
		}
		f.mu.Unlock()
		select {
		case <-f.got:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %d messages", n)
		}
	}
}

func TestServerHandshake(t *testing.T) {
	s := NewServer(Config{Device: DeviceInfo{Name: "Kitchen"}, Log: testLogger(t)})
	s.SetNowPlaying(NowPlaying{App: App{BundleID: "com.spotify.client"}, State: StatePaused, ItemID: "x"})
	ch := &fakeChannel{got: make(chan struct{}, 16)}
	s.Open(ch)
	defer s.Close(ch)

	s.Data(ch, Frame(protocolMessage(TypeDeviceInfo, "hello", extDeviceInfo, enc(nil).str(2, "iPhone"))))
	s.Data(ch, Frame(protocolMessage(TypeClientUpdatesConfig, "cfg", extClientUpdatesConfig, enc(nil).bool(2, true))))
	msgs := ch.wait(t, 4)
	want := []struct {
		typ int
		id  string
	}{{TypeDeviceInfo, "hello"}, {TypeGeneric, "cfg"}, {TypeSetNowPlayingClient, ""}, {TypeSetState, ""}}
	for i, w := range want {
		if msgs[i].Type != w.typ || msgs[i].Identifier != w.id {
			t.Fatalf("message %d: type %d id %q, want %d %q", i, msgs[i].Type, msgs[i].Identifier, w.typ, w.id)
		}
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}
