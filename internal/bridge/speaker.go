// Package bridge connects one AirPlay receiver to one Cast device: it
// implements airplay.Player by transcoding the sender's audio to Opus and
// playing it over Cast Streaming, scheduled so that each sample comes out of
// the speaker at the time the AirPlay sender asked for.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"time"

	"castplay/internal/airplay"
	"castplay/internal/cast"
	"castplay/internal/dacp"
	"castplay/internal/media"
	"castplay/internal/ptp"
)

type Config struct {
	Device        cast.Device
	TargetDelay   time.Duration // Cast Streaming receiver buffer
	OutputLatency time.Duration // speaker's own pipeline after that buffer
	BitrateKbps   int
	VolumeScale   float64 // Cast volume for AirPlay full volume
	IdleTimeout   time.Duration
	PTP           *ptp.Follower // nil: play relative to when anchors arrive
	Log           *slog.Logger
}

const (
	opusFrame = 960 // 20 ms at 48 kHz

	// lookahead bounds how far ahead of its play time audio is transcoded.
	lookahead = 1500 * time.Millisecond
	// sendLead is how early a frame may reach the Cast receiver before its
	// capture time (the spike showed 300 ms early is honoured).
	sendLead = 250 * time.Millisecond
	// maxQueue bounds buffered AirPlay audio (senders push far ahead).
	maxQueue = 20000 // ~7.7 min of 1024-sample AAC frames
)

// Speaker is the airplay.Player for one Cast device.
type Speaker struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	cond   *sync.Cond
	format airplay.Format
	queue  []airplay.Packet
	epoch  int // bumped whenever playback must stop (pause, flush, teardown)
	tc     *media.Transcoder
	volume float64 // last AirPlay dB
	hasVol bool
	remote *dacp.Remote // the sender's, for volume changes made on the Cast side
	events *airplay.EventChannel
	// A volume change pushed to the sender: until pushedAt+echoWindow, its
	// SET_PARAMETER echoes between pushedFrom and pushedTo (dB) are ignored.
	pushedFrom, pushedTo float64
	pushedAt             time.Time
	idle                 *time.Timer
	running              bool

	castMu    sync.Mutex
	cc        *cast.Client
	tx        *cast.StreamingSender
	castStop  context.CancelFunc
	castEpoch time.Time

	// The last volume we set on the Cast device, to tell its echo in
	// RECEIVER_STATUS from a change made with the Google Home app or on the
	// speaker itself.
	setLevel   float64
	setMuted   bool
	setAt      time.Time
	setPending bool
}

func NewSpeaker(cfg Config) *Speaker {
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = time.Minute
	}
	s := &Speaker{cfg: cfg, log: cfg.Log.With("cast", cfg.Device.Name)}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// --- airplay.Player ------------------------------------------------------

func (s *Speaker) StreamStarted(info airplay.StreamInfo) {
	s.mu.Lock()
	s.stopLocked()
	s.format = info.Format
	s.remote = info.Remote
	s.events = info.Events
	s.queue = s.queue[:0]
	s.running = true
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.mu.Unlock()
	switch info.Format.Codec {
	case airplay.CodecAAC, airplay.CodecALAC, airplay.CodecPCM:
	default:
		s.log.Warn("unsupported audio format; the sender will play to silence", "format", info.Format.String())
	}
	s.log.Info("stream started", "sender", info.Sender, "format", info.Format.String(), "timing", info.TimingProtocol)
	// Bring the Cast session up now; the play anchor follows within ~0.3 s.
	go func() {
		if err := s.ensureCast(); err != nil {
			s.log.Error("cast session", "err", err)
		}
	}()
}

func (s *Speaker) Packet(p airplay.Packet) {
	if len(p.Payload) == 0 {
		return
	}
	s.mu.Lock()
	if len(s.queue) >= maxQueue {
		s.queue = s.queue[1:]
	}
	s.queue = append(s.queue, p)
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *Speaker) Anchor(a airplay.Anchor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	if a.Rate == 0 {
		s.log.Info("paused")
		return
	}
	at := a.Received
	source := "arrival"
	if s.cfg.PTP != nil && !a.NetworkTime.IsZero() {
		if t, ok := s.cfg.PTP.LocalTime(a.NetworkTime.ClockID, a.NetworkTime.Nanos()); ok {
			at, source = t, "ptp"
		}
	}
	s.log.Info("play", "rtpTime", a.RTPTime, "anchorIn", time.Until(at).Round(time.Millisecond), "clock", source)
	go s.pump(s.epoch, a.RTPTime, at, s.format)
}

