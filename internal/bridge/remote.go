package bridge

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"

	"castplay/internal/airplay"
	"castplay/internal/cast"
	"castplay/internal/mrp"
)

// RemoteControl shows what the Cast device plays on its own (Spotify,
// YouTube Music, "Hey Google, play…") on Apple devices, the way they show a
// HomePod's music, and carries out their play/pause/skip/seek. It is the
// speaker's airplay.RemoteControl.
//
// castplay's own streaming app is ignored: while an Apple device plays
// through castplay, that device already knows what is playing.
type RemoteControl struct {
	log *slog.Logger
	mon *cast.Monitor
	srv *mrp.Server
}

func NewRemoteControl(dev cast.Device, info mrp.DeviceInfo, log *slog.Logger) *RemoteControl {
	rc := &RemoteControl{log: log.With("cast", dev.Name)}
	rc.srv = mrp.NewServer(mrp.Config{Device: info, Command: rc.command, Artwork: mrp.FetchArtwork, Log: rc.log})
	rc.mon = cast.NewMonitor(dev.Addr, []string{cast.AudioMirroringReceiver}, rc.changed, rc.log)
	return rc
}

// Run follows the Cast device until ctx is done.
func (rc *RemoteControl) Run(ctx context.Context) { rc.mon.Run(ctx) }

func (rc *RemoteControl) SetDevice(d cast.Device) { rc.mon.SetAddr(d.Addr) }

func (rc *RemoteControl) RemoteOpened(ch *airplay.RemoteChannel)         { rc.srv.Open(ch) }
func (rc *RemoteControl) RemoteData(ch *airplay.RemoteChannel, b []byte) { rc.srv.Data(ch, b) }
func (rc *RemoteControl) RemoteClosed(ch *airplay.RemoteChannel)         { rc.srv.Close(ch) }

func (rc *RemoteControl) changed(c cast.NowPlaying) {
	rc.srv.SetNowPlaying(nowPlaying(c))
}

// nowPlaying maps a Cast MEDIA_STATUS to MediaRemote state.
func nowPlaying(c cast.NowPlaying) mrp.NowPlaying {
	if c.AppID == "" {
		return mrp.NowPlaying{State: mrp.StateStopped}
	}
	np := mrp.NowPlaying{
		App:      castApp(c.AppID, c.AppName),
		Title:    c.Title,
		Artist:   c.Artist,
		Album:    c.Album,
		Duration: c.Duration,
		Elapsed:  c.CurrentTime,
		At:       c.At,
	}
	switch c.PlayerState {
	case "PLAYING":
		np.State, np.Rate = mrp.StatePlaying, 1
	case "PAUSED":
		np.State = mrp.StatePaused
	case "BUFFERING":
		np.State = mrp.StatePlaying
	default:
		np.State = mrp.StateStopped
		return np
	}
	np.ItemID = c.ContentID
	if np.ItemID == "" {
		np.ItemID = c.Title + "\x00" + c.Artist
	}
	np.ArtworkURL = c.ImageURL
	if c.Commands&cast.CommandPause != 0 {
		np.Commands = append(np.Commands, mrp.CommandPlay, mrp.CommandPause, mrp.CommandTogglePlayPause)
	}
	np.Commands = append(np.Commands, mrp.CommandStop)
	if c.Commands&cast.CommandQueueNext != 0 {
		np.Commands = append(np.Commands, mrp.CommandNextTrack)
	}
	if c.Commands&cast.CommandQueuePrev != 0 {
		np.Commands = append(np.Commands, mrp.CommandPreviousTrack)
	}
	if c.Commands&cast.CommandSeek != 0 && c.Duration > 0 {
		np.Commands = append(np.Commands, mrp.CommandSeekToPosition)
	}
	return np
}

// castApps gives well-known Cast apps their iOS bundle ID, so Apple devices
// can show the app's icon.
var castApps = map[string]string{
	"CC32E753": "com.spotify.client",
	"2DB7CC49": "com.google.ios.youtubemusic",
	"233637DE": "com.google.ios.youtube",
	"9AC194DC": "com.plexapp.plex",
	"5E81F6DB": "com.audible.iphone",
}

func castApp(id, name string) mrp.App {
	bundle := castApps[id]
	if bundle == "" {
		bundle = "com.google.cast." + id
	}
	h := fnv.New32a()
	h.Write([]byte(id))
	return mrp.App{BundleID: bundle, Name: name, PID: int(h.Sum32()%30000) + 1000}
}

var errUnsupported = errors.New("command not supported")

// command carries out a MediaRemote command on the Cast app.
func (rc *RemoteControl) command(ctx context.Context, c mrp.SendCommand) error {
	switch c.Command {
	case mrp.CommandPlay:
		return rc.mon.Media(ctx, "PLAY", nil)
	case mrp.CommandPause:
		return rc.mon.Media(ctx, "PAUSE", nil)
	case mrp.CommandTogglePlayPause:
		if rc.srv.NowPlaying().State == mrp.StatePlaying {
			return rc.mon.Media(ctx, "PAUSE", nil)
		}
		return rc.mon.Media(ctx, "PLAY", nil)
	case mrp.CommandStop:
		return rc.mon.Media(ctx, "STOP", nil)
	case mrp.CommandNextTrack:
		return rc.mon.Media(ctx, "QUEUE_UPDATE", map[string]any{"jump": 1})
	case mrp.CommandPreviousTrack:
		return rc.mon.Media(ctx, "QUEUE_UPDATE", map[string]any{"jump": -1})
	case mrp.CommandSeekToPosition:
		return rc.mon.Media(ctx, "SEEK", map[string]any{"currentTime": c.Position})
	case mrp.CommandSkipForward, mrp.CommandSkipBackward:
		step := c.Interval
		if step == 0 {
			step = 15
		}
		if c.Command == mrp.CommandSkipBackward {
			step = -step
		}
		return rc.mon.Media(ctx, "SEEK", map[string]any{"relativeTime": step})
	}
	return errUnsupported
}
