package airplay

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"castplay/internal/airplay/hap"
)

// Remote-control relay probe (experimental, -rc-relay). With status flag
// RemoteControlRelay set, iOS may open a type 130 data stream to fetch now
// playing over MediaRemote, as it does with HomePods. This only logs what
// the sender asks for: it accepts the stream, decrypts the first frames and
// never replies.

const (
	streamRemoteControl      = 130
	statusRemoteControlRelay = 1 << 11
)

// probeRemoteControl answers a type 130 stream descriptor.
func (s *session) probeRemoteControl(desc map[string]any) (map[string]any, error) {
	keys := make([]string, 0, len(desc))
	for k, v := range desc {
		keys = append(keys, fmt.Sprintf("%s=%v", k, summarize(v)))
	}
	sort.Strings(keys)
	s.log.Info("remote control stream requested", "desc", keys)

	seed, hasSeed := pUint(desc, "seed")
	s.r.mu.Lock()
	s.r.nextID++
	id := s.r.nextID
	s.r.mu.Unlock()
	if !hasSeed {
		// No seed: the sender uses the shared (control) connection, and the
		// reply has no data port (as shairport-sync).
		return map[string]any{"type": streamRemoteControl, "streamID": id}, nil
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	go func() {
		defer ln.Close()
		ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Minute))
		c, err := ln.Accept()
		if err != nil {
			s.log.Info("remote control stream: sender never connected", "err", err)
			return
		}
		defer c.Close()
		s.log.Info("remote control stream connected", "from", c.RemoteAddr())
		s.probeDataStream(c, seed)
	}()
	return map[string]any{"type": streamRemoteControl, "streamID": id, "dataPort": ln.Addr().(*net.TCPAddr).Port}, nil
}

func (s *session) probeDataStream(c net.Conn, seed uint64) {
	shared := s.pair.SharedKey
	salts := []string{"DataStream-Salt" + strconv.FormatUint(seed, 10)}
	if signed := strconv.FormatInt(int64(seed), 10); signed != strconv.FormatUint(seed, 10) {
		salts = append(salts, "DataStream-Salt"+signed)
	}
	infos := []string{"DataStream-Output-Encryption-Key", "DataStream-Input-Encryption-Key"}
	salt, info := "", ""
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	for n := uint64(0); n < 8; n++ {
		var hdr [2]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			s.log.Info("remote control stream closed", "blocks", n, "err", err)
			return
		}
		sealed := make([]byte, int(binary.LittleEndian.Uint16(hdr[:]))+16)
		if _, err := io.ReadFull(c, sealed); err != nil {
			s.log.Info("remote control stream closed", "blocks", n, "err", err)
			return
		}
		var plain []byte
		var err error
		if salt == "" {
		search:
			for _, sa := range salts {
				for _, in := range infos {
					if plain, err = hap.OpenBlock(shared, sa, in, n, hdr[:], sealed); err == nil {
						salt, info = sa, in
						s.log.Info("remote control stream key found", "salt", sa, "info", in)
						break search
					}
				}
			}
			if salt == "" {
				s.log.Warn("remote control stream: no candidate key decrypts", "len", len(sealed)-16,
					"head", fmt.Sprintf("%x", sealed[:min(32, len(sealed))]))
				return
			}
		} else if plain, err = hap.OpenBlock(shared, salt, info, n, hdr[:], sealed); err != nil {
			s.log.Warn("remote control stream decrypt", "block", n, "err", err)
			return
		}
		s.log.Info("remote control frame", "block", n, "len", len(plain),
			"hex", fmt.Sprintf("%x", plain[:min(96, len(plain))]), "text", printable(plain[:min(400, len(plain))]))
	}
}

// logCommand logs a POST /command body from the sender (probe only).
func (s *session) logCommand(req *request) {
	body, err := req.plistBody()
	if err != nil {
		s.log.Info("sender command", "len", len(req.Body), "text", printable(req.Body[:min(300, len(req.Body))]))
		return
	}
	keys := make([]string, 0, len(body))
	for k, v := range body {
		keys = append(keys, fmt.Sprintf("%s=%v", k, summarize(v)))
	}
	sort.Strings(keys)
	s.log.Info("sender command", "type", body["type"], "body", keys)
}

func summarize(v any) string {
	switch v := v.(type) {
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(v))
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k, x := range v {
			keys = append(keys, k+":"+summarize(x))
		}
		sort.Strings(keys)
		return fmt.Sprintf("{%s}", strings.Join(keys, " "))
	case []any:
		parts := make([]string, 0, len(v))
		for _, x := range v {
			parts = append(parts, summarize(x))
		}
		return fmt.Sprintf("[%s]", strings.Join(parts, " "))
	}
	return fmt.Sprint(v)
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}
