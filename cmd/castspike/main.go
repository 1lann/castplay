// castspike measures how long Google Cast receivers take to play a live HTTP
// audio stream, comparing AAC, WAV and FLAC (castplay P0 spike).
//
// It serves a paced test signal (a self-identifying beep every second), tells
// a Cast device to LOAD it, and listens through the microphone to timestamp
// when each beep is actually heard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"castplay/internal/cast"
)

type options struct {
	device      string
	addr        string
	list        bool
	discover    time.Duration
	codecs      []string
	duration    time.Duration
	gap         time.Duration
	streamType  string
	httpLen     string
	lead        time.Duration
	burst       time.Duration
	noiseDB     float64
	flacBlock   int
	aacBitrate  string
	aacEncoder  string
	opusBitrate string
	volume      float64
	micInput    string
	appID       string
	port        int
	outDir      string
	selftest    bool
	loopback    bool
	verbose     bool
}

func parseFlags() options {
	var o options
	var codecs string
	flag.StringVar(&o.device, "device", "", "Cast device or group name (case-insensitive substring)")
	flag.StringVar(&o.addr, "addr", "", "Cast device host:port, skipping discovery")
	flag.BoolVar(&o.list, "list", false, "list Cast devices and exit")
	flag.DurationVar(&o.discover, "discover-timeout", 3*time.Second, "mDNS browse time")
	flag.StringVar(&codecs, "codec", "aac,wav,flac", "comma-separated runs: aac, wav, flac (HTTP media LOAD) or rt:<ms> (Cast Streaming Opus with that receiver target, e.g. rt:20,rt:100)")
	flag.DurationVar(&o.duration, "duration", 30*time.Second, "how long to play each codec")
	flag.DurationVar(&o.gap, "gap", 3*time.Second, "pause between codec runs")
	flag.StringVar(&o.streamType, "stream-type", "LIVE", "Cast streamType: LIVE or BUFFERED")
	flag.StringVar(&o.httpLen, "http-len", "chunked", "HTTP body framing: chunked, close (no length) or fixed (dummy Content-Length)")
	flag.DurationVar(&o.lead, "lead", 0, "publish audio this far ahead of real time (like AirPlay buffered senders)")
	flag.DurationVar(&o.burst, "burst", 0, "backlog sent to a new connection to fill the player faster")
	flag.Float64Var(&o.noiseDB, "noise", 0, "white-noise bed under the beeps in dBFS, e.g. -40 (0 = digital silence)")
	flag.IntVar(&o.flacBlock, "flac-block", 1152, "FLAC block size in samples")
	flag.StringVar(&o.aacBitrate, "aac-bitrate", "256k", "AAC bitrate")
	flag.StringVar(&o.opusBitrate, "opus-bitrate", "128k", "Opus bitrate for Cast Streaming (rt:<ms>) runs")
	flag.StringVar(&o.aacEncoder, "aac-encoder", "aac", "ffmpeg AAC encoder: aac or aac_at (AudioToolbox, macOS)")
	flag.Float64Var(&o.volume, "volume", -1, "set device volume 0..1 before playing (-1 leaves it alone)")
	flag.StringVar(&o.micInput, "mic", defaultMic(), "microphone: macOS input device name substring (miccap), or ffmpeg input elsewhere; empty disables acoustic measurement")
	flag.StringVar(&o.appID, "app", cast.DefaultMediaReceiver, "Cast receiver app ID")
	flag.IntVar(&o.port, "port", 0, "HTTP listen port (0 = random)")
	flag.StringVar(&o.outDir, "out", "", "directory for per-beep CSV files")
	flag.BoolVar(&o.selftest, "selftest", false, "validate encoders and the detector locally via ffmpeg, no Cast device needed")
	flag.BoolVar(&o.loopback, "loopback", false, "validate the microphone path by playing the signal on this machine's speaker (macOS)")
	flag.BoolVar(&o.verbose, "v", false, "debug logging (includes all Cast messages)")
	flag.Parse()
	for _, c := range strings.Split(codecs, ",") {
		if c = strings.TrimSpace(strings.ToLower(c)); c != "" {
			o.codecs = append(o.codecs, c)
		}
	}
	o.streamType = strings.ToUpper(o.streamType)
	if o.noiseDB < 0 {
		noiseAmp = math.Pow(10, o.noiseDB/20)
	}
	return o
}

