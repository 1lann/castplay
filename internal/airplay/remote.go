package airplay

import (
	"context"
	"fmt"
	"strconv"
)

// Remote control channels. With status flag RemoteControlRelay set, every
// Apple device on the network opens a MediaRemote channel to the speaker, as
// it does to a HomePod, to show and control what the speaker plays:
//
//   - The device sends a phase-2 SETUP with a type 130 stream {controlType: 1,
//     channelID, clientUUID, clientTypeUUID} and no seed. The reply carries
//     only the streamID (no dataPort, as shairport-sync): the channel then
//     shares the existing connections.
//   - Device to speaker: POST /command {params: {data}} on the control
//     connection.
//   - Speaker to device: the same request on the event channel, tagged with
//     X-Apple-StreamID.
//
// data is a run of MRP ProtocolMessages, each prefixed with its varint length.

const (
	streamRemoteControl      = 130
	statusRemoteControlRelay = 1 << 11
)

// RemoteControl serves the MediaRemote channels senders open. Its methods are
// called on the sender's control connection goroutine and must not block.
type RemoteControl interface {
	RemoteOpened(ch *RemoteChannel)
	RemoteData(ch *RemoteChannel, frame []byte)
	RemoteClosed(ch *RemoteChannel)
}

// RemoteChannel is one MediaRemote channel from a sender.
type RemoteChannel struct {
	s         *session
	id        uint64 // streamID
	channelID string
}

func (c *RemoteChannel) String() string {
	return fmt.Sprintf("%s/%d", c.s.sender, c.id)
}

// Send delivers a frame of MRP messages to the sender.
func (c *RemoteChannel) Send(ctx context.Context, frame []byte) error {
	if c.s.events == nil {
		return errNoEventChannel
	}
	return c.s.events.postHeader(ctx, map[string]any{"params": map[string]any{"data": frame}},
		"X-Apple-StreamID", strconv.FormatUint(c.id, 10))
}

// openRemote answers a type 130 stream descriptor.
func (s *session) openRemote(desc map[string]any) map[string]any {
	s.r.mu.Lock()
	s.r.nextID++
	id := s.r.nextID
	s.r.mu.Unlock()
	ch := &RemoteChannel{s: s, id: id, channelID: pString(desc, "channelID")}
	s.remotes[id] = ch
	s.log.Debug("remote control stream", "streamID", id, "channelID", ch.channelID, "clientUUID", pString(desc, "clientUUID"))
	s.r.cfg.RemoteControl.RemoteOpened(ch)
	return map[string]any{"type": streamRemoteControl, "streamID": id}
}

func (s *session) closeRemote(id uint64) bool {
	ch := s.remotes[id]
	if ch == nil {
		return false
	}
	delete(s.remotes, id)
	s.r.cfg.RemoteControl.RemoteClosed(ch)
	return true
}

// remoteCommand handles a sender's POST /command. Remote control frames
// carry only params.data; anything else (updateMRSupportedCommands from a
// Mac) is acknowledged and ignored.
func (s *session) remoteCommand(req *request) {
	body, err := req.plistBody()
	if err != nil {
		return
	}
	params, _ := body["params"].(map[string]any)
	data := pBytes(params, "data")
	if body["type"] != nil || data == nil {
		return
	}
	ch := s.remoteFor(req)
	if ch == nil {
		s.log.Warn("remote control data without a channel", "len", len(data), "headers", req.Header)
		return
	}
	s.r.cfg.RemoteControl.RemoteData(ch, data)
}

// remoteFor finds the channel a /command belongs to: by X-Apple-StreamID if
// the sender says, else the newest.
func (s *session) remoteFor(req *request) *RemoteChannel {
	if v := req.Header.Get("X-Apple-StreamID"); v != "" {
		if id, err := strconv.ParseUint(v, 10, 64); err == nil && s.remotes[id] != nil {
			return s.remotes[id]
		}
	}
	var newest *RemoteChannel
	for _, ch := range s.remotes {
		if newest == nil || ch.id > newest.id {
			newest = ch
		}
	}
	if len(s.remotes) > 1 {
		s.log.Debug("remote control data: channel ambiguous, using newest", "streamID", newest.id, "headers", req.Header)
	}
	return newest
}
