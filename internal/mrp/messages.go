package mrp

import "time"

// PlaybackState (Common.proto PlaybackState.Enum).
type PlaybackState int

const (
	StateUnknown PlaybackState = iota
	StatePlaying
	StatePaused
	StateStopped
	StateInterrupted
	StateSeeking
)

func (s PlaybackState) String() string {
	return [...]string{"unknown", "playing", "paused", "stopped", "interrupted", "seeking"}[min(int(s), 5)]
}

// Command is a MediaRemote command (CommandInfo.proto Command).
type Command int

const (
	CommandPlay            Command = 1
	CommandPause           Command = 2
	CommandTogglePlayPause Command = 3
	CommandStop            Command = 4
	CommandNextTrack       Command = 5
	CommandPreviousTrack   Command = 6
	CommandSkipForward     Command = 18
	CommandSkipBackward    Command = 19
	CommandSeekToPosition  Command = 45
)

// DeviceInfo describes the speaker in its DEVICE_INFO_MESSAGE.
type DeviceInfo struct {
	UniqueIdentifier string // the speaker's pairing ID
	Name             string
	ModelName        string // localizedModelName, e.g. "Google Home Max"
	ModelID          string
	GroupID          string // AirPlay group UUID
}

func (d DeviceInfo) encode() enc {
	return enc(nil).
		str(1, d.UniqueIdentifier).
		str(2, d.Name).
		str(3, d.ModelName).
		str(4, "22A100").
		str(5, "com.apple.mediaremoted").
		str(6, "1").
		int(7, 1).   // protocolVersion
		int(8, 139). // lastSupportedMessageType, as iOS 26 sends
		bool(9, true).
		bool(10, true).
		bool(11, true). // connected
		bool(13, true). // supportsACL
		bool(14, true). // supportsSharedQueue
		bool(15, true). // supportsExtendedMotion
		int(17, 3).     // sharedQueueVersion
		str(19, d.UniqueIdentifier).
		int(21, 7). // deviceClass Accessory
		int(22, 1). // logicalDeviceCount
		str(26, d.GroupID).
		str(27, d.Name).
		bool(29, true). // isGroupLeader
		bool(30, true). // isAirplayActive
		str(39, d.ModelID).
		str(42, d.GroupID)
}

func deviceInfoMessage(id string, d DeviceInfo) enc {
	return protocolMessage(TypeDeviceInfo, id, extDeviceInfo, d.encode())
}

// genericMessage acknowledges a request the device has nothing to say to.
func genericMessage(id string) enc {
	return protocolMessage(TypeGeneric, id, 0, nil)
}

func sendCommandResult(id string, path enc, ok bool) enc {
	status := 0 // Success
	if !ok {
		status = 2 // CommandFailed
	}
	result := enc(nil).
		msg(1, path).
		int(2, 0). // sendError NoError
		msg(3, enc(nil).int(1, status))
	return protocolMessage(TypeSendCommandResult, id, extSendCommandResult, enc(nil).
		int(1, 0).
		int(2, status).
		msg(5, path).
		msg(6, result))
}

// App identifies the app now playing on the speaker (the NowPlayingClient).
type App struct {
	BundleID string
	Name     string
	PID      int
}

func (a App) client() enc {
	return enc(nil).int(1, a.PID).str(2, a.BundleID).str(7, a.Name)
}

func (a App) playerPath() enc {
	return enc(nil).
		msg(1, enc(nil).int(1, 1).int(3, 1)). // origin {type Local, identifier 1}
		msg(2, a.client()).
		msg(3, enc(nil).str(1, "MediaRemote-DefaultPlayer").str(2, "Default Player"))
}

func setNowPlayingClient(a App) enc {
	return protocolMessage(TypeSetNowPlayingClient, "", extSetNowPlayingClient, enc(nil).msg(1, a.client()))
}

// NowPlaying is what the speaker is playing.
type NowPlaying struct {
	App    App
	State  PlaybackState
	ItemID string // unique per track
	Title  string
	Artist string
	Album  string

	Duration float64   // seconds, 0 if unknown or live
	Elapsed  float64   // seconds into the track at ElapsedAt
	At       time.Time // when Elapsed was measured
	Rate     float64   // 1 while playing, 0 when paused or buffering

	ArtworkURL string // "" if none
	Commands   []Command
}

