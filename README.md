# castplay

**Turn every Google Cast speaker into a real AirPlay 2 speaker.**

castplay runs on a small always-on Linux box on your network. Every Chromecast, Google Home, Nest speaker and Cast group it finds shows up in the AirPlay menu on your iPhone, iPad and Mac. It plays in sync with your HomePods and other AirPlay 2 speakers, can be added to the Apple Home app, and keeps volume and playback controls working in both directions.

## Features

**Plays like an AirPlay 2 speaker**
- **AirPlay 2, not AirPlay 1.** The speakers appear in the iOS and macOS AirPlay pickers next to HomePods, and you can pick several at once.
- **Multi-room sync.** castplay follows the sender's PTP clock and schedules every audio frame for the moment the sender asked for it. A Google Home Max measured within about 15 ms of the Mac playing the same track.
- **Low latency.** Audio goes over Cast Streaming, the protocol Chrome tab-casting uses, with a 100 ms buffer, rather than as an HTTP stream the speaker buffers for seconds. Play, pause and seek respond like any AirPlay speaker.
- **Add to the Home app**, like any AirPlay 2 speaker, and assign each one to a room.
- **One AirPlay speaker per Cast device and group**, discovered automatically. Each keeps a stable identity across restarts, so your devices don't re-pair.

**Controls both ways**
- **Volume:** the AirPlay slider sets the speaker's volume. Changing the volume in the Google Home app or on the speaker moves the slider on your iPhone or Mac.
- **Pause from the speaker:** pressing pause on the speaker or saying "Hey Google, pause" pauses the iPhone or Mac that is playing.
- **Takeover:** when a second iPhone picks the speaker, the newest sender wins, as on a HomePod.

**The speaker in Control Center (`-rc-relay`)**

Apple devices see the speaker the way they see a HomePod: the speaker's tile in Control Center and the Home app shows what it is playing, with artwork, and lets you control it.
- **Cast-native playback:** when the speaker plays Spotify Connect, YouTube Music or a "Hey Google, play…" request, your iPhone shows the track, with artwork, and can play, pause and skip it.
- **AirPlay playback:** when your Mac is AirPlaying to it, your iPhone shows the Mac's track and controls the Mac.
- **Speaker volume slider:** works from any Apple device, whatever is playing.

## How it compares

