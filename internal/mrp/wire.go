// Package mrp is a minimal MediaRemote (MRP) device: the now-playing service
// a HomePod offers Apple devices over AirPlay's remote control channel. It
// answers the sender's DEVICE_INFO handshake, pushes now-playing state and
// artwork, and turns the sender's commands into callbacks.
//
// Messages are hand-encoded protobuf; field numbers follow pyatv's
// pyatv/protocols/mrp/protobuf/*.proto.
package mrp

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// Message types (ProtocolMessage.Type).
const (
	TypeSendCommand          = 1
	TypeSendCommandResult    = 2
	TypeGetState             = 3
	TypeSetState             = 4
	TypeDeviceInfo           = 15
	TypeClientUpdatesConfig  = 16
	TypeVolumeAvailability   = 17
	TypePlaybackQueueRequest = 32
	TypeSetConnectionState   = 38
	TypeGeneric              = 42
	TypeSetNowPlayingClient  = 46
	TypeGetVolume            = 49
	TypeGetVolumeResult      = 50
	TypeSetVolume            = 51
	TypeVolumeDidChange      = 52
	TypeVolumeCapabilities   = 64
	TypeUpdateOutputDevice   = 65
)

// ProtocolMessage fields and the extensions used here.
const (
	fieldType             = 1
	fieldIdentifier       = 2
	fieldErrorCode        = 4
	fieldUniqueIdentifier = 85

	extSendCommand          = 6
	extSendCommandResult    = 7
	extSetState             = 9
	extDeviceInfo           = 20
	extClientUpdatesConfig  = 21
	extPlaybackQueueRequest = 37
	extSetConnectionState   = 42
	extVolumeAvailability   = 22
	extSetNowPlayingClient  = 50
	extGetVolumeResult      = 54
	extSetVolume            = 55
	extVolumeDidChange      = 56
	extVolumeCapabilities   = 68
	extUpdateOutputDevice   = 69
)

// Message is one decoded ProtocolMessage: its header and the raw bytes of
// each length-delimited field, by field number (the extension bodies).
type Message struct {
	Type       int
	Identifier string
	Fields     map[protowire.Number][]byte
}

// Ext returns the body of extension n, or nil.
func (m *Message) Ext(n protowire.Number) fields {
	b, ok := m.Fields[n]
	if !ok {
		return nil
	}
	f, _ := parseFields(b)
	return f
}

// Split decodes the messages in a remote control frame: each ProtocolMessage
// prefixed with its varint length.
func Split(data []byte) ([]*Message, error) {
	var out []*Message
	for len(data) > 0 {
		n, l := protowire.ConsumeVarint(data)
		if l < 0 || uint64(len(data)-l) < n {
			return out, errors.New("mrp: truncated frame")
		}
		m, err := parseMessage(data[l : l+int(n)])
		if err != nil {
			return out, err
		}
		out = append(out, m)
		data = data[l+int(n):]
	}
	return out, nil
}

func parseMessage(b []byte) (*Message, error) {
	f, err := parseFields(b)
	if err != nil {
		return nil, err
	}
	m := &Message{Type: int(f.uint(fieldType)), Identifier: f.str(fieldIdentifier), Fields: map[protowire.Number][]byte{}}
	for n, vs := range f {
		if b, ok := vs[0].([]byte); ok {
			m.Fields[n] = b
		}
	}
	return m, nil
}

// fields is a decoded protobuf message: every value of each field, as
// uint64 (varint, fixed) or []byte (length-delimited).
type fields map[protowire.Number][]any

func parseFields(b []byte) (fields, error) {
	f := fields{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return f, protowire.ParseError(n)
		}
		b = b[n:]
		var v any
		switch typ {
		case protowire.VarintType:
			x, n2 := protowire.ConsumeVarint(b)
			v, n = x, n2
		case protowire.Fixed64Type:
			x, n2 := protowire.ConsumeFixed64(b)
			v, n = x, n2
		case protowire.Fixed32Type:
			x, n2 := protowire.ConsumeFixed32(b)
			v, n = uint64(x), n2
		case protowire.BytesType:
			x, n2 := protowire.ConsumeBytes(b)
			v, n = x, n2
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return f, protowire.ParseError(n)
		}
		b = b[n:]
		if v != nil {
			f[num] = append(f[num], v)
		}
	}
	return f, nil
}

