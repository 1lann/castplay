package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"time"

	"github.com/grandcat/zeroconf"

	"castplay/internal/airplay"
	"castplay/internal/bridge"
	"castplay/internal/cast"
	"castplay/internal/mrp"
	"castplay/internal/ptp"
)

// bridged is one Cast device exposed as an AirPlay speaker.
type bridged struct {
	dev     cast.Device
	speaker *bridge.Speaker
	remote  *bridge.RemoteControl // nil without -rc-relay
	mdns    *zeroconf.Server
	stop    context.CancelFunc
	missing int // consecutive discovery rounds without the device
}

func runBridge(ctx context.Context, o options, ifc net.Interface, ip net.IP, log *slog.Logger) error {
	var include, exclude *regexp.Regexp
	var err error
	if o.include != "" {
		if include, err = regexp.Compile("(?i)" + o.include); err != nil {
			return fmt.Errorf("-include: %w", err)
		}
	}
	if o.exclude != "" {
		if exclude, err = regexp.Compile("(?i)" + o.exclude); err != nil {
			return fmt.Errorf("-exclude: %w", err)
		}
	}
	var follower *ptp.Follower
	if !o.noPTP {
		follower = startPTP(o, ip, "02:00:00:00:00:01", log)
		if follower != nil {
			defer follower.Close()
		}
	}

	speakers := map[string]*bridged{} // by Cast device ID
	defer func() {
		for _, b := range speakers {
			b.close()
		}
	}()
	nextPort := o.basePort
	for {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		devs, err := cast.Discover(dctx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Warn("cast discovery", "err", err)
		}
		seen := map[string]bool{}
		for _, d := range devs {
			key := d.ID
			if key == "" {
				key = d.Name
			}
			if (d.IsGroup() && !o.groups) || (include != nil && !include.MatchString(d.Name)) || (exclude != nil && exclude.MatchString(d.Name)) {
				continue
			}
			seen[key] = true
			if b := speakers[key]; b != nil {
				b.missing = 0
				if b.dev.Addr != d.Addr {
					log.Info("cast device moved", "name", d.Name, "addr", d.Addr)
					b.speaker.SetDevice(d)
					if b.remote != nil {
						b.remote.SetDevice(d)
					}
					b.dev = d
				}
				continue
			}
			b, port, err := startSpeaker(ctx, o, d, key, ifc, ip, nextPort, follower, log)
			if err != nil {
				log.Error("bridge speaker", "name", d.Name, "err", err)
				continue
			}
			nextPort = port + 1
			speakers[key] = b
		}
		for key, b := range speakers {
			if !seen[key] {
				b.missing++
				if b.missing >= 3 {
					log.Info("cast device gone", "name", b.dev.Name)
					b.close()
					delete(speakers, key)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.discover):
		}
	}
}

func startSpeaker(ctx context.Context, o options, d cast.Device, key string, ifc net.Interface, ip net.IP,
	port int, follower *ptp.Follower, log *slog.Logger) (*bridged, int, error) {
	id, err := loadIdentity(o.stateDir, key)
	if err != nil {
		return nil, 0, err
	}
	sp := bridge.NewSpeaker(bridge.Config{
		Device: d, TargetDelay: o.targetDelay, OutputLatency: o.pipeline,
		BitrateKbps: o.bitrate, VolumeScale: o.volumeScale, PTP: follower, Log: log,
	})
	model := "Google Cast"
	if d.Model != "" {
		model = d.Model
	}
	sctx, stop := context.WithCancel(ctx)
	var rc *bridge.RemoteControl
	var rcIface airplay.RemoteControl
	if o.rcRelay {
		rc = bridge.NewRemoteControl(d, mrp.DeviceInfo{
			UniqueIdentifier: id.hap().PairingID, Name: d.Name, ModelName: model, ModelID: model, GroupID: id.GroupID,
		}, sp, log)
		rcIface = rc
		go rc.Run(sctx)
	}
	var r *airplay.Receiver
	for ; port < o.basePort+1000; port++ {
		r = airplay.NewReceiver(airplay.Config{
			Name: d.Name, DeviceID: id.DeviceID, GroupID: id.GroupID, Identity: id.hap(),
			Model: model, Port: port, IP: ip, OutputLatency: o.targetDelay + o.pipeline,
			HomeFile: statePath(o.stateDir, key, ".home.json"), RemoteControl: rcIface, Log: log,
		}, sp)
		if err = r.Listen(); err == nil {
			break
		}
	}
	if err != nil {
		stop()
		return nil, 0, fmt.Errorf("no free port: %w", err)
	}
	mdns, err := r.Advertise([]net.Interface{ifc})
	if err != nil {
		stop()
		return nil, 0, err
	}
	go func() {
		if err := r.Serve(sctx); err != nil {
			log.Error("airplay receiver", "name", d.Name, "err", err)
		}
	}()
	log.Info("bridging", "name", d.Name, "model", d.Model, "cast", d.Addr, "airplayPort", r.Port(), "deviceID", id.DeviceID)
	return &bridged{dev: d, speaker: sp, remote: rc, mdns: mdns, stop: stop}, r.Port(), nil
}

func (b *bridged) close() {
	b.mdns.Shutdown()
	b.stop()
	b.speaker.Close()
}
