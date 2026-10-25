package cast

// Cast Streaming (the mirroring protocol) for audio: OFFER/ANSWER negotiation
// plus an encrypted RTP/RTCP sender. Adapted from shoutout by Lars Karlslund
// (MIT, github.com/lkarlslund/shoutout internal/cast/{streaming,transport}.go).
//
// Unlike shoutout's transport, the caller owns pacing and the RTP timeline:
// SendFrame sends immediately, and SendReport publishes the RTP↔wall-clock
// mapping the receiver uses to schedule playout. That lets callers test whether
// the receiver honours sender timestamps or just plays on arrival.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// StreamingSession holds negotiated parameters for one encrypted audio
// stream. Key material must never be logged.
type StreamingSession struct {
	SenderSSRC, ReceiverSSRC uint32
	Port                     int
	Key, IV                  [16]byte
	TargetDelay              time.Duration
	FrameSamples             int // at 48 kHz
}

const StreamingRate = 48000

// NegotiateAudio offers one stereo 48 kHz Opus stream with the given bitrate
// (bits/s) and receiver target delay. The mirroring app must be launched.
func (c *Client) NegotiateAudio(ctx context.Context, bitrate int, delay time.Duration, frameSamples int) (StreamingSession, error) {
	s := StreamingSession{TargetDelay: delay, FrameSamples: frameSamples}
	tid, err := c.transport()
	if err != nil {
		return s, err
	}
	var random [36]byte
	if _, err := rand.Read(random[:]); err != nil {
		return s, err
	}
	// Older receivers reject SSRC values above the signed 32-bit range.
	s.SenderSSRC = (binary.BigEndian.Uint32(random[:4]) & 0x7fffffff) | 1
	copy(s.Key[:], random[4:20])
	copy(s.IV[:], random[20:])
	stream := map[string]any{
		"index": 0, "type": "audio_source", "codecName": "opus", "codecParameter": "",
		"sampleRate": StreamingRate, "rtpProfile": "cast", "rtpPayloadType": 127,
		"ssrc": s.SenderSSRC, "targetDelay": delay.Milliseconds(), "channels": 2,
		"bitRate": bitrate, "timeBase": "1/48000",
		"aesKey": hex.EncodeToString(s.Key[:]), "aesIvMask": hex.EncodeToString(s.IV[:]),
		"receiverRtcpEventLog": false,
	}
	r, err := c.request(ctx, tid, NSWebRTC, map[string]any{
		"type": "OFFER", "offer": map[string]any{"castMode": "mirroring", "supportedStreams": []any{stream}},
	})
	if err != nil {
		return s, err
	}
	var reply struct {
		Type, Result string
		Error        struct {
			Code        int
			Description string
		}
		Answer struct {
			UDPPort     int      `json:"udpPort"`
			SendIndexes []int    `json:"sendIndexes"`
			SSRCs       []uint32 `json:"ssrcs"`
		}
	}
	if err := json.Unmarshal(r.Payload, &reply); err != nil {
		return s, err
	}
	if reply.Type != "ANSWER" || reply.Result != "ok" {
		return s, fmt.Errorf("cast: audio offer rejected: %s (%d)", reply.Error.Description, reply.Error.Code)
	}
	a := reply.Answer
	if a.UDPPort < 1 || a.UDPPort > 65535 || len(a.SendIndexes) != 1 || a.SendIndexes[0] != 0 || len(a.SSRCs) != 1 {
		return s, errors.New("cast: invalid audio answer")
	}
	s.Port, s.ReceiverSSRC = a.UDPPort, a.SSRCs[0]
	return s, nil
}

// StreamingStats is what the receiver has told us so far.
type StreamingStats struct {
	Frames, Packets, Retransmits, Feedback uint64
	Acknowledged                           int64
	ReceiverDelay                          time.Duration // from Cast feedback
}

type sentFrame struct {
	packets         [][]byte
	sent, lastRetry time.Time
}

