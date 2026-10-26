package bridge

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"

	"castplay/internal/airplay"
	"castplay/internal/cast"
	"castplay/internal/mrp"
)

// RemoteControl shows what the Cast device plays on its own (Spotify,
// YouTube Music, "Hey Google, play…") on Apple devices, the way they show a
// HomePod's music, and carries out their play/pause/skip/seek. It is the
// speaker's airplay.RemoteControl.
//
// While an AirPlay sender plays through castplay (the Cast device runs
// castplay's own streaming app, which the monitor ignores), they see the
// sender's track instead, and their commands go to the sender.
type RemoteControl struct {
	log *slog.Logger
	sp  *Speaker
	mon *cast.Monitor
	srv *mrp.Server

	mu     sync.Mutex
	cast   cast.NowPlaying
	sender senderTrack
	muted  bool // the Cast device's
}

func NewRemoteControl(dev cast.Device, info mrp.DeviceInfo, sp *Speaker, log *slog.Logger) *RemoteControl {
	rc := &RemoteControl{log: log.With("cast", dev.Name), sp: sp}
	rc.srv = mrp.NewServer(mrp.Config{Device: info, Command: rc.command, SetVolume: rc.setVolume,
		Artwork: mrp.FetchArtwork, Log: rc.log})
	rc.mon = cast.NewMonitor(dev.Addr, []string{cast.AudioMirroringReceiver}, rc.castChanged, rc.log)
	rc.mon.OnVolume(rc.castVolume)
	sp.OnSenderTrack(rc.senderChanged)
	return rc
}

// Run follows the Cast device until ctx is done.
func (rc *RemoteControl) Run(ctx context.Context) { rc.mon.Run(ctx) }

func (rc *RemoteControl) SetDevice(d cast.Device) { rc.mon.SetAddr(d.Addr) }

func (rc *RemoteControl) RemoteOpened(ch *airplay.RemoteChannel)         { rc.srv.Open(ch) }
func (rc *RemoteControl) RemoteData(ch *airplay.RemoteChannel, b []byte) { rc.srv.Data(ch, b) }
func (rc *RemoteControl) RemoteClosed(ch *airplay.RemoteChannel)         { rc.srv.Close(ch) }

// The Apple devices' volume slider is the AirPlay one: full volume is the
// Cast level VolumeScale, as for an AirPlay sender's volume.
func (rc *RemoteControl) scale() float64 {
	if s := rc.sp.cfg.VolumeScale; s > 0 {
		return s
	}
	return 1
}

func (rc *RemoteControl) castVolume(level float64, muted bool) {
	rc.mu.Lock()
	rc.muted = muted
	rc.mu.Unlock()
	v := min(level/rc.scale(), 1)
	if muted {
		v = 0
	}
	rc.srv.SetVolume(v)
}

func (rc *RemoteControl) setVolume(ctx context.Context, v float64) error {
	rc.mu.Lock()
	unmute := rc.muted && v > 0
	rc.mu.Unlock()
	return rc.mon.SetVolume(ctx, v*rc.scale(), unmute)
}

func (rc *RemoteControl) castChanged(c cast.NowPlaying) {
	rc.mu.Lock()
	rc.cast = c
	rc.publishLocked()
	rc.mu.Unlock()
}

func (rc *RemoteControl) senderChanged(t senderTrack) {
	rc.mu.Lock()
	rc.sender = t
	rc.publishLocked()
	rc.mu.Unlock()
}

// publishLocked pushes the current source's state; called with mu held.
func (rc *RemoteControl) publishLocked() {
	if rc.sender.active {
		rc.srv.SetNowPlaying(senderNowPlaying(rc.sender))
	} else {
		rc.srv.SetNowPlaying(nowPlaying(rc.cast))
	}
}

// senderNowPlaying maps an AirPlay sender's track to MediaRemote state. The
// sender doesn't say which app plays; Music is the usual one.
func senderNowPlaying(t senderTrack) mrp.NowPlaying {
	np := mrp.NowPlaying{
		App:      mrp.App{BundleID: "com.apple.Music", Name: "Music", PID: 999},
		State:    mrp.StatePaused,
		ItemID:   t.title + "\x00" + t.artist + "\x00" + t.album,
		Title:    t.title,
		Artist:   t.artist,
		Album:    t.album,
		Duration: t.duration(),
		Elapsed:  t.elapsed,
		At:       t.elapsedAt,
		Commands: []mrp.Command{mrp.CommandPlay, mrp.CommandPause, mrp.CommandTogglePlayPause,
			mrp.CommandNextTrack, mrp.CommandPreviousTrack},
	}
	if t.playing {
		np.State, np.Rate = mrp.StatePlaying, 1
	}
	if len(t.artwork) > 0 {
		np.ArtworkData = t.artwork
		np.ArtworkID = fmt.Sprintf("airplay-%d", t.artworkSeq)
	}
	return np
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

// senderCommands maps MediaRemote commands to the AirPlay sender's.
var senderCommands = map[mrp.Command]airplay.MediaCommand{
	mrp.CommandPlay:            airplay.CommandPlay,
	mrp.CommandPause:           airplay.CommandPause,
	mrp.CommandTogglePlayPause: airplay.CommandPlayPause,
	mrp.CommandStop:            airplay.CommandStop,
	mrp.CommandNextTrack:       airplay.CommandNext,
	mrp.CommandPreviousTrack:   airplay.CommandPrevious,
}

// command carries out a MediaRemote command on the AirPlay sender, or else
// on the Cast app.
func (rc *RemoteControl) command(ctx context.Context, c mrp.SendCommand) error {
	rc.mu.Lock()
	sender := rc.sender.active
	rc.mu.Unlock()
	if sender {
		mc, ok := senderCommands[c.Command]
		if !ok {
			return errUnsupported
		}
		return rc.sp.SenderCommand(ctx, mc)
	}
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