func (f fields) uint(n protowire.Number) uint64 {
	if vs := f[n]; len(vs) > 0 {
		x, _ := vs[0].(uint64)
		return x
	}
	return 0
}

func (f fields) has(n protowire.Number) bool { return len(f[n]) > 0 }

func (f fields) bool(n protowire.Number) bool { return f.uint(n) != 0 }

func (f fields) double(n protowire.Number) float64 { return math.Float64frombits(f.uint(n)) }

func (f fields) float(n protowire.Number) float64 {
	return float64(math.Float32frombits(uint32(f.uint(n))))
}

func (f fields) str(n protowire.Number) string {
	if vs := f[n]; len(vs) > 0 {
		b, _ := vs[0].([]byte)
		return string(b)
	}
	return ""
}

func (f fields) msg(n protowire.Number) fields {
	if vs := f[n]; len(vs) > 0 {
		if b, ok := vs[0].([]byte); ok {
			m, _ := parseFields(b)
			return m
		}
	}
	return fields{}
}

// Describe renders a message's fields for logs: numbers, strings, and nested
// messages where the bytes parse as one.
func (m *Message) Describe() string {
	var sb strings.Builder
	for n, b := range m.Fields {
		if n == fieldIdentifier || n == fieldUniqueIdentifier {
			continue
		}
		fmt.Fprintf(&sb, "%d:", n)
		describe(&sb, b, 0)
		sb.WriteByte(' ')
	}
	return strings.TrimSpace(sb.String())
}

func describe(sb *strings.Builder, b []byte, depth int) {
	if f, err := parseFields(b); err == nil && len(b) > 0 && depth < 6 && !printableText(b) {
		sb.WriteByte('{')
		first := true
		for n, vs := range f {
			for _, v := range vs {
				if !first {
					sb.WriteByte(' ')
				}
				first = false
				fmt.Fprintf(sb, "%d:", n)
				switch v := v.(type) {
				case uint64:
					fmt.Fprint(sb, v)
				case []byte:
					describe(sb, v, depth+1)
				}
			}
		}
		sb.WriteByte('}')
		return
	}
	if printableText(b) {
		fmt.Fprintf(sb, "%q", b)
		return
	}
	fmt.Fprintf(sb, "<%d bytes>", len(b))
}

func printableText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// enc builds one protobuf message.
type enc []byte

func (e enc) varint(n protowire.Number, v uint64) enc {
	e = protowire.AppendTag(e, n, protowire.VarintType)
	return protowire.AppendVarint(e, v)
}

func (e enc) int(n protowire.Number, v int) enc { return e.varint(n, uint64(int64(v))) }

func (e enc) bool(n protowire.Number, v bool) enc { return e.varint(n, protowire.EncodeBool(v)) }

func (e enc) double(n protowire.Number, v float64) enc {
	e = protowire.AppendTag(e, n, protowire.Fixed64Type)
	return protowire.AppendFixed64(e, math.Float64bits(v))
}

func (e enc) float(n protowire.Number, v float64) enc {
	e = protowire.AppendTag(e, n, protowire.Fixed32Type)
	return protowire.AppendFixed32(e, math.Float32bits(float32(v)))
}

func (e enc) bytes(n protowire.Number, v []byte) enc {
	e = protowire.AppendTag(e, n, protowire.BytesType)
	return protowire.AppendBytes(e, v)
}

func (e enc) str(n protowire.Number, v string) enc {
	if v == "" {
		return e
	}
	return e.bytes(n, []byte(v))
}

func (e enc) msg(n protowire.Number, v enc) enc { return e.bytes(n, v) }

// protocolMessage wraps an extension body in a ProtocolMessage.
func protocolMessage(typ int, id string, ext protowire.Number, body enc) enc {
	e := enc(nil).int(fieldType, typ).str(fieldIdentifier, id)
	if ext != 0 {
		e = e.msg(ext, body)
	}
	return e
}

// Frame concatenates messages, each prefixed with its varint length.
func Frame(msgs ...enc) []byte {
	var out []byte
	for _, m := range msgs {
		out = protowire.AppendVarint(out, uint64(len(m)))
		out = append(out, m...)
	}
	return out
}