// StreamingSender sends encrypted Opus frames over UDP and services receiver
// feedback (acknowledgements and retransmission requests).
type StreamingSender struct {
	conn    *net.UDPConn
	session StreamingSession
	block   cipher.Block

	mu       sync.Mutex
	next     uint32 // next frame id
	sequence uint16
	octets   uint32
	cache    map[uint32]*sentFrame
	stats    StreamingStats
}

const packetPayload = 1100

func NewStreamingSender(remoteIP string, s StreamingSession) (*StreamingSender, error) {
	block, err := aes.NewCipher(s.Key[:])
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: s.Port})
	if err != nil {
		return nil, err
	}
	return &StreamingSender{conn: conn, session: s, block: block, cache: map[uint32]*sentFrame{},
		stats: StreamingStats{Acknowledged: -1}}, nil
}

func (t *StreamingSender) Close() error { return t.conn.Close() }

func (t *StreamingSender) Stats() StreamingStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stats
}

// SendFrame encrypts and sends the next frame with the given RTP timestamp.
func (t *StreamingSender) SendFrame(rtpTS uint32, opus []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	enc := frameCipher(t.block, t.session.IV, id, opus)
	packets := audioPackets(t.session.SenderSSRC, id, rtpTS, &t.sequence, enc)
	for _, p := range packets {
		if err := t.write(p, false); err != nil {
			return err
		}
	}
	t.cache[id] = &sentFrame{packets: packets, sent: time.Now()}
	t.next++
	t.stats.Frames++
	return nil
}

// SendReport sends an RTCP sender report stating that rtpTS corresponds to
// wall time at.
func (t *StreamingSender) SendReport(at time.Time, rtpTS uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := make([]byte, 28)
	p[0], p[1] = 0x80, 200
	binary.BigEndian.PutUint16(p[2:4], 6)
	binary.BigEndian.PutUint32(p[4:8], t.session.SenderSSRC)
	binary.BigEndian.PutUint32(p[8:12], uint32(at.Unix()+2208988800))
	binary.BigEndian.PutUint32(p[12:16], uint32((uint64(at.Nanosecond())<<32)/1e9))
	binary.BigEndian.PutUint32(p[16:20], rtpTS)
	binary.BigEndian.PutUint32(p[20:24], uint32(t.stats.Packets))
	binary.BigEndian.PutUint32(p[24:28], t.octets)
	_ = t.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := t.conn.Write(p)
	return err
}

func (t *StreamingSender) write(p []byte, retry bool) error {
	_ = t.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := t.conn.Write(p); err != nil {
		return err
	}
	t.stats.Packets++
	t.octets += uint32(len(p) - 19)
	if retry {
		t.stats.Retransmits++
	}
	return nil
}

func frameCipher(block cipher.Block, mask [16]byte, id uint32, plain []byte) []byte {
	var nonce [16]byte
	binary.BigEndian.PutUint32(nonce[8:12], id)
	for i := range nonce {
		nonce[i] ^= mask[i]
	}
	out := make([]byte, len(plain))
	cipher.NewCTR(block, nonce[:]).XORKeyStream(out, plain)
	return out
}

// audioPackets splits one encrypted frame into Cast RTP packets. Every audio
// frame is a key frame that references itself.
func audioPackets(ssrc, frame, rtpTS uint32, sequence *uint16, enc []byte) [][]byte {
	count := max(1, (len(enc)+packetPayload-1)/packetPayload)
	packets := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		n := min(packetPayload, len(enc)-i*packetPayload)
		p := make([]byte, 19+n)
		p[0] = 0x80
		p[1] = 127
		if i == count-1 {
			p[1] |= 0x80
		}
		binary.BigEndian.PutUint16(p[2:4], *sequence)
		*sequence++
		binary.BigEndian.PutUint32(p[4:8], rtpTS)
		binary.BigEndian.PutUint32(p[8:12], ssrc)
		p[12] = 0xc0 // key frame, reference id present
		p[13] = byte(frame)
		binary.BigEndian.PutUint16(p[14:16], uint16(i))
		binary.BigEndian.PutUint16(p[16:18], uint16(count-1))
		p[18] = byte(frame)
		copy(p[19:], enc[i*packetPayload:i*packetPayload+n])
		packets = append(packets, p)
	}
	return packets
}

