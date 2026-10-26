package airplay

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// Metadata is what a sender says about the track it plays, sent with
// SET_PARAMETER: DMAP text, progress, or artwork. Each call carries one kind.
type Metadata struct {
	// Text: from an application/x-dmap-tagged body.
	HasText              bool
	Title, Artist, Album string

	// Progress: RTP timestamps of the track's start, the current position
	// and its end, at the stream's sample rate.
	HasProgress         bool
	Start, Current, End uint32

	// Artwork: an image/* body; empty data clears it.
	HasArtwork  bool
	Artwork     []byte
	ArtworkMIME string
}

// parseMetadata decodes a SET_PARAMETER body other than volume.
func parseMetadata(contentType string, body []byte) (Metadata, bool) {
	switch {
	case contentType == "application/x-dmap-tagged":
		m := Metadata{HasText: true}
		walkDMAP(body, func(tag string, v []byte) {
			switch tag {
			case "minm":
				m.Title = string(v)
			case "asar":
				m.Artist = string(v)
			case "asal":
				m.Album = string(v)
			}
		})
		return m, true
	case strings.HasPrefix(contentType, "image/"):
		return Metadata{HasArtwork: true, Artwork: body, ArtworkMIME: contentType}, true
	case strings.HasPrefix(contentType, "text/parameters"):
		for _, line := range strings.Split(string(body), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || strings.TrimSpace(k) != "progress" {
				continue
			}
			f := strings.Split(strings.TrimSpace(v), "/")
			if len(f) != 3 {
				return Metadata{}, false
			}
			var ts [3]uint32
			for i, s := range f {
				n, err := strconv.ParseUint(s, 10, 64)
				if err != nil {
					return Metadata{}, false
				}
				ts[i] = uint32(n)
			}
			return Metadata{HasProgress: true, Start: ts[0], Current: ts[1], End: ts[2]}, true
		}
	}
	return Metadata{}, false
}

// dmapContainers are the DMAP tags whose value is itself DMAP.
var dmapContainers = map[string]bool{"mlit": true, "mlcl": true, "cmst": true}

// walkDMAP calls f for every leaf item: a 4-byte tag, a 4-byte big-endian
// length, then the value.
func walkDMAP(b []byte, f func(tag string, v []byte)) {
	for len(b) >= 8 {
		tag := string(b[:4])
		n := binary.BigEndian.Uint32(b[4:8])
		if uint64(n) > uint64(len(b)-8) {
			return
		}
		v := b[8 : 8+n]
		if dmapContainers[tag] {
			walkDMAP(v, f)
		} else {
			f(tag, v)
		}
		b = b[8+n:]
	}
}
