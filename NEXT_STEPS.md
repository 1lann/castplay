# castplay: handoff / next steps (2026-10-26)

Read DESIGN.md §8–§9 first: the spike results, the decisions (Cast Streaming as output, 100 ms target, ffmpeg AAC→Opus), and implementation status. This file covers only the work in progress and what comes after it.

## Last task (user request): ALAC support + volume back to AirPlay

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
  - **Deployed but untested** (image 07ddc612).
- **Next/previous: not possible from the speaker** while using Cast Streaming, because the mirroring app has no media commands. `CommandNext`/`CommandPrevious` exist for whenever something can trigger them.
  - The user decided not to use Cast media playback mode for this: it measured 4–9 s of latency and can't follow AirPlay anchors.
- The DACP client (`internal/dacp`) stays as a fallback for senders that do send the headers.

### 3. Adding speakers to Apple Home: built, untested

- The Home app transient-pairs, opens a `timing=None` session, then sends `/pair-add` (its controller key), then `/configure`. castplay used to 404 `/pair-add` ("Unable to Add Accessory").
- castplay now handles `/pair-add`, `/pair-remove`, `/pair-list` (`hap.HandlePairings`) and `/configure`.
  - **State:** stored in `speaker-<id>.home.json` in the state dir (`internal/airplay/home.go`).
  - **Advertising:** once configured, the speaker advertises status flag bit 10 (HomeKit access control) and the ACL.
  - **Access control:** recorded and echoed back, but not enforced (pair-verify accepts anyone).
- The user hasn't retried adding the speaker yet. If it still fails, the `-v` logs show the next unhandled request.
- **Production runs with `-v`** (tf comment says so) for these tests; remove it when done.

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
