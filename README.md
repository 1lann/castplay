# castplay

AirPlay 2 → Google Cast bridge. Every Cast speaker on the network shows up as an AirPlay 2 speaker. Audio is transcoded to Opus and played over Cast Streaming, scheduled from the sender's PTP clock so it stays in sync with Apple speakers in the same AirPlay group.

Design and measurements are in [DESIGN.md](DESIGN.md); §9 covers current status and gaps.

## Running

Needs Linux (or another machine than the AirPlay sender), `ffmpeg` with libopus, and permission to bind UDP 319/320 for PTP (root or `CAP_NET_BIND_SERVICE`). On Kubernetes, use `hostNetwork: true` for mDNS and PTP.

```bash
go build -o bin/castplay ./cmd/castplay
./bin/castplay                                  # bridge every Cast device and group
./bin/castplay -include '^Living Room' -v       # just some devices, debug logging
./bin/castplay -dump /tmp/airplay -name test    # debug: receive AirPlay into files, no Cast
```

Main options:

- `-target-delay` (default 100 ms): the Cast receiver buffer.
- `-output-latency` (default 80 ms): the speaker's own delay, for sync.
- `-volume-scale`
- `-iface`: the interface to advertise on (default: the one with the default route).
- `-state`: where speaker identities are kept, so senders see the same device after a restart.

## P0 spike: `castspike`

Measures how long a Cast receiver takes to play live audio, two ways:

- HTTP media LOAD: AAC, WAV or FLAC.
- Cast Streaming: Opus over encrypted RTP, `rt:<ms>`, with the transport adapted from [shoutout](https://github.com/lkarlslund/shoutout).

Results so far are in DESIGN.md §8.

It serves a paced test signal: a 60 ms beep every second, and each beep's pitch encodes its index mod 16. It then LOADs that stream on a Cast device and timestamps every beep it hears through the microphone.

```bash
go build -o bin/castspike ./cmd/castspike
./bin/castspike -selftest                      # no device needed: checks encoders + detector via ffmpeg
./bin/castspike -list                          # find devices and groups
swiftc -O tools/miccap/main.swift -o bin/miccap   # macOS mic capture with real timestamps
./bin/castspike -loopback -mic MacBook         # validates the mic path via the Mac's own speaker
./bin/castspike -device "Living Room" -volume 0.4 -mic MacBook -noise -40 -codec aac,wav,flac,rt:50,rt:100 -out results
```

Use `-noise -40`. Receivers buffer HTTP by bytes, and a digital-silence signal compresses so well that AAC/FLAC starve. On macOS the mic goes through `bin/miccap`, because ffmpeg's avfoundation input drops ~10% of buffers and corrupts timing.

Requires `ffmpeg` on PATH, with libopus for `rt:` runs. It's used to pre-encode AAC/Opus, and to capture the mic on Linux. On macOS, the terminal running it needs microphone permission. Put the mic within a few metres of the speaker.

Reported per codec, all measured from sending LOAD:

- **load ack**: when the receiver replied to LOAD. Receivers often hold the reply until they have buffered.
- **1st req**: when the receiver first requested our HTTP stream.
- **playing**: when the receiver first reported PLAYING.
- **1st sound**: when the first served sample was actually heard. This is press-to-sound.
- **lat**: steady-state latency, from a sample going out on the live stream to it being heard. Reported as median, p10, p90, and min.
- **drift**: how fast that latency changes, in ms/min. This is the receiver's clock vs ours.
- **status est**: the same latency inferred from `MEDIA_STATUS.currentTime`. If this matches **lat**, P3 can track Cast delay without a mic.

Knobs to explore:

- `-stream-type BUFFERED`
- `-http-len close|fixed`: like aircast's `-g`.
- `-burst 2s`: backlog sent on connect, which trades latency for faster startup.
- `-lead 1s`: publish ahead of real time, like AirPlay buffered senders.
- `-flac-block 4096`
- `-aac-encoder aac_at`: Apple's encoder.
- `-app <id>`: a custom receiver.

Accuracy: the mic timestamps are bounded to within the input device's latency, a few ms on built-in mics. The detector's offset is about −1.5 ms (`-selftest` reports it). Beep identity is unambiguous only up to 16 s of latency.