// Receive services feedback until ctx is done or the socket fails.
func (t *StreamingSender) Receive(ctx context.Context) error {
	buf := make([]byte, 65536)
	for ctx.Err() == nil {
		_ = t.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := t.conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		for _, f := range parseFeedback(buf[:n], t.session.SenderSSRC, t.session.ReceiverSSRC) {
			t.feedback(f)
		}
	}
	return ctx.Err()
}

type packetLoss struct {
	frame     byte
	packet    uint16
	following byte
}

type castFeedback struct {
	checkpoint byte
	delayMS    int
	loss       []packetLoss
}

func parseFeedback(b []byte, senderSSRC, receiverSSRC uint32) []castFeedback {
	var out []castFeedback
	for len(b) >= 4 && b[0]>>6 == 2 {
		size := (int(binary.BigEndian.Uint16(b[2:4])) + 1) * 4
		if size > len(b) {
			return out
		}
		p := b[:size]
		b = b[size:]
		if p[0]&0x20 != 0 {
			pad := int(p[len(p)-1])
			if pad == 0 || pad > len(p)-4 {
				return out
			}
			p = p[:len(p)-pad]
		}
		// Payload-specific feedback (206), FMT 15, with the "CAST" identifier.
		if p[1] != 206 || p[0]&31 != 15 || len(p) < 20 {
			continue
		}
		if binary.BigEndian.Uint32(p[4:8]) != receiverSSRC || binary.BigEndian.Uint32(p[8:12]) != senderSSRC || string(p[12:16]) != "CAST" {
			continue
		}
		count := int(p[17])
		if len(p) < 20+count*4 {
			return out
		}
		f := castFeedback{checkpoint: p[16], delayMS: int(binary.BigEndian.Uint16(p[18:20]))}
		for i := 0; i < count; i++ {
			o := 20 + i*4
			f.loss = append(f.loss, packetLoss{p[o], binary.BigEndian.Uint16(p[o+1 : o+3]), p[o+3]})
		}
		out = append(out, f)
	}
	return out
}

func (t *StreamingSender) feedback(f castFeedback) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.next == 0 {
		return
	}
	// Expand the 8-bit checkpoint relative to the latest frame sent.
	latest := int64(t.next) - 1
	ack := latest - int64(byte(byte(latest)-f.checkpoint))
	if ack < t.stats.Acknowledged || latest-ack > 127 {
		return
	}
	t.stats.Acknowledged = ack
	t.stats.Feedback++
	t.stats.ReceiverDelay = time.Duration(f.delayMS) * time.Millisecond
	for id := range t.cache {
		if int64(id) <= ack {
			delete(t.cache, id)
		}
	}
	now := time.Now()
	for _, loss := range f.loss {
		id := ack + int64(uint8(loss.frame-byte(ack)))
		fr := t.cache[uint32(id)]
		if id <= ack || id > latest || fr == nil || now.Sub(fr.lastRetry) < 15*time.Millisecond ||
			now.Sub(fr.sent) > t.session.TargetDelay+200*time.Millisecond {
			continue
		}
		retry := func(i int) {
			if i < 0 || i >= len(fr.packets) {
				return
			}
			p := append([]byte(nil), fr.packets[i]...)
			binary.BigEndian.PutUint16(p[2:4], t.sequence)
			t.sequence++
			_ = t.write(p, true)
		}
		if loss.packet == 0xFFFF {
			for i := range fr.packets {
				retry(i)
			}
		} else {
			retry(int(loss.packet))
			for bit := 0; bit < 8; bit++ {
				if loss.following&(1<<bit) != 0 {
					retry(int(loss.packet) + bit + 1)
				}
			}
		}
		fr.lastRetry = now
	}
}