func (s *Speaker) Flush(f airplay.Flush) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
	kept := s.queue[:0]
	for _, p := range s.queue {
		inRange := f.All || seqLE(p.Seq, f.UntilSeq) && (!f.HasFrom || seqLE(f.FromSeq, p.Seq))
		if !inRange {
			kept = append(kept, p)
		}
	}
	s.log.Info("flush", "dropped", len(s.queue)-len(kept), "kept", len(kept))
	s.queue = kept
}

func (s *Speaker) Volume(db float64) {
	s.mu.Lock()
	s.volume, s.hasVol = db, true
	echo := time.Since(s.pushedAt) < echoWindow && s.isPushEcho(db)
	s.mu.Unlock()
	if echo {
		s.log.Debug("volume echo from sender", "airplayDB", db)
		return
	}
	go s.applyVolume(db)
}

// echoWindow is how long the sender's SET_PARAMETER volume echoes of a
// change pushed to it are recognised.
const echoWindow = 3 * time.Second

// isPushEcho reports whether db lies on the path of the last volume change
// pushed to the sender (the steps echo too); called with mu held.
func (s *Speaker) isPushEcho(db float64) bool {
	lo, hi := math.Min(s.pushedFrom, s.pushedTo), math.Max(s.pushedFrom, s.pushedTo)
	if lo <= -30 {
		lo = math.Inf(-1) // the bottom of the slider and mute
	}
	return db >= lo-0.3 && db <= hi+0.3
}

func (s *Speaker) TimingPeers(addrs []string) {
	if s.cfg.PTP != nil {
		s.cfg.PTP.SetPeers(addrs)
	}
}

func (s *Speaker) StreamStopped() {
	s.mu.Lock()
	s.stopLocked()
	s.queue = s.queue[:0]
	s.running = false
	if s.idle != nil {
		s.idle.Stop()
	}
	s.idle = time.AfterFunc(s.cfg.IdleTimeout, func() {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			s.log.Info("idle; releasing Cast device")
			s.closeCast()
		}
	})
	s.mu.Unlock()
	s.log.Info("stream stopped")
}

// stopLocked ends the current pump. Callers hold s.mu.
func (s *Speaker) stopLocked() {
	s.epoch++
	if s.tc != nil {
		s.tc.Close()
		s.tc = nil
	}
	s.cond.Broadcast()
}

// seqLE reports a <= b for 24-bit buffered-stream sequence numbers.
func seqLE(a, b uint32) bool { return (b-a)&0xffffff < 0x800000 }

// --- playback ----------------------------------------------------------------

