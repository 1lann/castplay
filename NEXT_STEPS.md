# castplay: handoff / next steps (2026-10-25)

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

### 2. Volume from the Google side back to the AirPlay sender: built, BLOCKED on the sender

- **Detecting Cast-side changes works on the Home Max.**
  - `cast.Client.OnVolume` reports RECEIVER_STATUS volume. `bridge.Speaker.castVolume` treats statuses as echoes of our own set until one matches it, or for 5 s; the Home Max takes about 1.1 s to apply a set and reports the old level meanwhile.
  - Verified: an external `SetVolume(0.3)` logged "volume changed on Cast device … airplay=30".
- **DACP client is written and unit-tested** (`internal/dacp`): mDNS `iTunes_Ctrl_<id>`, DMAP parsing, and shairport's absolute/relative volume algorithm. The speaker number is our DeviceID MAC as an integer.
- **Blocker: macOS 27 Music (AirPlay/980.77.5) sends no `DACP-ID`/`Active-Remote`.** It isn't in any RTSP header (GET /info, SETUP, …) or in the SETUP plist, even though it advertises `iTunes_Ctrl_…` over mDNS. Without Active-Remote, DACP requests are refused.
  - Tried advertising shairport's extra feature bits: 11 and 38 changed nothing. Bit 26 makes Music demand MFi `/auth-setup`, which then fails (error -15016). Reverted to the original bits.
  - The code still picks the headers up (`session.handle`) for senders that do send them, such as older iTunes/iOS (unverified).
- **Likely real mechanism for modern senders:** the AirPlay 2 event channel. The sender connects to our `eventPort` (currently `discardConns`), encrypted with HKDF "Events-Salt" keys; HomePods talk MediaRemote over it.
  - **Needs research:** check shairport-sync's event-port handling and pyatv's AirPlay 2 / MRP code. Ask the user before investing in it.
- `rtsp` debug log lines now include request headers, which is handy for this investigation.

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
2. **Test from an iPhone** (iOS uses buffered AAC; untested so far).
3. **Cast groups over Cast Streaming** (untested; there's no group on this network).
4. **More sync runs** to firm up the `-output-latency` calibration, ideally per model.
5. Optional:
   - Report Google-side play/pause to the sender (DACP `playpause`).
   - Metadata/artwork from SET_PARAMETER (DMAP / image) to Cast.
   - Reduce the ~100–200 ms of dropped start-up audio (target 50 ms, or start the cast session earlier).

## Code map

- `internal/airplay/`:
  - `receiver.go`: RTSP verbs and the Player interface.
  - `stream.go`: buffered TCP / realtime UDP, ChaCha20-Poly1305 decrypt; control UDP port → 0xD7 realtime anchors.
  - `hap/`: SRP-6a, pair-setup/verify, encrypted control channel; has tests.
  - `fairplay.go`: canned fp-setup replies.
  - `mdns.go`, `format.go` (audioFormat bits, `ALACCookie`).
- `internal/dacp/`: DACP remote client (volume to sender).
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