func defaultMic() string {
	switch runtime.GOOS {
	case "darwin":
		return "default" // miccap device-name substring; "default" = system input
	case "linux":
		return "default"
	}
	return ""
}

func main() {
	o := parseFlags()
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch {
	case o.selftest:
		err = selftest(ctx, o, log)
	case o.loopback:
		err = loopback(ctx, o, log)
	case o.list:
		err = listDevices(ctx, o)
	default:
		err = measure(ctx, o, log)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "castspike:", err)
		os.Exit(1)
	}
}

func listDevices(ctx context.Context, o options) error {
	dctx, cancel := context.WithTimeout(ctx, o.discover)
	defer cancel()
	devs, err := cast.Discover(dctx)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no Cast devices found")
	}
	for _, d := range devs {
		fmt.Println(d)
	}
	return nil
}

func resolveDevice(ctx context.Context, o options) (cast.Device, error) {
	if o.addr != "" {
		if _, _, err := net.SplitHostPort(o.addr); err != nil {
			return cast.Device{Name: o.addr, Addr: net.JoinHostPort(o.addr, "8009")}, nil
		}
		return cast.Device{Name: o.addr, Addr: o.addr}, nil
	}
	if o.device == "" {
		return cast.Device{}, errors.New("pass -device <name> or -addr <host:port> (use -list to see devices)")
	}
	dctx, cancel := context.WithTimeout(ctx, o.discover)
	defer cancel()
	devs, err := cast.Discover(dctx)
	if err != nil {
		return cast.Device{}, err
	}
	var matches []cast.Device
	for _, d := range devs {
		if strings.EqualFold(d.Name, o.device) {
			return d, nil
		}
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(o.device)) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return cast.Device{}, fmt.Errorf("no Cast device matching %q (found %d devices; try -list)", o.device, len(devs))
	case 1:
		return matches[0], nil
	}
	names := make([]string, len(matches))
	for i, d := range matches {
		names[i] = d.Name
	}
	return cast.Device{}, fmt.Errorf("%q is ambiguous: %s", o.device, strings.Join(names, ", "))
}

// server serves whichever liveStream is current.
type server struct {
	ln  net.Listener
	cur atomic.Pointer[liveStream]
}

