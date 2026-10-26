package airplay

import (
	"encoding/binary"
	"testing"
)

func dmapItem(tag string, v []byte) []byte {
	b := append([]byte(tag), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[4:], uint32(len(v)))
	return append(b, v...)
}

func TestParseMetadata(t *testing.T) {
	body := dmapItem("mlit", append(append(dmapItem("minm", []byte("Song")), dmapItem("asar", []byte("Artist"))...),
		dmapItem("asal", []byte("Album"))...))
	m, ok := parseMetadata("application/x-dmap-tagged", body)
	if !ok || m.Title != "Song" || m.Artist != "Artist" || m.Album != "Album" {
		t.Fatalf("text: %+v %v", m, ok)
	}
	m, ok = parseMetadata("text/parameters", []byte("progress: 100/44200/4410100\r\n"))
	if !ok || !m.HasProgress || m.Start != 100 || m.Current != 44200 || m.End != 4410100 {
		t.Fatalf("progress: %+v %v", m, ok)
	}
	if _, ok := parseMetadata("text/parameters", []byte("volume: -10.0\r\n")); ok {
		t.Fatal("volume parsed as metadata")
	}
}
