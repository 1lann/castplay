package bridge

import (
	"context"
	"errors"
	"time"

	"castplay/internal/airplay"
)

// senderTrack is what the AirPlay sender says it is playing through this
// speaker, for Apple devices watching it (RemoteControl).
type senderTrack struct {
	active               bool
	sender               string
	title, artist, album string

	sampleRate float64
	// From the last progress: the track's RTP start and end.
	start, end  uint32
	hasProgress bool
	// Position: elapsed seconds at elapsedAt, advancing while playing.
	playing   bool
	elapsed   float64
	elapsedAt time.Time

	artwork     []byte
	artworkMIME string
	artworkSeq  int // bumped per image, to name it
}

func (t senderTrack) duration() float64 {
	if !t.hasProgress || t.sampleRate == 0 {
		return 0
	}
	return float64(t.end-t.start) / t.sampleRate
}

// OnSenderTrack registers a callback for changes to what the AirPlay sender
// plays. It is called with the speaker's lock released.
func (s *Speaker) OnSenderTrack(f func(senderTrack)) {
	s.mu.Lock()
	s.onTrack = f
	s.mu.Unlock()
}

// trackChanged reports the sender's track; called with mu held, it returns
// the notification to run after unlocking.
func (s *Speaker) trackChanged() func() {
	f, t := s.onTrack, s.track
	if f == nil {
		return func() {}
	}
	return func() { f(t) }
}

func (s *Speaker) Metadata(m airplay.Metadata) {
	s.mu.Lock()
	t := &s.track
	switch {
	case m.HasText:
		t.title, t.artist, t.album = m.Title, m.Artist, m.Album
	case m.HasProgress:
		t.start, t.end, t.hasProgress = m.Start, m.End, true
		if t.sampleRate > 0 {
			t.elapsed = float64(int32(m.Current-m.Start)) / t.sampleRate
			t.elapsedAt = time.Now()
		}
	case m.HasArtwork:
		t.artwork, t.artworkMIME = m.Artwork, m.ArtworkMIME
		t.artworkSeq++
	}
	notify := s.trackChanged()
	s.mu.Unlock()
	notify()
}

// trackAnchor follows play and pause; called with mu held.
func (s *Speaker) trackAnchor(a airplay.Anchor, at time.Time) {
	t := &s.track
	if a.Rate == 0 {
		if t.playing {
			t.elapsed += time.Since(t.elapsedAt).Seconds()
			t.elapsedAt = time.Now()
		}
		t.playing = false
		return
	}
	t.playing = true
	if t.hasProgress && t.sampleRate > 0 {
		t.elapsed = float64(int32(a.RTPTime-t.start)) / t.sampleRate
		t.elapsedAt = at
	} else if t.elapsedAt.IsZero() {
		t.elapsedAt = at
	}
}

var errNoSender = errors.New("no AirPlay sender")

// SenderCommand asks the AirPlay sender to play, pause, skip, etc.
func (s *Speaker) SenderCommand(ctx context.Context, c airplay.MediaCommand) error {
	s.mu.Lock()
	events, active := s.events, s.track.active
	s.mu.Unlock()
	if events == nil || !active {
		return errNoSender
	}
	return events.Command(ctx, c)
}