| | **castplay** | [AirConnect](https://github.com/philippe44/AirConnect) (aircast) | [Music Assistant](https://www.music-assistant.io/plugins/airplay-receiver/) AirPlay Receiver |
|---|---|---|---|
| Protocol the speakers appear as | AirPlay 2 | AirPlay 1 (RAOP) | AirPlay 2 (shairport-sync) |
| How audio reaches the Cast speaker | Cast Streaming (Opus, 100 ms buffer) | HTTP stream (FLAC, MP3, AAC or WAV) | HTTP stream from Music Assistant |
| In sync with HomePods and other AirPlay 2 speakers | ✅ follows the sender's PTP clock | ❌ ¹ | ❌ ² |
| Delay on play / pause / seek | Like any AirPlay 2 speaker | Seconds ¹ | About 5 s (documented) |
| Add to the Apple Home app | ✅ | ❌ (needs AirPlay 2) | Not documented |
| Volume: Google Home app or speaker → iPhone slider | ✅ | ✅ | Not documented |
| Pause on the speaker pauses the iPhone | ✅ | ✅ (also next / previous) | Not documented |
| Track info and artwork on the speaker's display | ❌ ³ | ✅ (MP3 / AAC) | ✅ |
| Cast-native playback (Spotify, YouTube Music…) shown and controlled on Apple devices | ✅ (`-rc-relay`) | ❌ | ❌ |
| Also bridges Sonos / UPnP | ❌ | ✅ (airupnp) | ✅ (any Music Assistant player) |
| Cast groups | Bridged, not yet tested | ✅ | ✅ |
| Runs on | Linux (Docker image) | Linux, macOS, Windows, BSD… | Music Assistant server (e.g. Home Assistant) |
| Maturity | New; tested on a Google Home Max | Mature, widely used | Alpha |

1. AirConnect's own README explains it relays real-time audio into an HTTP stream whose playback time it can't control. Its delay can grow over time as the sender's and speaker's clocks drift.
2. Music Assistant re-streams over HTTP to the Cast speaker, after its own buffering.
3. The Cast app castplay streams into has a fixed title and no media controls. In return, latency stays low enough for multi-room sync. Cast media mode would add 4–9 s.

**Pick castplay** if you mix Cast speakers with HomePods or AirPlay 2 speakers, want the speakers in the Home app, or care about latency. **Pick AirConnect** if you need Sonos/UPnP, a non-Linux host, or the track title on the speaker's own screen. **Pick Music Assistant** if you already run it and want AirPlay as one more source for all its players.

## Quick start

You need an always-on Linux machine on the same network as the speakers, with host networking for mDNS and PTP. It can't be the Mac you AirPlay from: both need UDP ports 319/320 for PTP.

```bash
docker run -d --name castplay --restart unless-stopped \
  --network host \
  -v castplay-state:/state \
  ghcr.io/1lann/castplay:main -rc-relay
```

Within a few seconds every Cast speaker shows up in the AirPlay menu. To put one in the Home app, add it the way you would any AirPlay 2 speaker.

The image includes ffmpeg and runs as an unprivileged user. It binds PTP's low ports with `CAP_NET_BIND_SERVICE`, which Docker grants by default. Speaker identities and Home pairings are kept in `/state`.

**Kubernetes.** Use `hostNetwork: true` with `dnsPolicy: ClusterFirstWithHostNet`. Mount a persistent volume at `/state` (owned by UID 1000), and keep the `NET_BIND_SERVICE` capability if you drop the rest.

**From source.** You need Go and `ffmpeg` built with libopus on `PATH`.

```bash
go build -o bin/castplay ./cmd/castplay
./bin/castplay -rc-relay
```

## Options

Most setups only need `-rc-relay`, plus `-iface` on hosts with several network interfaces. The recommended column is what the author runs at home: a Google Home Max on a k3s node, with every other option at its default.

| Option | Default | Recommended | What it does |
|---|---|---|---|
| `-rc-relay` | off | **on** | Apple devices show and control what the speaker plays, and get a working speaker volume slider. Off by default while it is new. |
| `-iface` | the interface with the default route | your LAN interface (e.g. `eno1`) | Where castplay advertises the speakers and listens for PTP. Set it if the host has Docker, VPN or other interfaces. |
| `-state` | `/state` in the image | default | Where speaker identities and Home pairings are kept. Keep it on persistent storage, or every restart looks like a new speaker. |
| `-target-delay` | `100ms` | default | The Cast receiver's buffer. Raise it, e.g. to `200ms`, if audio drops out on weak Wi-Fi; latency goes up by the same amount. |
| `-output-latency` | `80ms` | default | The speaker's own delay after that buffer, used for sync. Calibrated on a Google Home Max: raise it if the speaker plays ahead of your HomePods, lower it if it lags. |
| `-bitrate` | `128` | default | Opus bitrate in kbit/s. |
| `-volume-scale` | `1.0` | default | The Cast volume that full AirPlay volume maps to. Lower it, e.g. to `0.6`, if full volume is too loud. |
| `-include` / `-exclude` | all devices | unset | Regular expressions on Cast device names to bridge only some speakers. |
| `-groups` | `true` | default | Also bridge Cast speaker groups. |
| `-rediscover` | `30s` | default | How often to look for new or removed Cast devices. |
| `-port` | `7100` | default | First RTSP port; each speaker uses the next free one. |
| `-no-ptp` | off | off | Don't follow the sender's PTP clock: no UDP 319/320 needed, but no multi-room sync, and macOS Music won't start playing. |
| `-v` | off | off | Debug logging. |

## Known limitations

- **No track info on the Cast speaker itself.** The speaker's screen and the Google Home app show a generic title, and its next/previous buttons do nothing during AirPlay playback.
- **Linux host required**, and not the same machine as the sender (PTP ports).
- **Not yet tested:** Cast groups over Cast Streaming, speakers other than the Google Home Max, and realtime ALAC streams (e.g. macOS system audio). The sync calibration (`-output-latency`) may differ per speaker model.
- **`-rc-relay`:** next/previous on Cast apps other than Spotify, and seeking from an iPhone, are untested.

## How it works

castplay implements an AirPlay 2 receiver: mDNS, HomeKit pairing, the encrypted RTSP control channel, and buffered and realtime audio. It decodes the sender's AAC, ALAC or PCM audio with ffmpeg, re-encodes it as Opus and sends it over Cast Streaming. Each frame is timestamped so it plays at the time the sender's PTP clock asked for. For `-rc-relay`, it speaks MediaRemote, the protocol HomePods use for now-playing, over AirPlay's remote control channel.

The design, measurements and protocol notes are in [DESIGN.md](DESIGN.md).
