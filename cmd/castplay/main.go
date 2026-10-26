// castplay bridges AirPlay 2 to Google Cast: every Cast speaker or group on
// the network appears as an AirPlay 2 speaker, and audio sent to it is
// transcoded to Opus and played over Cast Streaming.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"castplay/internal/airplay"
	"castplay/internal/ptp"
)

type options struct {
	stateDir    string
	rcRelay     bool
	iface       string
	basePort    int
	dumpDir     string
	name        string
	targetDelay time.Duration
	pipeline    time.Duration
	bitrate     int
	include     string
	exclude     string
	groups      bool
	volumeScale float64
	discover    time.Duration
	noPTP       bool
	verbose     bool
}

func main() {
	var o options
	flag.StringVar(&o.stateDir, "state", defaultStateDir(), "directory for persistent speaker identities")
	flag.StringVar(&o.iface, "iface", "", "network interface to advertise on (default: the one with the default route)")
	flag.IntVar(&o.basePort, "port", 7100, "first RTSP port; each speaker uses the next free one")
	flag.StringVar(&o.dumpDir, "dump", "", "debug: run one receiver that writes received audio to this directory instead of casting")
	flag.StringVar(&o.name, "name", "castplay", "speaker name in -dump mode")
	flag.DurationVar(&o.targetDelay, "target-delay", 100*time.Millisecond, "Cast Streaming receiver target delay")
	flag.DurationVar(&o.pipeline, "output-latency", 80*time.Millisecond, "speaker output latency after the receiver buffer; calibrated against Apple playout on a Home Max")
	flag.IntVar(&o.bitrate, "bitrate", 128, "Opus bitrate in kbit/s")
	flag.StringVar(&o.include, "include", "", "only bridge Cast devices whose name matches this regexp")
	flag.StringVar(&o.exclude, "exclude", "", "skip Cast devices whose name matches this regexp")
	flag.BoolVar(&o.groups, "groups", true, "also bridge Cast groups")
	flag.Float64Var(&o.volumeScale, "volume-scale", 1.0, "maximum Cast volume (0..1) that AirPlay full volume maps to")
	flag.DurationVar(&o.discover, "rediscover", 30*time.Second, "how often to look for new or vanished Cast devices")
	flag.BoolVar(&o.noPTP, "no-ptp", false, "don't follow sender PTP clocks (UDP 319/320); playback then starts on arrival")
	flag.BoolVar(&o.rcRelay, "rc-relay", false, "experimental: show what Cast speakers play natively (Spotify, YouTube Music, ...) on Apple devices, with playback control, via MediaRemote")
	flag.BoolVar(&o.verbose, "v", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ifc, ip, err := pickInterface(o.iface)
	if err != nil {
		fmt.Fprintln(os.Stderr, "castplay:", err)
		os.Exit(1)
	}
	log.Info("network", "interface", ifc.Name, "ip", ip)

	if o.dumpDir != "" {
		err = runDump(ctx, o, ifc, ip, log)
	} else {
		err = runBridge(ctx, o, ifc, ip, log)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "castplay:", err)
		os.Exit(1)
	}
}

func runDump(ctx context.Context, o options, ifc net.Interface, ip net.IP, log *slog.Logger) error {
	if err := os.MkdirAll(o.dumpDir, 0o755); err != nil {
		return err
	}
	id, err := loadIdentity(o.stateDir, "dump-"+o.name)
	if err != nil {
		return err
	}
	r := airplay.NewReceiver(airplay.Config{
		Name: o.name, DeviceID: id.DeviceID, GroupID: id.GroupID, Identity: id.hap(),
		Model: "castplay", Port: o.basePort, IP: ip, OutputLatency: o.targetDelay + o.pipeline,
		HomeFile: statePath(o.stateDir, "dump-"+o.name, ".home.json"), Log: log,
	}, &dumpPlayer{dir: o.dumpDir, log: log, ptp: startPTP(o, ip, id.DeviceID, log)})
	if err := r.Listen(); err != nil {
		return err
	}
	srv, err := r.Advertise([]net.Interface{ifc})
	if err != nil {
		return err
	}
	defer srv.Shutdown()
	log.Info("advertising", "name", o.name, "port", r.Port(), "deviceID", id.DeviceID)
	return r.Serve(ctx)
}

func defaultStateDir() string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" { // systemd
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "castplay")
	}
	return "state"
}

// pickInterface returns the named interface, or the one carrying the
// default route, with its IPv4 address.
func pickInterface(name string) (net.Interface, net.IP, error) {
	if name == "" {
		c, err := net.Dial("udp4", "192.0.2.1:9") // no packets are sent
		if err != nil {
			return net.Interface{}, nil, fmt.Errorf("find default interface: %w", err)
		}
		local := c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
		ifcs, _ := net.Interfaces()
		for _, ifc := range ifcs {
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.Equal(local) {
					return ifc, local, nil
				}
			}
		}
		return net.Interface{}, nil, fmt.Errorf("no interface has %s; pass -iface", local)
	}
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return net.Interface{}, nil, err
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return *ifc, n.IP.To4(), nil
		}
	}
	return net.Interface{}, nil, fmt.Errorf("interface %s has no IPv4 address", name)
}

// startPTP binds the PTP ports, or returns nil (with a warning) if that
// isn't possible, e.g. without privileges or when another PTP daemon runs.
func startPTP(o options, ip net.IP, deviceID string, log *slog.Logger) *ptp.Follower {
	if o.noPTP {
		return nil
	}
	f, err := ptp.Listen(nil, clockID(deviceID), log)
	if err != nil {
		log.Warn("PTP disabled", "err", err)
		return nil
	}
	return f
}

// clockID derives an EUI-64 style PTP clock identity from a MAC-style ID.
func clockID(mac string) uint64 {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return 0x0002030405060708
	}
	return uint64(hw[0])<<56 | uint64(hw[1])<<48 | uint64(hw[2])<<40 | 0xFFFE<<24 |
		uint64(hw[3])<<16 | uint64(hw[4])<<8 | uint64(hw[5])
}