// Artwork is a fetched cover image.
type Artwork struct {
	Data          []byte
	MIME          string
	Width, Height int
}

// cocoaEpoch is 2001-01-01 UTC, the epoch of MediaRemote timestamps.
var cocoaEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func cocoaTime(t time.Time) float64 { return t.Sub(cocoaEpoch).Seconds() }

// setState encodes SET_STATE_MESSAGE. art is non-nil when answering a
// PLAYBACK_QUEUE_REQUEST for artwork.
func setState(id string, np NowPlaying, art *Artwork) enc {
	path := np.App.playerPath()
	s := enc(nil)
	if np.ItemID != "" {
		md := enc(nil).
			str(1, np.Title).
			str(6, np.Album).
			str(7, np.Artist).
			bool(27, true). // isCurrentlyPlaying
			float(39, np.Rate).
			int(64, 1) // mediaType Audio
		if np.Duration > 0 {
			md = md.double(14, np.Duration)
		}
		if !np.At.IsZero() {
			md = md.double(35, np.Elapsed).double(74, cocoaTime(np.At))
		}
		if np.ArtworkURL != "" {
			md = md.bool(19, true).str(31, artworkMIME(art)).str(80, artworkID(np.ArtworkURL))
		}
		item := enc(nil).str(1, np.ItemID).msg(2, md)
		if art != nil {
			item = item.bytes(3, art.Data).int(13, art.Width).int(14, art.Height)
		}
		s = s.msg(3, enc(nil).int(1, 0).msg(2, item))
	} else {
		s = s.msg(3, enc(nil).int(1, 0))
	}
	if len(np.Commands) > 0 {
		cmds := enc(nil)
		for _, c := range np.Commands {
			cmds = cmds.msg(1, enc(nil).int(1, int(c)).bool(2, true))
		}
		s = s.msg(2, cmds)
	}
	s = s.int(6, int(np.State)).msg(9, path)
	if !np.At.IsZero() {
		s = s.double(11, cocoaTime(np.At))
	}
	return protocolMessage(TypeSetState, id, extSetState, s)
}

func artworkMIME(a *Artwork) string {
	if a != nil && a.MIME != "" {
		return a.MIME
	}
	return "image/jpeg"
}

// artworkID names an image so the sender refetches only when it changes.
func artworkID(url string) string {
	h := uint64(14695981039346656037)
	for i := 0; i < len(url); i++ {
		h = (h ^ uint64(url[i])) * 1099511628211
	}
	return "art-" + hex64(h)
}

func hex64(v uint64) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&15]
		v >>= 4
	}
	return string(b)
}

// SendCommand is a decoded SEND_COMMAND_MESSAGE.
type SendCommand struct {
	Command  Command
	Position float64 // seconds, for CommandSeekToPosition
	Interval float64 // seconds, for skip forward/backward
}

func parseSendCommand(m *Message) SendCommand {
	f := m.Ext(extSendCommand)
	opts := f.msg(2)
	return SendCommand{
		Command:  Command(f.uint(1)),
		Position: opts.double(9),
		Interval: opts.float(5),
	}
}

// queueRequest is a decoded PLAYBACK_QUEUE_REQUEST_MESSAGE.
type queueRequest struct {
	Location, Length int
	ArtworkWidth     float64
	ArtworkHeight    float64
}

func parseQueueRequest(m *Message) queueRequest {
	f := m.Ext(extPlaybackQueueRequest)
	return queueRequest{
		Location:      int(f.uint(1)),
		Length:        int(f.uint(2)),
		ArtworkWidth:  f.double(4),
		ArtworkHeight: f.double(5),
	}
}

// peerInfo is the part of the sender's DEVICE_INFO worth logging.
type peerInfo struct {
	Name, Model, Build string
}

func parseDeviceInfo(m *Message) peerInfo {
	f := m.Ext(extDeviceInfo)
	return peerInfo{Name: f.str(2), Model: f.str(39), Build: f.str(4)}
}
