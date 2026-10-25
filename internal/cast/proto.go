package cast

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
)

// Message is the Cast v2 CastMessage (cast_channel.proto). Only the string
// payload type is used by the media/receiver namespaces we speak, so binary
// payloads are decoded but never produced.
type Message struct {
	SourceID      string
	DestinationID string
	Namespace     string
	PayloadUTF8   string
	PayloadBinary []byte
}

const (
	fieldProtocolVersion = 1
	fieldSourceID        = 2
	fieldDestinationID   = 3
	fieldNamespace       = 4
	fieldPayloadType     = 5
	fieldPayloadUTF8     = 6
	fieldPayloadBinary   = 7

	protocolCastV2_1_0 = 0
	payloadTypeString  = 0

	maxMessageSize = 64 << 10
)

func (m *Message) marshal() []byte {
	var b []byte
	b = protowire.AppendTag(b, fieldProtocolVersion, protowire.VarintType)
	b = protowire.AppendVarint(b, protocolCastV2_1_0)
	b = protowire.AppendTag(b, fieldSourceID, protowire.BytesType)
	b = protowire.AppendString(b, m.SourceID)
	b = protowire.AppendTag(b, fieldDestinationID, protowire.BytesType)
	b = protowire.AppendString(b, m.DestinationID)
	b = protowire.AppendTag(b, fieldNamespace, protowire.BytesType)
	b = protowire.AppendString(b, m.Namespace)
	b = protowire.AppendTag(b, fieldPayloadType, protowire.VarintType)
	b = protowire.AppendVarint(b, payloadTypeString)
	b = protowire.AppendTag(b, fieldPayloadUTF8, protowire.BytesType)
	b = protowire.AppendString(b, m.PayloadUTF8)
	return b
}

func (m *Message) unmarshal(b []byte) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch {
		case typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
			switch num {
			case fieldSourceID:
				m.SourceID = string(v)
			case fieldDestinationID:
				m.DestinationID = string(v)
			case fieldNamespace:
				m.Namespace = string(v)
			case fieldPayloadUTF8:
				m.PayloadUTF8 = string(v)
			case fieldPayloadBinary:
				m.PayloadBinary = append([]byte(nil), v...)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return nil
}

// writeMessage frames m with the 4-byte big-endian length prefix used on the
// Cast TLS channel.
func writeMessage(w io.Writer, m *Message) error {
	body := m.marshal()
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err := w.Write(buf)
	return err
}

func readMessage(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size > maxMessageSize {
		return nil, fmt.Errorf("cast: message too large (%d bytes)", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	m := &Message{}
	if err := m.unmarshal(body); err != nil {
		return nil, errors.Join(errors.New("cast: bad message"), err)
	}
	return m, nil
}