func startServer(port int) (*server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	s := &server{ln: ln}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ls := s.cur.Load(); ls != nil {
			ls.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	return s, nil
}

func (s *server) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// statusSample is one MEDIA_STATUS observation.
type statusSample struct {
	at    time.Time
	state string
	ct    float64
}

func measure(ctx context.Context, o options, log *slog.Logger) error {
	dev, err := resolveDevice(ctx, o)
	if err != nil {
		return err
	}
	fmt.Printf("device: %s (%s) at %s\n", dev.Name, dev.Model, dev.Addr)

	var m *mic
	if o.micInput != "" {
		if m, err = startMic(ctx, o.micInput, log); err != nil {
			return err
		}
		if err := m.waitReady(5 * time.Second); err != nil {
			return err
		}
		fmt.Println("microphone: capturing")
	} else {
		fmt.Println("microphone: disabled; only control-plane timings and MEDIA_STATUS estimates will be reported")
	}

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	c, err := cast.Dial(dctx, dev.Addr, log)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()

	var evMu sync.Mutex
	var events []statusSample
	c.OnMediaStatus(func(st cast.MediaStatus, at time.Time) {
		evMu.Lock()
		events = append(events, statusSample{at: at, state: st.PlayerState, ct: st.CurrentTime})
		evMu.Unlock()
		log.Debug("media status", "state", st.PlayerState, "currentTime", st.CurrentTime, "idle", st.IdleReason)
	})

	if o.volume >= 0 {
		vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.SetVolume(vctx, o.volume)
		cancel()
		if err != nil {
			return fmt.Errorf("set volume: %w", err)
		}
	}
	srv, err := startServer(o.port)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(c.LocalAddr().String())

	var results []result
	for i, codec := range o.codecs {
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			time.Sleep(o.gap)
		}
		delay, isRT, err := rtDelay(codec)
		if err != nil {
			return err
		}
		var r result
		if isRT {
			fmt.Printf("\n== %s: Cast Streaming, Opus %s, receiver target %s\n", codec, o.opusBitrate, delay)
			r, err = runStreaming(ctx, c, dev, delay, o, log)
		} else {
			evMu.Lock()
			events = events[:0]
			evMu.Unlock()
			r, err = runHTTP(ctx, c, srv, host, codec, o, log)
			evMu.Lock()
			r.events = append([]statusSample(nil), events...)
			evMu.Unlock()
		}
		if err != nil {
			if errors.Is(err, errConnLost) {
				return err
			}
			fmt.Printf("   failed: %v\n", err)
			r.codec, r.err = codec, err
			results = append(results, r)
			continue
		}

		// Give the mic a moment to deliver the tail of the capture.
		time.Sleep(300 * time.Millisecond)
		if m != nil {
			r.mic = true
			r.heard = m.heardBetween(r.loadSent, r.stopped.Add(time.Second))
		}
		r.analyse()
		r.printDetail()
		if o.outDir != "" {
			if err := r.writeCSV(o.outDir); err != nil {
				log.Warn("csv", "err", err)
			}
		}
		results = append(results, r)
	}
	printSummary(o, dev, results)
	return nil
}

var errConnLost = errors.New("cast connection lost")

// runHTTP plays one codec through the media namespace: LOAD a live HTTP
// stream on the configured receiver app and poll MEDIA_STATUS while it plays.
func runHTTP(ctx context.Context, c *cast.Client, srv *server, host, codec string, o options, log *slog.Logger) (result, error) {
	r := result{codec: codec}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := c.Launch(lctx, o.appID)
	cancel()
	if err != nil {
		return r, err
	}
	src, err := newSource(codec, o)
	if err != nil {
		return r, err
	}
	ls := newLiveStream(src, o, log)
	srv.cur.Store(ls)
	url := fmt.Sprintf("http://%s/%s", net.JoinHostPort(host, fmt.Sprint(srv.port())), strings.TrimPrefix(ls.path, "/"))
	fmt.Printf("\n== %s: %s\n", codec, url)

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	go ls.run(runCtx)
	if o.burst > 0 {
		time.Sleep(o.burst)
	}

	r.loadSent = time.Now()
	ldctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, reply, err := c.Load(ldctx, cast.LoadRequest{URL: url, ContentType: src.contentType(), StreamType: o.streamType, Title: "castspike " + codec})
	cancel()
	if err != nil {
		return r, fmt.Errorf("LOAD: %w", err)
	}
	r.loadAck = reply.At

	end := r.loadSent.Add(o.duration)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for now := time.Now(); now.Before(end) && ctx.Err() == nil; now = time.Now() {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, rep, err := c.Status(sctx)
		cancel()
		if err == nil {
			r.status = append(r.status, statusSample{at: rep.At, state: st.PlayerState, ct: st.CurrentTime})
			if st.PlayerState == "IDLE" {
				fmt.Printf("   player went IDLE early\n")
				break
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
		case <-c.Done():
			return r, errConnLost
		}
	}
	r.stopped = time.Now()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = c.Stop(sctx)
	cancel()
	stopRun()
	r.t0 = ls.start()
	r.lead = o.lead
	r.conns = ls.connections()
	r.maxLate = ls.maxLate
	return r, nil
}
