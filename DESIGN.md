# castplay — viability & feature sketch

An AirPlay 2 receiver, written in Go, that bridges to Google Cast speakers and groups.
It plays the same role as philippe44's `aircast` (part of airconnect), but on the AirPlay 2 side, and adds timing awareness.

References surveyed:

| Repo | What it is | What we take from it |
|---|---|---|
| `airconnect/aircast` + `common/libraop` | AirPlay **1** receiver → Cast bridge (C) | Cast control flow, HTTP streaming, latency/drift/silence knobs, field-tested quirks |
| `airplay2-receiver` | AirPlay **2** receiver (Python) | Receiver side: `/info`, feature bits, HAP pairing, SETUP plists, buffered/realtime RTP, SETRATEANCHORTIME, SETPEERS |
| `airplay2-rs` | AirPlay **2** sender (Rust) | Protocol spec (`AIRPLAY_2_SPEC.md`), working PTP/gPTP analysis (`PTP analysis/`), jitter lessons (`JITTER.md`) |

Also worth reading but not in the workspace: shairport-sync + nqptp (the reference AP2 receiver and PTP follower), and `openairplay/goplay2` (an earlier Go AP2 receiver; I haven't checked its current state).

---

## 1. Verdict

**Viable, and Go fits well.** The AirPlay 2 receiver side is now well understood. Every crypto primitive it needs is in `golang.org/x/crypto`, and the Cast v2 protocol is small (aircast does it in about 1,100 lines of C).

> **Updated after the P0 spike (§8):** use **Cast Streaming**, the mirroring protocol that [shoutout](https://github.com/lkarlslund/shoutout) uses, not media LOAD. On a Google Home Max it measured **~170–220 ms** steady latency, against 4–9 s for HTTP. It also **schedules playout from sender timestamps**, which gives us a real "play at time T" primitive. The original reasoning below assumed HTTP media LOAD and is kept for context.

The hard limit is on the **Cast side, not AirPlay**:

- Google's media API takes a URL and plays it whenever it decides to. There is no "play this sample at time T" call, and the Default Media Receiver buffers HTTP audio for 4–9 s (measured: §8).
- So "minimal latency" depends almost entirely on **which Cast receiver and protocol we use** (see §4).
- ~~"Clock sync" in the sense of sample-accurate PTP alignment with HomePods is not achievable through Cast.~~ Cast Streaming honours RTP/RTCP sender timestamps, so scheduled playout is possible. There is a per-session offset of about ±20 ms that still needs calibrating (§8).

What Cast does give us for free: **Cast groups sync among themselves.** If we expose a Google Cast group as a single AirPlay speaker, all the Google speakers in it stay in tight sync. This is untested for Cast Streaming.

---

## 2. Key design insight: AAC passthrough (no transcoding, no cgo)

iOS music apps send AirPlay 2 **buffered audio** (RTP type 103, TCP) as **AAC-LC 44.1 kHz stereo**, encrypted per packet with ChaCha20-Poly1305. Google Cast plays `audio/aac` natively.

So the default pipeline is: **decrypt → add an ADTS header to each 1024-sample access unit → serve over HTTP.**

- No decoder, no encoder, effectively zero CPU, and no codec delay.
- No cgo, so it builds as a single static binary that cross-compiles to arm/arm64/amd64. This is a real advantage over airconnect's per-platform builds.
- Silence fill (pause, underrun) uses a precomputed silent AAC frame.
- The cost: we can only adjust timing in whole frames (~23 ms). Fine-grained sync needs the PCM path below.

**PCM path**, needed for realtime streams and for precise sync:

| Source | Decode | Output to Cast |
|---|---|---|
| Realtime type 96, ALAC (macOS system audio, some apps) | ALAC → PCM (port `libraop/src/alac.c`, ~1.1k lines, or find a Go lib) | WAV/LPCM (zero encode delay, ~1.4 Mbps) or FLAC (`mewkiz/flac`, blocksize 1152) |
| Buffered AAC when fine sync/resampling is wanted | AAC decode (cgo fdk-aac, so optional build tag) | WAV/FLAC |
| AAC-ELD (screen-mirroring audio) | out of scope initially | — |

Cast doesn't support ALAC, so the ALAC path always transcodes.

---

## 3. Architecture

```
                 ┌──────────────── castplay (one process) ─────────────────┐
 iPhone/Mac ──►  │ mDNS (_airplay._tcp + _raop._tcp, one identity per bridge)│
   RTSP/HTTP ──► │ ap2/rtsp  ──► ap2/pairing (HAP transient / normal)        │
   RTP audio ──► │ ap2/stream (buffered TCP 103 / realtime UDP 96, decrypt)  │
   PTP 319/320 ► │ ptp (one gPTP follower, shared by all bridges)            │
                 │      │                                                    │
                 │      ▼                                                    │
                 │ bridge  ── jitter buffer, anchor scheduling, drift ctl ──┐│
                 │      │                                                   ││
                 │      ▼                                                   ▼│
                 │ httpstream (LIVE AAC/WAV/FLAC per session) ◄── Cast pulls  │
                 │ cast (TLS:8009 protobuf, LAUNCH/LOAD/VOLUME/MEDIA_STATUS) ─┼─► Google speaker / group
                 └───────────────────────────────────────────────────────────┘
```

Suggested packages:

- `mdns`: advertise and browse (`brutella/dnssd` handles multiple services and probing well).
- `ap2/rtsp`: RTSP-over-HTTP server and the post-pairing encrypted framing.
- `ap2/pairing`: SRP-6a (3072-bit) and TLV8 for HAP transient pairing (PIN 3939) and pair-verify. `brutella/hap` has reusable pieces; `airplay2-receiver/ap2/pairing/hap.py` is the receiver-side reference.
- `ap2/stream`: SETUP phases 1 and 2 (binary plists via `howett.net/plist`), audio receive and decrypt, FLUSH and SETRATEANCHORTIME.
- `ptp`: a gPTP unicast follower. nqptp is the model; the pcap analysis in `airplay2-rs/PTP analysis/` shows the exact message flow.
- `codec`: ADTS framing, ALAC decode, WAV/FLAC writers.
- `cast`: a Cast v2 client (TLS, protobuf `CastMessage`, heartbeat, receiver and media namespaces). Mirror `aircast/src/castcore.c` + `cast_util.c`, or use `vishen/go-chromecast` as a base.
- `bridge`: one goroutine tree per Cast target, wiring the pieces above together.

Each Cast device or group gets its own AirPlay identity: device ID, Ed25519 long-term key, port, and persisted name and pairings. One process can run dozens of them cheaply. That's the natural shape for goroutines, compared with aircast's thread-per-device.

### Features to advertise

Follow `airplay2-receiver/ap2/bitflags.py`:

- Bit 40 (BufferedAudio), 41 (PTPClock), 30 (UnifiedAdvertisingInfo), and 48 (transient pairing).
- `audioLatencies` in `/info`.
- `timingPeerInfo` in SETUP.
- No video or screen bits.

**PTP (bit 41) is what lets iOS put us in a multi-room group** next to HomePods. Without it we're a lone target.

---

## 4. Latency: where it goes and what we can do

| Stage | Typical | Control we have |
|---|---|---|
| AirPlay sender pre-roll | buffered mode sends seconds ahead; anchor sets play time | We can forward early (low latency) or honor the anchor (sync) |
| castplay processing | <5 ms (passthrough) | ✅ |
| HTTP → Cast receiver buffer | **1–4 s with Default Media Receiver** (`CC1AD845`) | ⚠️ depends on receiver app |
| Cast DAC / group distribution | ~100–200 ms; groups add their own buffer | ❌ |

Three receiver strategies, in order of effort:

1. **Default Media Receiver, LIVE stream** (what aircast does). Zero setup. Expect about 2–4 s from press to sound. Useful immediately.
2. **Custom Web Receiver (CAF)** with a tuned media-player config: small buffer, LIVE, low-latency mode. Needs a Cast developer registration (one-time fee) and an HTTPS-hosted receiver page. Plausibly about 0.5–1 s.
3. **Custom receiver that bypasses the media element**: stream PCM/Opus over a WebSocket into WebAudio. That gives explicit scheduling, so the receiver can play at a timestamp we send and correct drift itself. Best case is a few hundred ms, plus actual timed playback. **Unknowns to spike first:**
   - Do audio-only Nest/Home speakers run custom receivers with WebAudio?
   - Does mixed content (an HTTPS page talking to a LAN `ws://`) work?
   - Does this work inside Cast groups at all? Probably not, because groups replicate media-element playback.

**Recommendation:** ship option 1, and spike options 2 and 3 on a real Nest Audio before investing more.

### Keep the Cast session warm

The biggest UX latency in aircast is re-LOADing: each LOAD costs seconds. airconnect's README documents FLUSH → STOP → PLAY problems.

- Keep **one continuous LIVE HTTP stream per AirPlay session** across track changes.
- Fill gaps with silence frames.
- Only re-LOAD on a real FLUSH (skip or seek), where stale buffered audio would otherwise play.
- Pause maps to Cast PAUSE for instant silence. Resume within the same position maps to PLAY.
- This trade-off (responsiveness vs. stale buffer) is what gets tuned.

---

## 5. Clock synchronization: what's realistic

1. **PTP follower.** Required to interpret SETRATEANCHORTIME (`rtpTime` ↔ `networkTime` in the master clock's timeline) and to join multi-room groups.
   - Bind 319/320, which needs `CAP_NET_BIND_SERVICE` or root.
   - It conflicts with other PTP daemons, and with the macOS "AirPlay Receiver". Plan for Linux/Docker `--net=host` deployments, and turn off AirPlay Receiver when developing on a Mac.
   - The Python receiver fakes this part (it anchors to local monotonic time at receipt), so it's no help here. Use nqptp and `airplay2-rs/PTP analysis/` instead.
2. **Anchor-aligned release.** Hold each frame until `anchor_time(frame) − castPipelineDelay`, then write it to the HTTP stream.
3. **Measure `castPipelineDelay`.** Compare bytes released against `MEDIA_STATUS.currentTime` (poll at ~1 Hz) to estimate how far behind the Cast player is. Refine with a one-time acoustic calibration (a phone mic picks up a click on a HomePod and on the Nest), stored per device.
4. **Drift.** The Cast DAC clock differs from the PTP master by up to tens of ppm (≈ 3 ms/min at 50 ppm). Once the Cast buffer is full, the **rate it pulls HTTP bytes tracks its playback clock**, which gives a free drift signal. Correct it by:
   - dropping or inserting frames (passthrough: 23 ms steps, so only when far off), or
   - resampling (PCM path; see `airplay2-rs/crates/airplay-resampler`).
5. **Report latency back.** Put the measured total in `audioLatencies` so iOS delays video accordingly. airconnect says A/V sync "cannot work" because Cast's buffer is opaque and variable. With measured delay plus a custom receiver it might be close enough for casual video. Treat this as an experiment, not a promise.

**Expected outcomes:**

- Google speakers within one Cast group: tight (Google handles it).
- Google group vs. HomePods in the same AirPlay multi-room session: ~±30–80 ms with the default receiver after calibration, wandering as Cast re-buffers.
- Custom receiver (option 3): potentially <20 ms.

---

## 6. Feature list (phased)

**P0: spikes (de-risk before building)**
- Go Cast client: LOAD an HTTP LIVE stream; measure press-to-sound for AAC vs. WAV vs. FLAC on Nest Audio, a Chromecast, and a group.
- Custom receiver smoke test on an audio-only speaker (WebAudio + WebSocket).

**P1: AirPlay 2 receiver core**
- mDNS, `/info`, HAP transient pairing and pair-verify, encrypted RTSP, SETUP (phases 1 and 2).
- Buffered AAC receive and decrypt → ADTS file. Validate with Apple Music on iOS.

**P2: the bridge (MVP, parity with aircast)**
- Auto-discover `_googlecast._tcp` devices and groups. Group TXT `md=Google Cast Group` uses a non-8009 port.
- One AirPlay speaker per Cast target.
- AAC passthrough over HTTP LIVE; warm sessions; pause/flush semantics.
- Volume: AirPlay dB (−30..0, −144 = mute) → Cast `SET_VOLUME`, with a media-volume factor as in aircast `-v`. Sync volume changes made in the Google app back to AirPlay.
- Silence fill, reconnect on Cast drop, heartbeats.
- Config: include/exclude devices, rename, codec choice, latency knobs. Keep aircast's semantics where they make sense.

**P3: timing**
- PTP follower, anchor-aligned release, drift measurement and correction, latency reporting, per-device calibration.

**P4: breadth**
- Realtime ALAC path (Mac "AirPlay to…" system audio) → WAV/FLAC.
- Metadata and artwork: DMAP/`image/*` via SET_PARAMETER, sent to Cast LOAD/queue metadata. Shows on Nest Hubs.
- Normal (non-transient) pairing, so bridged speakers can be added to the Home app and Siri. Persist pairings.
- Remote control back to the sender (play/pause/next from the Google side) via DACP/MediaRemote. Stretch goal.
- Custom Cast receiver (§4 option 2 or 3) if the spikes pass.

**Explicitly out of scope:** video/screen mirroring, FairPlay (not needed for AP2 with HAP pairing; the Python repo has a FairPlay v3 port if some sender ever demands it), and MFi auth.

---

## 7. Risks

| Risk | Severity | Mitigation |
|---|---|---|
| Cast receiver buffering sets a latency floor we can't control | **High** | Custom receiver spikes (P0); set expectations in docs |
| Custom receivers unsupported/limited on audio-only speakers or groups | High | Verify in P0; keep the default receiver as fallback |
| iOS updates change AP2 pairing/feature expectations | Medium | Track shairport-sync changes; keep the feature set close to theirs |
| PTP port conflicts / privileges | Medium | Document deployment; one shared follower per process |
| Pause/skip feel sluggish due to Cast buffer | Medium | Warm session + immediate Cast PAUSE + re-LOAD only on FLUSH |
| AAC frame-granular drift correction is audible | Low | Correct rarely; PCM+resample mode when sync matters |

---

## 8. P0 spike results (2026-10-25)

Tool: `cmd/castspike` (see README). It plays a self-identifying beep track and timestamps each beep acoustically with the MacBook mic through `tools/miccap`. Validation:

- **Loopback:** 3 ms spread, ~0 drift.
- **Selftest:** onset error ≤2.3 ms for every encoder.
- **Opus timeline test:** ±4 ms.

Device: Google Home Max ("Living Room speaker"), CrKey 1.56, on Wi-Fi, volume 0.4. Latency is measured from when the audio is "captured" or published on our side to when it's heard.

### HTTP media LOAD (Default Media Receiver `CC1AD845`, LIVE, chunked)

| codec | to first sound | steady latency | notes |
|---|---|---|---|
| AAC 256k | ~5 s | **4.03 s** (±3 ms) | ~18 s of erratic start/restart before settling |
| WAV | 8.0 s | **9.36 s** (±2 ms) | BUFFERING for ~8 s first |
| FLAC | 9.6 s | **9.51 s** (±2 ms) | |

- With **digital-silence** content, AAC never played and FLAC starved for 26 s, then settled at 14 s. The receiver buffers **by bytes**, so low-bitrate streams starve. The tests above use a −40 dBFS noise bed for realistic bitrates. A bridge must never send long runs of near-empty frames.
- The `MEDIA_STATUS.currentTime` estimate tracked WAV acoustics with a constant ~260 ms offset, but it was meaningless for FLAC and AAC.
- Not yet tried: `-burst`, `-lead`, BUFFERED, and other HTTP framing modes. They no longer matter much given the result below.

### Cast Streaming (audio mirroring receiver `85CDB22F`, Opus 128k CBR, encrypted RTP/UDP)

| receiver target | frame | heard latency | spread | retransmits | notes |
|---|---|---|---|---|---|
| 20 ms | 5 ms | 136 → **239 ms** | step +100 ms at ~8 s | 13 | **unstable**: one slip, then steady |
| 50 ms | 10 ms | **170 ms** | ±3 ms | 10 | |
| 100 ms | 20 ms | **221 ms** (211–252 across sessions) | ±3 ms | 1–35 | |
| 200 ms | 20 ms | **315 ms** | ±3 ms | 48 | |

- **Latency ≈ target + ~118 ms.** The ~118 ms is the Home Max's own output pipeline (DSP/DAC), which we can't remove.
- From OFFER to first sound takes 0.45–0.65 s, so starting a session is cheap.
- **It schedules by sender timestamps.** At target 100 ms, sending frames 50 ms or 300 ms early gave 211 ms and 252 ms. A play-on-arrival receiver would have shown ~170 ms, and <0 for the 300 ms case. So we can map an AirPlay anchor time T to an RTP capture time of T − target − pipeline and have it play at T. That's the clock-sync primitive §5 said was missing.
- **The per-session offset varies by ~±20 ms** (211/221/252 ms at the same settings across three sessions). It's probably the receiver's estimate of our clock from RTCP. That sets the multi-room alignment floor without per-session acoustic calibration.
- **Drift was ~0 ms/min within a session.** The receiver tracks our clock, so we don't need to resample for drift; that becomes the receiver's problem.
- **Recommendation: default target 100 ms (~220 ms heard), with 50 ms as a "low latency" option.** 20 ms slipped within seconds on Wi-Fi. 50 ms was clean here but has little margin for a busier network. Not yet tested: long runs (30+ min), a loaded network, groups, other models.

### Decisions (2026-10-25)

- **Output is Cast Streaming**, receiver target **100 ms** (configurable; tune later). HTTP LOAD is dropped from the main path; it's kept only as a possible fallback for receivers without the mirroring app.
- **AAC passthrough is dropped** (supersedes §2). Audio path: AirPlay AAC/ALAC → **ffmpeg subprocess** (decode, resample 44.1→48 kHz, libopus `lowdelay`, 20 ms frames, CBR) → Cast Streaming RTP. This is the same shape as shoutout's encoder pipe, and it means ffmpeg is a runtime dependency.
- **No long stability run for now.** Parameters get tuned once the bridge works end to end.
- **Sync (§5) becomes concrete:** PTP follower → anchor time → RTP timestamp/SR mapping, with no HTTP buffer guessing. The remaining error is the ~±20 ms session offset plus a fixed per-model pipeline delay (~118 ms on Home Max). The pipeline delay needs a calibration table per model.
- **Still open:** Cast Streaming to a Cast group (none on this network yet), and whether the session offset can be read back from receiver RTCP.

---

## 9. Implementation status (2026-10-25)

Steps 1–5 are implemented and were tested end to end. The setup:

- **Sender:** macOS Music on a MacBook Air.
- **Receiver:** castplay in a temporary hostNetwork pod on `moro`.
- **Speaker:** Google Home Max, over Cast Streaming at a 100 ms target.

| Piece | Where | Verified how |
|---|---|---|
| mDNS `_airplay._tcp`, `/info`, feature bits | `internal/airplay` | Music lists the speaker |
| HomeKit transient pairing (SRP-6a, PIN 3939), encrypted control channel | `internal/airplay/hap` | unit tests against an independent SRP client; macOS pairs |
| FairPlay setup handshake (canned replies only; the stream key comes from SETUP `shk`) | `internal/airplay/fairplay.go` | macOS proceeds past `/fp-setup` |
| SETUP phases 1/2, buffered (TCP, type 103) and realtime (UDP, type 96) streams, ChaCha20-Poly1305 decrypt | `internal/airplay` | dumped AAC decodes to the exact 440/660 Hz test tone |
| SETRATEANCHORTIME, FLUSHBUFFERED, volume, SETPEERS, TEARDOWN | `internal/airplay` | pause, resume, seek (flush and re-anchor) and volume all work from Music |
| PTP follower (nqptp-style: passive, plus the "awakening" Announce pair) | `internal/ptp` | Sync/Follow_Up at 8 Hz from the Mac; anchors convert to local time |
| AAC → Opus (ffmpeg subprocess, 20 ms frames) | `internal/media` | 36 ms average / 52 ms worst added latency (test) |
| Cast Streaming bridge with PTP-scheduled RTP timestamps | `internal/bridge` | the Home Max plays; 0 late frames in steady state |
| Discovery of Cast devices, one AirPlay speaker per device, stable identities | `cmd/castplay` | the Home Max appears as "Living Room speaker" |

**Why PTP mattered.** Music only sets up the audio stream after the receiver is in the session's PTP domain. Without a PTP follower on UDP 319/320, sessions stop after SETPEERS. On the Mac itself the sender's own PTP stack shares those ports, so receiver testing needs a second machine.

**Sync measurement.** Music played a 1 kHz beep track to the Mac's speaker and castplay's Home Max in one AirPlay group. Per-device volume swapped which one was audible halfway through, keeping a single timeline, and the MacBook mic (`bin/miccap`) timed the beeps.

- With `-output-latency 118ms` (the spike's figure) the Home Max played 31 and 52 ms *early*.
- With **80 ms** (the new default) it was **15 ms early**, including a few ms of sound travel.

That is within the ±20 ms per-session offset seen in the spike. More runs, and other speaker models, are needed for a per-model calibration table.

**Not done / known gaps**

- **Realtime ALAC is implemented but untested with a real sender.** ffmpeg now gets a live Matroska stream, which carries the ALAC config and per-frame timestamps; gaps become silence. Realtime timing comes from the control port's 0xD7 sync packets (shairport-sync's interpretation, including its 0.25 s fudge). It needs a macOS system-audio test.
- **iOS not yet tested.** Only macOS Music was used as the sender.
- **Cast groups not tested over Cast Streaming.** No group exists on this network.
- **Volume back to the sender is blocked.** Cast-side changes are detected, and a DACP client exists (`internal/dacp`), but macOS 27 Music never sends the DACP-ID/Active-Remote it needs. Modern senders probably expect MediaRemote over the AirPlay 2 event channel instead (unexplored).
- **Late start.** The first ~100–200 ms after a play/seek can be dropped as late, because senders anchor playback barely ahead of our ~220 ms pipeline.
- **Deployment.** There is no container image, terraform module or persistent state volume yet. airconnect is still deployed.