// pump plays one stretch of audio, from an anchor until the next stop.
// The feeder hands AirPlay frames to ffmpeg up to `lookahead` before they
// are due; this goroutine reads Opus packets back and sends each to Cast
// Streaming stamped with its capture time: play time - target - output
// latency, so the receiver plays it exactly at the AirPlay play time.
func (s *Speaker) pump(epoch int, anchorRTP uint32, anchorAt time.Time, f airplay.Format) {
	log := s.log.With("epoch", epoch)
	in := media.Input{SampleRate: f.SampleRate, Channels: f.Channels, BitDepth: f.BitDepth}
	switch f.Codec {
	case airplay.CodecAAC:
		in.Codec = "aac"
	case airplay.CodecALAC:
		in.Codec, in.ALACConfig = "alac", f.ALACCookie()[12:]
	case airplay.CodecPCM:
		in.Codec = "pcm"
	default:
		return
	}
	tx, castEpoch, err := s.castReady(5 * time.Second)
	if err != nil {
		log.Error("no Cast session", "err", err)
		return
	}
	tc, err := media.StartOpus(context.Background(), in, s.cfg.BitrateKbps, opusFrame)
	if err != nil {
		log.Error("transcoder", "err", err)
		return
	}
	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		tc.Close()
		return
	}
	s.tc = tc
	s.mu.Unlock()

	rate := float64(f.SampleRate)
	playAt := func(ts float64) time.Time { // ts: RTP samples relative to the anchor
		return anchorAt.Add(time.Duration(ts / rate * float64(time.Second)))
	}
	firstTS := make(chan uint32, 1)
	go s.feed(epoch, tc, anchorRTP, playAt, firstTS, log)

	var first uint32
	select {
	case first = <-firstTS:
	case <-time.After(30 * time.Second):
		return
	}
	delay := s.cfg.TargetDelay + s.cfg.OutputLatency
	var rtp0 uint32
	late, sent := 0, 0
	defer func() {
		log.Info("playback segment ended", "frames", sent, "late", late)
	}()
	for j := 0; ; j++ {
		pkt, err := tc.ReadPacket()
		if err != nil {
			if s.current(epoch) {
				log.Warn("transcoder stopped", "err", err)
			}
			return
		}
		// 48 kHz sample where this packet starts, mapped back to the
		// sender's RTP clock relative to the anchor.
		start48 := float64(j*opusFrame - tc.PreSkip)
		rel := float64(int32(first-anchorRTP)) + start48*rate/48000
		capture := playAt(rel).Add(-delay)
		if j == 0 {
			rtp0 = uint32(int64(capture.Sub(castEpoch)) * 48000 / int64(time.Second))
		}
		if wait := time.Until(capture.Add(-sendLead)); wait > 0 {
			time.Sleep(wait)
		}
		if !s.current(epoch) {
			return
		}
		if time.Now().After(capture.Add(s.cfg.TargetDelay - 10*time.Millisecond)) {
			late++ // would arrive after the receiver needs it
			continue
		}
		if err := tx.SendFrame(rtp0+uint32(j*opusFrame), pkt); err != nil {
			log.Warn("cast send", "err", err)
			return
		}
		sent++
		if sent%500 == 0 { // every 10 s
			st := tx.Stats()
			log.Info("stream stats", "sent", sent, "late", late, "castRetransmits", st.Retransmits,
				"unacked", int64(st.Frames)-1-st.Acknowledged, "receiverDelay", st.ReceiverDelay,
				"sendMargin", time.Until(capture.Add(s.cfg.TargetDelay)).Round(time.Millisecond))
		}
	}
}

// feed writes queued frames from the anchor onwards into the transcoder,
// no earlier than `lookahead` before each is due to play.
func (s *Speaker) feed(epoch int, tc *media.Transcoder, anchorRTP uint32, playAt func(float64) time.Time, firstTS chan<- uint32, log *slog.Logger) {
	var first, next uint32
	started := false
	for {
		s.mu.Lock()
		var p airplay.Packet
		found := false
		for !found {
			if s.epoch != epoch {
				s.mu.Unlock()
				return
			}
			// Frames whose play time is well past are done with; keep the
			// rest (even if already transcoded) so a pause can resume them.
			for len(s.queue) > 0 && time.Since(playAt(float64(int32(s.queue[0].Timestamp-anchorRTP)))) > 2*time.Second {
				s.queue = s.queue[1:]
			}
			for _, q := range s.queue {
				if int32(q.Timestamp-anchorRTP) < 0 || (started && int32(q.Timestamp-next) < 0) {
					continue
				}
				p, found = q, true
				break
			}
			if !found {
				s.cond.Wait()
			}
		}
		s.mu.Unlock()

		due := playAt(float64(int32(p.Timestamp - anchorRTP)))
		if wait := time.Until(due) - lookahead; wait > 0 {
			time.Sleep(wait)
		}
		if !s.current(epoch) {
			return
		}
		if started && p.Timestamp != next {
			// A missing frame may have been resent while we waited.
			s.mu.Lock()
			for _, q := range s.queue {
				if int32(q.Timestamp-next) >= 0 && int32(q.Timestamp-p.Timestamp) < 0 {
					p = q // the earliest; resent frames are queued out of order
				}
			}
			s.mu.Unlock()
			if p.Timestamp != next {
				log.Debug("gap in sender audio", "expected", next, "got", p.Timestamp)
			}
		}
		if !started {
			first, started = p.Timestamp, true
			firstTS <- first
		}
		// Frames go in at their RTP position, so gaps become silence.
		if err := tc.WriteFrame(int64(int32(p.Timestamp-first)), p.Payload); err != nil {
			return
		}
		next = p.Timestamp + uint32(max(s.frameSamples(), 1))
	}
}

