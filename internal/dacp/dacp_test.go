package dacp

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func item(tag string, v []byte) []byte {
	b := append([]byte(tag), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[4:], uint32(len(v)))
	return append(b, v...)
}

func u32(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }

func cat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func speakerItem(name string, id uint64, vol uint32) []byte {
	return item("mdcl", cat(item("minm", []byte(name)), item("msma", binary.BigEndian.AppendUint64(nil, id)),
		item("cmvo", u32(vol)), item("caia", []byte{1})))
}

// TestSetVolume checks shairport's algorithm against a fake sender with
// overall volume 80 and another speaker at 100% of it (so 80 absolute).
func TestSetVolume(t *testing.T) {
	const us = 0xAABBCCDDEEFF
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Active-Remote") != "1234" {
			t.Errorf("missing Active-Remote")
		}
		cmd := strings.TrimPrefix(r.URL.String(), "/ctrl-int/1/")
		switch {
		case strings.HasPrefix(cmd, "getproperty"):
			w.Write(item("cmgt", item("cmvo", u32(80))))
		case cmd == "getspeakers":
			w.Write(item("casp", cat(speakerItem("Mac", 1, 100), speakerItem("Living Room speaker", us, 50))))
		default:
			mu.Lock()
			got = append(got, cmd)
			mu.Unlock()
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	r := &Remote{ID: "X", ActiveRemote: "1234", Speaker: SpeakerNumber("AA:BB:CC:DD:EE:FF"), Log: slog.New(slog.DiscardHandler),
		addr: strings.TrimPrefix(srv.URL, "http://")}
	if r.Speaker != us {
		t.Fatalf("speaker number %x", r.Speaker)
	}
	for _, c := range []struct {
		vo   int
		want []string
	}{
		{90, []string{"setproperty?include-speaker-id=187723572702975&dmcp.volume=90"}},
		{40, []string{"setproperty?speaker-id=187723572702975&dmcp.volume=50"}},
	} {
		got = nil
		if err := r.SetVolume(context.Background(), c.vo); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("vo %d: sent %q, want %q", c.vo, got, c.want)
		}
	}
}
