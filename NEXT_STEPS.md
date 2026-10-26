# castplay: handoff / next steps (2026-10-26)

Read DESIGN.md §8–§9 first: the spike results, the decisions (Cast Streaming as output, 100 ms target, ffmpeg AAC→Opus), and implementation status. This file covers only the work in progress and what comes after it.

## Current task (user request): Cast content on Apple devices ("part 2")

**Goal:** when the Home Max plays Cast-native content (Spotify, YouTube Music, "Hey Google, play…"), Apple devices see it the way they see a HomePod: Control Center and the Home app's speaker tile show title, artist, artwork and play state, and play/pause/next/previous from the iPhone control the Cast app.

- **Part 1, watching the Cast side: easy, not started.** Standard Cast protocol.
- **Part 2, showing it on Apple devices:** MediaRemote (MRP) over AirPlay. The experiment is done; the build has not started.
- The user approved building both parts. They chose not to switch audio to Cast media mode for next/previous or metadata during AirPlay playback: the mirroring app castplay streams into has no media commands and a fixed title, and media mode costs 4–9 s of latency.

### What the experiment established (2026-10-26)

- **The flag works.** With status flag bit 11 (`RemoteControlRelay`) advertised (`-rc-relay`), every Apple device on the LAN opens a MediaRemote channel to castplay within seconds: the iPhone (.254), the Mac "Aoigin" (.242), and the Apple TV "Living Room" (.229, AppleTV14,1). The speaker is in the user's Home (Home add works); it is unknown whether that is required.
- **How a device opens the channel:**
  - It sends a phase-2 `SETUP` with one stream `{type: 130, controlType: 1, channelID, clientUUID, clientTypeUUID: 1910A70F-DBC0-4242-AF95-115DB30604E1}`, with **no `seed`**.
  - Answer as shairport-sync does with no seed: `{streams: [{type: 130, streamID}]}`, **no `dataPort`**. If given a dataPort, devices never connect to it.
  - The channel then rides the **shared control connection**: the device sends `POST /command` with bplist `{params: {data: <bytes>}}` and no `type`.
  - `data` is one MRP `ProtocolMessage`, prefixed with its varint length (as on pyatv's MRP wire).
- **The first message is always `DEVICE_INFO_MESSAGE` (type 15)**, with `identifier` (field 2), `uniqueIdentifier` (field 85) and `DeviceInfoMessage` as extension field 20. Decoded examples from the iPhone:
  - name "Chuie's iPhone", modelID iPhone18,1, build 24A446;
  - protocolVersion 1, lastSupportedMessageType 139;
  - systemMediaApplication com.apple.Music, routingContextID, airPlayGroupID.
- **Nothing answers yet.** castplay replies 200 with an empty body. After ~10 s the device TEARDOWNs the 130 stream and retries, which is why the speaker tile shows "Connecting…" / "Tap to connect".
- **Unknown: how the receiver sends MRP messages back.**
  - Best guess: the event channel (`internal/airplay/events.go`), as a mirrored `POST /command {params: {data: <varint-len + ProtocolMessage>}}`.
  - Alternative: the response body of the device's own `/command`.
  - Also log the `/command` request headers, which may carry the channelID.
  - The Mac's AirPlay framework strings mention `rcsUsesSharedConnection`, `remoteConrolSessionCreate` and `endpoint_handleRemoteControlSessionEvent`.

### Plan

1. **Find the reply path.**
   - Reply to `DEVICE_INFO` with castplay's own `DEVICE_INFO_MESSAGE`, echoing the request's `identifier`. Fill in: uniqueIdentifier = speaker pairing ID, name, localizedModelName "Google Home Max", protocolVersion 1, lastSupportedMessageType 139, deviceUID / modelID.
   - Try the event channel first. Log every incoming MRP message (type number + decoded fields) to see what follows. Expect, as in pyatv's client flow: SET_CONNECTION_STATE (38), CLIENT_UPDATES_CONFIG (16), GET_STATE (3), PLAYBACK_QUEUE_REQUEST (32), SEND_COMMAND (1).
2. **Minimal MRP device** (new package, e.g. `internal/mrp`, hand-encoded with `google.golang.org/protobuf/encoding/protowire`; no codegen, since protoc isn't installed):
   - **Push state:** send `SET_STATE_MESSAGE` (4, ext 9). It carries NowPlayingInfo or a PlaybackQueue of ContentItem + ContentItemMetadata (title/artist/album/duration/elapsed/artwork), supportedCommands (CommandInfo list), playbackState and playerPath (Origin / NowPlayingClient / NowPlayingPlayer).
   - **Answer requests:** PLAYBACK_QUEUE_REQUEST (with artwork), and SEND_COMMAND (1, ext 6) → SEND_COMMAND_RESULT (2, ext 7).
   - Possibly also SET_NOW_PLAYING_CLIENT / PLAYER (46/47).
   - **Proto reference:** pyatv `pyatv/protocols/mrp/protobuf/*.proto` (`gh api repos/postlund/pyatv/contents/pyatv/protocols/mrp/protobuf/<Name>.proto -H "Accept: application/vnd.github.raw"`). Extension numbers on ProtocolMessage:
     - SendCommand 6, SendCommandResult 7, SetState 9, SetArtwork 10;
     - DeviceInfo 20, ClientUpdatesConfig 21, VolumeControlAvailability 22;
     - PlaybackQueueRequest 37, SetConnectionState 42;
     - SetNowPlayingClient 50, SetNowPlayingPlayer 51;
     - GetVolume 53, SetVolume 55, UpdateContentItem 60.
   - Still to read: ContentItemMetadata.proto, CommandInfo.proto, and the Command / PlaybackState enums in Common.proto.
3. **Cast monitor (part 1):**
   - One always-on `cast.Client` per speaker, connected to receiver-0 and watching RECEIVER_STATUS.
   - When an app other than castplay's mirroring app (85CDB22F) runs, CONNECT to its transportId and GET_STATUS on `urn:x-cast:com.google.cast.media`. Follow MEDIA_STATUS: media.metadata title/artist/albumName/images, playerState, currentTime, duration, supportedMediaCommands.
   - Commands map to media PLAY/PAUSE/STOP, QUEUE_NEXT/QUEUE_PREV (or QUEUE_UPDATE jump ±1), and SEEK.
4. **Wiring:**
   - The Cast monitor feeds the MRP device: state changes push SET_STATE; commands from the iPhone go to the Cast app.
   - While castplay itself is streaming (85CDB22F active), report nothing (the iPhone is the source).
5. **Test:** cast Spotify/YouTube Music to the Home Max; check the iPhone's Control Center and the Home app tile; try play/pause/next.

### State of things right now

- **Production runs a dev build**, not the tf image: `docker.io/library/castplay:dev-cca67f0-…`, loaded by `tools/deploy-dev.sh`.
- **tmphouse-infrastructure pins** `ghcr…@sha256:3340433c…` (probe iteration 2) with args `-iface eno1 -v -rc-relay`. Any infra push resets the Deployment to that.
- **While `-rc-relay` is on, all Apple devices show the speaker tile as "Connecting…".** Playback is unaffected. Remove the flag (or finish this) before leaving it.
- **castplay commits not pushed to GitHub:** `ab54940`, `d3bf259`, `cca67f0` (probe iterations, deploy-dev.sh, workflow `paths-ignore` for `**.md` and `tools/**`).
  - ab54940 was pushed and built by CI (that's the 3340433c image); d3bf259 and cca67f0 are local only.
  - Pushing main costs Actions quota. Push only for releases.
- **Probe code:** `internal/airplay/rcprobe.go`. It logs "remote control stream requested", "sender command" and "sender command data" (hex). The `RemoteControlRelay` config is wired via `-rc-relay`.
  - Decode logged data with a raw protobuf walker: varint length, then fields. There's no protoc here.

### How to test builds (user preference: no GitHub Actions for iterations)

- Run `tools/deploy-dev.sh`. It runs `docker buildx` for linux/amd64, then `docker save | ssh ubuntu@100.97.228.124 sudo k3s ctr images import -`, then `kubectl set image` and waits for the rollout. Use the Tailscale IP: `ssh moro` fails host-key verification.
- Releases: push main → CI → bump `castplay_image` in `tmphouse-infrastructure/modules/tmphouse/castplay.tf` → push (Terraform Cloud auto-applies).

## Previous task: ALAC support + volume back to AirPlay

### 1. ALAC + realtime streams: DONE, realtime untested end-to-end

- **ffmpeg input is a live Matroska stream** for every codec (`internal/media/mkv.go`): AAC, ALAC or PCM, timestamped SimpleBlocks in µs. Clusters span at most 32 ms because a block's relative timecode is int16.
  - **Gap filling:** ffmpeg runs `aresample=48000:async=1:min_hard_comp=0.001:first_pts=0`, so missing frames become silence and output sample n == input time n.
- **Bridge:** `feed()` writes `WriteFrame(int64(int32(ts-first)), payload)`. ALAC uses `Format.ALACCookie()[12:]` as its config.
- **Tests:**
  - `TestMKVDecodes` / `TestMKVDecodesALAC`: a gap is filled to the exact timeline. The ALAC test uses ffmpeg's 4096-sample frames, since its encoder ignores `-frame_size`.
  - Transcode latency: 13 ms average / 24 ms worst.
- **Realtime timing:** `stream.serveControl` (internal/airplay/stream.go) parses the 0xD7 sync packets on the control port, as in shairport-sync.
  - **Anchor:** RTP frame_1 (bytes 4–7) plays at PTP time (bytes 8–15) + 0.25 s. That's shairport's "fudge" of 11025 frames; constant `realtimeSyncFudge`.
  - **Re-anchoring:** an anchor goes to the player only for the first sync, after a FLUSH (now handled as `Flush{All: true}`), on a clock change, or on a >5 ms timeline jump.
  - Unit-tested in `stream_test.go`.
- **Tested with macOS system audio (2026-10-25): works, sync acceptable.** The user reports "very much acceptable", with minor jitter.
  - Anchoring: latency 77175 as expected; playback starts ~210 ms after the anchor arrives.
  - **Retransmits added:** a realtime seq gap → `0x80 0xD5 <reqseq> <first> <count>` to the sender's control address (learned from 0xD7 packets); the copy arrives as 0xD6 with the RTP packet at byte 4.
    - Without them, Wi‑Fi loss gave 24–56 ms dropouts every ~1.6 s.
    - With them, bursts of up to 78 packets were fully recovered.
  - `feed()` re-scans for a missing frame just before writing, so resent frames still get used.
  - "stream stats" log every 10 s: late, castRetransmits, unacked, receiverDelay, sendMargin. In the test run: 0 late, ~19 Cast retransmits per 40 s, margin 350 ms.
  - **Residual minor jitter: cause unknown, parked.** Candidates: Cast retransmits missing the 100 ms window, or the ALAC→Opus path. Ideas: compare with Music's AAC, or try a 200 ms target.
- Volume detection on the Cast side was confirmed with the speaker's touch controls.

### 2. Volume and pause from the Google side back to the AirPlay sender: DONE (2026-10-26)

- **Mechanism: the AirPlay 2 event channel, not DACP.** macOS 27 / iOS send no `DACP-ID`/`Active-Remote`.
  - The sender connects to the `eventPort` from SETUP. Keys are HKDF-SHA512 over the pairing secret with salt `Events-Salt`; the receiver writes with `Events-Write-Encryption-Key` and reads with `Events-Read-...` (the reverse of the control channel; see pair_ap's `cipher_new`).
  - On connect castplay sends `POST /command` `{type: updateInfo, value: <GET /info dict + txtAirPlay>}`.
  - Code: `internal/airplay/events.go` (`EventChannel.SetVolume`, `.Command`) and `hap.NewEventConn`.
- **Volume:** `{type: sendMediaRemoteCommand, value: dvlc, volume: <0..1>, params: {volume}}`, stepped by 0.09 every 10 ms (shairport-sync: a big jump makes Apple Music on macOS jump its local volume).
  - **Verified from an iPhone (2026-10-26):** every Home Max knob step moved the iPhone's slider. The iPhone didn't echo SET_PARAMETER; `Speaker.Volume` still ignores echoes on the pushed path for 3 s.
  - **Fixed in the same test:** dragging the iPhone slider raced concurrent `applyVolume` goroutines and could leave the speaker at an earlier position. `requestVolume` now applies changes in order, newest wins, and only unmutes when muted.
- **Pause:** the Cast mirroring app has `supportedMediaCommands: 0`. The speaker's pause button or "Hey Google, pause" stops the app, which closes our virtual connection (`cast.Client.AppClosed`). `Speaker.watchApp` then:
  - stops playback and releases the session,
  - sends MediaRemote pause (`modernMediaRemoteCommand: "1"`),
  - and the next play anchor starts a new Cast session.
  - **Deployed (image 07ddc612); the user's test result wasn't recorded.**
- **Next/previous: not possible from the speaker** while using Cast Streaming, because the mirroring app has no media commands. `CommandNext`/`CommandPrevious` exist for whenever something can trigger them.
  - The user decided not to use Cast media playback mode for this: it measured 4–9 s of latency and can't follow AirPlay anchors.
- The DACP client (`internal/dacp`) stays as a fallback for senders that do send the headers.

### 3. Adding speakers to Apple Home: DONE, verified (2026-10-26)

- The Home app transient-pairs, opens a `timing=None` session, then sends `/pair-add` (its controller key), then `/configure`. castplay used to 404 `/pair-add` ("Unable to Add Accessory").
- castplay now handles `/pair-add`, `/pair-remove`, `/pair-list` (`hap.HandlePairings`) and `/configure`.
  - **State:** stored in `speaker-<id>.home.json` in the state dir (`internal/airplay/home.go`).
  - **Advertising:** once configured, the speaker advertises status flag bit 10 (HomeKit access control) and the ACL.
  - **Access control:** recorded and echoed back, but not enforced (pair-verify accepts anyone).
- **Verified:** the user added Living Room speaker to Home. `speaker-<id>.home.json` holds the admin controller plus one non-admin controller, `accessControl: true`, `acl: 0`.
- **Production runs with `-v`** (tf comment says so); remove it when done.

## Test setup (how the previous session tested)

- **Sender:** macOS Music on this Mac (Aoigin, 192.168.1.242), driven by osascript:
  ```
  tell application "Music" to set current AirPlay devices to {AirPlay device "Living Room speaker"}
  ... play (POSIX file "<scratchpad>/castplay-test-tone.m4a")
  ```
  Per-device volume: `set sound volume of AirPlay device "X" to N`.
  - The library's own purchased track doesn't play (cloud).
  - The test files were in the session scratchpad and may be gone. Regenerate them with ffmpeg:
    - **Tone:** a 60 s 440 Hz/660 Hz stereo tone (`sine` + `amerge`, `volume=0.3`).
    - **Sync beeps:** `aevalsrc=0.5*sin(2*PI*1000*t)*lt(mod(t\,1)\,0.03)`, 40 s.
  - Playing these added "castplay test tone" and "castplay sync beeps" to the Music library; the user was told.
- **Receiver: it can't run on the same Mac as the sender** (both need PTP UDP 319/320). Production now runs on moro (see Deployment below). For a test build, stop or scale down the production Deployment first, or the test pod will fight it for the ports. The old temporary pod setup was:
  - **Where:** namespace `castplay-test`, pod `castplay-test`, created with the user's OK.
  - **Pod spec:** `alpine:3.20` sleeping, hostNetwork, nodeSelector moro.
  - **Manifest:** in the session scratchpad `castplay-test-pod.yaml`. If it's gone, recreate it: Namespace + Pod, hostNetwork, `dnsPolicy ClusterFirstWithHostNet`, `restartPolicy Never`, `command ["sleep","86400"]`.
  - **Deploy sequence:**
    ```
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/castplay-linux-amd64 ./cmd/castplay
    kubectl -n castplay-test delete pod castplay-test; kubectl apply -f <yaml>; kubectl wait ...
    kubectl exec ... -- apk add ffmpeg
    kubectl cp bin/castplay-linux-amd64 castplay-test:/castplay
    kubectl exec ... -- sh -c '(nohup /castplay -state /state -include "^Living Room speaker$" -v > /castplay.log 2>&1 &)'
    ```
  - **Gotcha:** a later `kubectl exec` can't kill processes started by an earlier one ("permission denied"), so recreate the pod to restart castplay.
  - The pod runs the current build (2026-10-25 23:03 UTC).
- **Acoustic checks:**
  - **Capture:** `bin/miccap MacBook > file.raw` (Swift helper; `swiftc -O tools/miccap/main.swift -o bin/miccap`).
  - **Format:** header `MCAP` + f64 rate + f64 latency, then per buffer `i64 unixNs` + `u32 frames` + s16 samples.
  - **Analysis:** small Python scripts (Goertzel tone levels / 1 kHz onset envelopes) were in the scratchpad (`tones.py`, `onsets.py`, `envelope.py`); rewrite them as needed.
  - **Sync test:** group "Aoigin" + "Living Room speaker", and swap which one is audible mid-play via per-device volume (one PTP timeline). Compare the beep phases of the two halves. Result: Home Max 15 ms early with the default `-output-latency 80ms`.
- airconnect still runs on moro (namespace `airconnect`). The user said castplay will replace it and it's fine to take it down if easier. Its AirPlay name is "Living Room", so there's no clash.

## After this task

1. **Deployment: DONE (2026-10-25).**
   - **Image:** public at `ghcr.io/1lann/castplay`, built by `.github/workflows/docker.yml` on every push to main. The run summary prints the digest.
   - **Terraform:** `tmphouse-infrastructure/modules/tmphouse/castplay.tf` replaced `airconnect.tf`.
     - Namespace `castplay`; hostNetwork on moro with `-iface eno1`.
     - Runs as uid 1000 with only NET_BIND_SERVICE (the binary has a setcap for PTP).
     - State lives in hostPath `/home/ubuntu/castplay`.
   - **Applying:** pushing tmphouse-infrastructure master auto-applies via Terraform Cloud; don't apply locally. To upgrade, bump `castplay_image` to the new digest and push.
   - airconnect and the `castplay-test` namespace are gone. Logs: `kubectl -n castplay logs deploy/castplay`.
   - It also bridges the TV "50S546" (Smart TV Pro); use `-include`/`-exclude` args in the tf if that's unwanted.
2. ~~Test from an iPhone~~: works (buffered AAC and realtime; volume both ways).
3. **Cast groups over Cast Streaming** (untested; there's no group on this network).
4. **More sync runs** to firm up the `-output-latency` calibration, ideally per model.
5. Optional:
   - Metadata/artwork from SET_PARAMETER (DMAP / image) to Cast.
   - Reduce the ~100–200 ms of dropped start-up audio (target 50 ms, or start the cast session earlier).

## Code map

- `internal/airplay/`:
  - `receiver.go`: RTSP verbs and the Player interface.
  - `stream.go`: buffered TCP / realtime UDP, ChaCha20-Poly1305 decrypt; control UDP port → 0xD7 realtime anchors.
  - `hap/`: SRP-6a, pair-setup/verify, encrypted control channel; has tests.
  - `fairplay.go`: canned fp-setup replies.
  - `mdns.go`, `format.go` (audioFormat bits, `ALACCookie`).
- `internal/airplay/rcprobe.go`: `-rc-relay` MediaRemote probe (type 130 SETUP, /command logging); to become the MRP device.
- `tools/deploy-dev.sh`: local build → k3s import → `kubectl set image`.
- `internal/airplay/events.go`: event channel (volume/MediaRemote commands to the sender); `home.go`: Home pairings and /configure.
- `internal/dacp/`: DACP remote client (volume to sender, older senders).
- `internal/ptp/ptp.go`: nqptp-style follower: Announce/Follow_Up, max-offset window, awakening Announce pair. `LocalTime(clockID, ns)`.
- `internal/bridge/speaker.go`: Player → ffmpeg → Cast Streaming.
  - Capture time = play time − target − output latency.
  - RTP clock: 48 kHz from `castEpoch`, SR every 250 ms. Frames are sent no earlier than 250 ms before capture time; late ones are dropped.
  - Volume mapping lives here too.
- `internal/cast/`: Cast v2 client (`client.go`, with `NSWebRTC` seqNum handling, `StopApp`, `SetMuted`), `streaming.go` (OFFER/ANSWER, encrypted RTP sender, feedback/retransmit; adapted from shoutout, MIT), `discover.go`.
- `internal/media/`: `transcode.go` (ffmpeg pipe, Ogg Opus parsing, PreSkip), `mkv.go` (live Matroska input), `adts.go`.
- `cmd/castplay/`:
  - `main.go`: flags; defaults: target 100 ms, output-latency 80 ms, port 7100+.
  - `bridge.go`: discovery loop, one receiver per Cast device.
  - `identity.go`: persistent IDs in `-state`.
  - `dump.go`: `-dump` debug mode.
- `cmd/castspike/`: the P0 latency spike (HTTP vs Cast Streaming measurements).