func (s *Speaker) frameSamples() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.format.SamplesPerFrame
}

func (s *Speaker) current(epoch int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch == epoch
}

// --- Cast session ------------------------------------------------------------

func (s *Speaker) ensureCast() error {
	s.castMu.Lock()
	defer s.castMu.Unlock()
	if s.cc != nil {
		select {
		case <-s.cc.Done():
			s.closeCastLocked()
		default:
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	began := time.Now()
	cc, err := cast.Dial(ctx, s.cfg.Device.Addr, s.log)
	if err != nil {
		return err
	}
	if err := cc.Launch(ctx, cast.AudioMirroringReceiver); err != nil {
		cc.Close()
		return fmt.Errorf("launch mirroring receiver: %w", err)
	}
	sess, err := cc.NegotiateAudio(ctx, s.cfg.BitrateKbps*1000, s.cfg.TargetDelay, opusFrame)
	if err != nil {
		cc.StopApp(ctx)
		cc.Close()
		return err
	}
	host, _, _ := net.SplitHostPort(s.cfg.Device.Addr)
	tx, err := cast.NewStreamingSender(host, sess)
	if err != nil {
		cc.StopApp(ctx)
		cc.Close()
		return err
	}
	runCtx, stop := context.WithCancel(context.Background())
	s.cc, s.tx, s.castStop, s.castEpoch = cc, tx, stop, time.Now()
	s.setAt = time.Time{}
	cc.OnVolume(s.castVolume)
	go tx.Receive(runCtx)
	go s.reportLoop(runCtx, tx, s.castEpoch)
	s.log.Info("cast session ready", "took", time.Since(began).Round(time.Millisecond), "target", s.cfg.TargetDelay)

	s.mu.Lock()
	db, has := s.volume, s.hasVol
	s.mu.Unlock()
	if has {
		go s.applyVolume(db)
	}
	return nil
}

// castReady waits for the Cast session started by StreamStarted.
func (s *Speaker) castReady(timeout time.Duration) (*cast.StreamingSender, time.Time, error) {
	deadline := time.Now().Add(timeout)
	for {
		s.castMu.Lock()
		tx, epoch := s.tx, s.castEpoch
		s.castMu.Unlock()
		if tx != nil {
			return tx, epoch, nil
		}
		if time.Now().After(deadline) {
			if err := s.ensureCast(); err != nil {
				return nil, time.Time{}, err
			}
			deadline = time.Now().Add(timeout)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reportLoop publishes the RTP clock: RTP 0 is castEpoch, 48 kHz.
func (s *Speaker) reportLoop(ctx context.Context, tx *cast.StreamingSender, epoch time.Time) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		now := time.Now()
		rtp := uint32(int64(now.Sub(epoch)) * 48000 / int64(time.Second))
		if err := tx.SendReport(now, rtp); err != nil && !errors.Is(err, net.ErrClosed) {
			s.log.Debug("sender report", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Speaker) closeCast() {
	s.castMu.Lock()
	defer s.castMu.Unlock()
	s.closeCastLocked()
}

func (s *Speaker) closeCastLocked() {
	if s.castStop != nil {
		s.castStop()
	}
	if s.tx != nil {
		s.tx.Close()
	}
	if s.cc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		s.cc.StopApp(ctx)
		cancel()
		s.cc.Close()
	}
	s.cc, s.tx, s.castStop = nil, nil, nil
}

// applyVolume maps AirPlay's -30..0 dB slider (-144 = mute) onto Cast's
// 0..1 device volume, scaled by VolumeScale.
func (s *Speaker) applyVolume(db float64) {
	s.castMu.Lock()
	cc := s.cc
	s.castMu.Unlock()
	if cc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if db <= -144 {
		s.log.Info("volume", "airplayDB", db, "cast", "muted")
		s.noteVolumeSet(-1, true)
		if err := cc.SetMuted(ctx, true); err != nil {
			s.log.Warn("mute", "err", err)
		}
		return
	}
	level := math.Max(0, math.Min(1, (db+30)/30)) * s.cfg.VolumeScale
	s.log.Info("volume", "airplayDB", db, "cast", fmt.Sprintf("%.2f", level))
	s.noteVolumeSet(level, false)
	if err := cc.SetMuted(ctx, false); err != nil {
		s.log.Warn("unmute", "err", err)
	}
	if err := cc.SetVolume(ctx, level); err != nil {
		s.log.Warn("set volume", "err", err)
	}
}

// noteVolumeSet records a volume we're about to set; level -1 keeps the
// last level (muting doesn't change it).
func (s *Speaker) noteVolumeSet(level float64, muted bool) {
	s.castMu.Lock()
	if level >= 0 {
		s.setLevel = level
	}
	s.setMuted, s.setAt, s.setPending = muted, time.Now(), true
	s.castMu.Unlock()
}

// castVolume handles the device volume from a RECEIVER_STATUS. If it isn't
// the echo of our own last set, someone changed it on the Cast side, and
// the AirPlay sender's slider is moved to match over the event channel (or
// DACP for senders that offer it). A set can take a
// second to apply, with statuses still showing the old level until then,
// so everything counts as an echo until one matches the set (or 5 s pass).
func (s *Speaker) castVolume(level float64, muted bool) {
	s.castMu.Lock()
	matches := muted == s.setMuted && (muted || math.Abs(level-s.setLevel) < 0.005)
	echo := s.setAt.IsZero() || matches
	if s.setPending {
		if matches || time.Since(s.setAt) > 5*time.Second {
			s.setPending = false
		} else {
			echo = true
		}
	}
	if !echo {
		s.setLevel, s.setMuted, s.setAt = level, muted, time.Now()
	}
	s.castMu.Unlock()
	if echo {
		return
	}
	vo := 0
	db := -144.0
	if !muted {
		pct := math.Min(1, level/s.cfg.VolumeScale)
		vo, db = int(math.Round(pct*100)), pct*30-30
	}
	s.mu.Lock()
	remote, events := s.remote, s.events
	from := -1.0
	if s.hasVol && s.volume > -144 {
		from = math.Max(0, s.volume/30+1)
	}
	s.pushedFrom, s.pushedTo, s.pushedAt = math.Max(-30, from*30-30), db, time.Now()
	if from < 0 {
		s.pushedFrom = db
	}
	// The sender will echo the new volume back as SET_PARAMETER; record it
	// now so a Cast session restart re-applies the right level.
	s.volume, s.hasVol = db, true
	s.mu.Unlock()
	s.log.Info("volume changed on Cast device", "cast", fmt.Sprintf("%.2f", level), "muted", muted, "airplay", vo)
	if events != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Muting moves the slider to the bottom; the sender has no
			// separate mute for a speaker.
			if err := events.SetVolume(ctx, from, float64(vo)/100); err != nil {
				s.log.Warn("send volume to AirPlay sender", "err", err)
			} else {
				s.log.Debug("sent volume to AirPlay sender", "from", from, "to", float64(vo)/100)
			}
		}()
		return
	}
	if remote == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := remote.SetVolume(ctx, vo); err != nil {
			s.log.Warn("send volume to AirPlay sender", "err", err)
		}
	}()
}

// SetDevice updates the Cast device's address; it takes effect on the next
// Cast session.
func (s *Speaker) SetDevice(d cast.Device) {
	s.castMu.Lock()
	s.cfg.Device = d
	s.castMu.Unlock()
}

// Close stops playback and releases the Cast device.
func (s *Speaker) Close() {
	s.mu.Lock()
	s.stopLocked()
	s.running = false
	if s.idle != nil {
		s.idle.Stop()
	}
	s.mu.Unlock()
	s.closeCast()
}
