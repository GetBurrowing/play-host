# Spike 01a0db5f stage 1: a session-0 frame path on host-a

Task 01a0db5f ("Playable review builds: a game_stream canvas"), plan v1, stage 1.
Measured 2026-09-26 between 02:10Z and 03:00Z on host-a (Windows 11 Home, RTX 3080 Ti,
driver 610.47, Godot 4.7.2, ffmpeg 9.0.2), with headless Google Chrome 153 on host-b as
the viewer. The raw per-probe samples are not kept in this repository; the tables below are their summary.

**If you are reading this to size stage 2 or 3, read "What stage 2 and 3 must know" first.**

## Result

| | |
|---|---|
| Chosen frame path | **C, in-engine readback**: `get_viewport().get_texture().get_image()` in `RenderingServer.frame_post_draw`, streamed as raw RGBA over 127.0.0.1 TCP to the host, piped into `ffmpeg -f rawvideo -pix_fmt rgba ... -c:v h264_nvenc` |
| Chosen framing | **ffmpeg `-f rtp` to 127.0.0.1, forwarded packet by packet to a pion `TrackLocalStaticRTP`**. The Annex-B/h264reader framing the plan specified works, but it was 4 ms slower at p50 (see below) |
| Chosen control preset | **`p1` / `ll`**, CBR 8000 kbps, 60 fps, GOP 120, baseline, zerolatency, no lookahead, no B-frames, forced IDR. As `game_stream.encoder_preset`: `{"preset":"p1","tune":"ll","bitrate_kbps":8000,"fps":60,"width":1280,"height":720,"gop_frames":120}`. **The delivered size is 1028x720, not 1280x720**; see below |
| Input-to-photon, control arm | p50 **41.6 ms**, p95 **51.7 ms**, n=150, 0 lost (run d1). All six p1/ll probes runs pooled: n=206, 4 lost |
| VRAM | Idle, with llama-swap holding `qwen3.5-9b`: 6796 MiB used of 12288. Streaming: 7224-7451 MiB used, **4636-4863 MiB free**. The stream (Godot + ffmpeg/NVENC) costs 430-655 MiB |
| Session-0 context | A throwaway scheduled task `suite-play-spike-01a0db5f`: S4U, user `HOST-A\operator`, RunLevel Limited, the same principal as `editor-godot`. It runs in window station `Service-0x0-3bdae4c$`, session 0. No interactive user is logged on (LogonUI.exe is in session 1) |

## Frame-path matrix

Every path was run from the scheduled task above, with Godot started through
`play-host.exe`'s Job Object and `--rendering-driver vulkan`. Godot logged
`Vulkan 1.4.341 - Forward+ - Using Device #0: NVIDIA - NVIDIA GeForce RTX 3080 Ti`
every time.

| Path | Result | Exact output |
|---|---|---|
| A. ffmpeg `ddagrab` (DXGI desktop duplication) | **Fails**, as predicted | `[Parsed_ddagrab_0] Failed to enumerate DXGI output 0` / `Failed to configure output pad on Parsed_ddagrab_0` / `Error opening input: Generic error in an external library` (ffmpeg exit -542398533). Same with and without a Godot window present |
| B. ffmpeg `gdigrab -i title=...` | **Fails**. It is not black frames: no frame is captured at all | By the plain project title: `Can't find window 'suite-play-spike-01a0db5f', aborting.` The real title is `suite-play-spike-01a0db5f (DEBUG)`, and the window exists but is not visible (`IsWindowVisible=False`). By that exact title: `Found window suite-play-spike-01a0db5f (DEBUG), capturing 1028x720x32 at (0,0)` then `Failed to capture image (error 5)` (ERROR_ACCESS_DENIED on BitBlt). `gdigrab -i desktop` also gives `Failed to capture image (error 5)` on the 1024x768 service desktop |
| C. In-engine readback, `get_image()` variant | **Works** | 60 fps sustained from Godot to NVENC to the browser. No dropped frames on the browser (`framesDropped: 0`). Full latency table below |
| C. In-engine readback, `RenderingDevice.texture_get_data_async` variant | **Works, but slower** | The RD texture is format 36 (`R8G8B8A8_UNORM`), 1028x720, byte-compatible with the rawvideo rgba input. p50 69.4 ms, p95 89.2 ms, n=94, 6 lost: about 28 ms worse than `get_image()`, because the async callback resolves several frames after the request |
| D. Vulkan external memory to CUDA to NVENC | **Paper only**; see below | Not needed at the measured readback cost |

## Latency

Method, as specified in the plan and reused by later stages. The browser sends
`{"t":"probe","seq":N}` on the reliable `input-events` data channel and timestamps it
with `performance.now()`. The host forwards it to the addon over the 127.0.0.1 link.
The addon draws N for 2 frames in the top-left corner. In `requestVideoFrameCallback`,
the browser decodes the corner and records `now - t0`.

The corner code is a 4x4 grid of 16x16 px cells, not "8x8 blocks". Each cell is one
H.264 macroblock, so DCT and chroma bleed stay inside a cell. The code is 12 bits of seq
plus a 4-bit check. `internal/probe` pins the wire values, and the GDScript and JS
copies mirror it.

`recv` is `rVFC metadata.receiveTime - t0`, the moment the last packet of the frame
arrived. `after recv` is the rest: the jitter buffer, decode and the compositor tick.
`host` is the time from the probe arriving at play-host to the addon's readback frame
that carries it, decoded on the host from the raw RGBA. That covers the addon's
`_process` poll (up to one frame), render and `get_image()`.

Chrome: Google Chrome 153 headless (`selenium/standalone-chrome` image) on host-b,
`--headless=new`. **Its GPU process fails to initialise** (`eglInitialize SwANGLE failed`),
so decode is software. `decoderImplementation` is hidden by Chrome's stats privacy gate
without a capture permission. The mean decode time is 0.40-0.51 ms per frame. The selected
pair is always `host <host-a>:4030x <-> prflx <host-b>` (LAN, no TURN).

| run | preset/tune | framing | readback | n | lost | p50 ms | p95 ms | host p50 | recv p50 | after recv p50 | fps | jitter buf ms | VRAM used, free |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| c1 | p4/ll | rtp | image | 60 | 0 | 34.4 | 53.2 | 14.8 | 19.3 | 14.6 | 61 | 7.5 | (not taken) |
| c2 | p1/ull | rtp | image | 57 | 3 | 38.2 | 55.1 | 14.7 | 20.9 | 14.8 | 59 | 8.1 | 7443, 4644 |
| c3 | p4/ull | rtp | image | 58 | 2 | 38.9 | 64.4 | 14.5 | 21.4 | 17.4 | 60 | 8.3 | 7224, 4863 |
| c4 | p7/ll | rtp | image | 60 | 0 | 48.1 | 59.4 | 13.9 | 22.5 | 28.8 | 60 | 7.4 | 7224, 4863 |
| c5 | p4/ll | rtp | image | 58 | 2 | 41.5 | 55.6 | 15.7 | 21.8 | 19.2 | 60 | 8.1 | 7224, 4863 |
| c6 | p1/ll | rtp | image | 56 | 4 | 36.0 | 60.5 | 12.9 | 17.7 | 16.0 | 60 | 7.5 | 7451, 4636 |
| **d1** | **p1/ll** | rtp | image | **150** | **0** | **41.6** | **51.7** | 13.4 | 17.3 | 26.7 | 60 | 7.4 | 7451, 4636 |
| d2 | p4/ll | rtp | image | 147 | 3 | 40.4 | 63.2 | 14.8 | 20.5 | 19.6 | 60 | 7.6 | 7451, 4636 |
| d3 | p1/ull | rtp | image | 140 | 10 | 37.4 | 67.3 | 16.5 | 22.0 | 13.0 | 60 | 7.6 | 7451, 4636 |
| d4 | p4/ull | rtp | image | 147 | 3 | 40.7 | 62.2 | 16.5 | 22.3 | 18.6 | 60 | 7.6 | 7451, 4636 |
| e1 | p1/ll | **annexb** | image | 100 | 0 | 45.5 | 55.6 | 14.7 | 18.5 | 27.0 | 60 | 7.6 | 7224, 4863 |
| e2 | p1/ll | rtp | **async** | 94 | 6 | 69.4 | 89.2 | n/a | 51.5 | 18.7 | 59 | 7.8 | 7224, 4863 |

What the table does and does not support:

- **The run-to-run spread is as large as the p1..p4 differences.** p4/ll gave a p50 of
  34.4, 41.5 and 40.4 ms in three runs. At n of about 150, p1/ll, p4/ll, p1/ull and p4/ull
  all sit at 37-42 ms p50. So the choice among them is not a p50 result.
- **p7 was the slowest arm, in its only run.** It was about 8 ms above the p4/ll runs at p50 (c4, 48.1, n=60). That is one run, so treat it as a lead, not a result.
- **`ull` loses more probes.** 13 of 197 were lost at p1/ull, against 4 of 206 at p1/ll.
  A lost probe is one whose corner never decoded within 1.5 s. The likeliest cause is
  that ull's lower quality blurs the 2-frame marker. That is an inference: no frame was
  inspected to confirm it.
- **Why p1/ll is the control.** It is the fastest encoder preset. It gave the best p95 at
  n=150 (51.7) with 0 lost, and the lowest `recv` p50 of any arm (17.3 and 17.7). `recv`
  is the part the encoder preset can move.
- **Annex-B framing costs about 4 ms at p50** (e1 45.5 against d1 41.6). That matches
  pion's h264reader needing the next frame's start code before it returns a frame's last NAL.
- **Where the time goes at the control arm, by p50:**
  - 13-15 ms, game side: the `_process` poll, which waits up to one frame, then render and readback;
  - about 4 ms, encode plus LAN: `recv` 17 ms minus the host part;
  - 16-27 ms after receive: jitter buffer (7.5 ms), decode (0.4 ms), and rVFC's
    compositor tick in software headless Chrome.
  The after-receive part is the noisiest, and it belongs to headless Chrome. A real
  desktop Chrome with hardware decode and a display should do better, but that is not
  measured here.
- **GPU contention.** Other work used host-a's GPU during some runs: nvidia-smi read 22-96%
  utilisation during d1, d2, d4 and c6, and 99% at idle afterwards, while llama-swap had
  qwen3.5-9b loaded. The numbers were taken under that load. They were not taken on a
  quiet GPU.

## VRAM headroom

- nvidia-smi on WDDM reports no per-process VRAM (`[N/A]`), so every figure here is a
  whole-GPU total.
- Idle, with llama-swap holding `qwen3.5-9b` (Q5_K_M, 6.13 GiB file; this is what was
  resident): 6796 MiB used, 5291 MiB free of 12288.
- During every streaming run: 7224-7451 MiB used, 4636-4863 MiB free.
- **Not measured: headroom with the largest model loaded.** That model is `gemma4-12b`
  (Q4_K_M, 6.63 GiB file). Loading it would have evicted qwen3.5-9b, which was in use by
  other agents during the spike. The file is 0.50 GiB larger, so the projected free VRAM
  while streaming is about 4.1 GiB. That figure is a projection from file size, not a
  measurement.

## D on paper: GPU-side sharing

To skip the readback, the frame would stay on the GPU:

1. Export the viewport's image with `VK_KHR_external_memory_win32`.
2. Import it into CUDA with `cuImportExternalMemory`, and a timeline semaphore with
   `cuImportExternalSemaphore`.
3. Register the CUDA array with `NvEncRegisterResource`.

Godot's RenderingDevice exposes `get_driver_resource` (VkImage, VkDevice) but has no
exportable-memory texture flag. So this needs:

- a C++ GDExtension that creates its own exportable VkImage and copies the viewport into
  it every frame;
- an in-process NVENC encoder, i.e. cgo or C++ on Windows. The ffmpeg subprocess cannot
  receive a CUDA handle.

That is exactly the cgo path v1 set out to avoid.

**Decision rule, from the plan: pursue D if C's readback costs more than one frame.** It
does not. At 60 fps (16.7 ms per frame), the whole game-side segment is 13-15 ms p50, and
that already includes an average half-frame (8.3 ms) wait for the `_process` poll. That
leaves render plus `get_image()` at roughly 5-7 ms. D stays a follow-up for 1080p60 or
higher frame rates.

A cheaper win exists first: service the probe/input link earlier in the frame, before
`_process`, e.g. from `_physics_process`, or by polling in `_input`. That attacks the
poll wait, which is the larger share.

## What stage 2 and 3 must know

1. **The session-0 desktop is 1024x768, and Godot's window is clamped to it.**
   - `--resolution 1280x720` produced a 1028x720 viewport and frames, in every run.
   - Stage 2's `encoder_preset` width/height cannot be honoured by resizing the window.
   - Stage 3 must either render into a fixed-size SubViewport, or accept the clamped size
     and report the real frame size in `play_session_status`.
   - The host already sizes ffmpeg from the first frame header, so nothing breaks; the
     picture is simply smaller.
2. **The window title carries a ` (DEBUG)` suffix.** Anything that matches the Godot
   window by title must allow for it. Path B is dead regardless.
3. **git is not installed on host-a's Windows side.** (`git` is not on PATH, and there is
   no `C:\Program Files\Git`.) Stage 3's bare mirror plus `git worktree add` needs Git for
   Windows installed, or it must shell into WSL git. The WSL route crosses the /mnt/c
   boundary and is slow on big checkouts.
4. **`query user` does not exist on Windows 11 Home.** Use the LogonUI.exe/explorer.exe
   session check instead.
5. **Headless Chrome for e2e must be Google Chrome, not `chromedp/headless-shell`.**
   - The headless-shell image in the `dev-on-host-b-e2e-rig` skill has no H.264 in WebRTC.
     `RTCRtpReceiver.getCapabilities('video')` lists VP8, VP9 and AV1 only.
   - NVENC on Ampere cannot encode AV1, and does not encode VP8 or VP9, so that image can
     never decode this stream.
   - What worked:
     `podman run -d --name <tag>-chrome --network host --shm-size 2g --entrypoint /usr/bin/google-chrome docker.io/selenium/standalone-chrome --headless=new --no-sandbox --remote-debugging-port=9223 --user-data-dir=/tmp/<tag> --autoplay-policy=no-user-gesture-required about:blank`
   - It binds 127.0.0.1 only, so reach it with `ssh -L 19223:127.0.0.1:9223 user@host-b.example`.
   - This also affects stage 6's e2e setup, which says "headless Chrome on host-b".
6. **The Job Object works.**
   - Godot is created suspended, assigned to the job, then resumed (`internal/launch`).
     `Kill` reported the whole tree every time, e.g. `pids=[13808 14008 9572]`: the console
     wrapper, the real exe and one more child.
   - `KILL_ON_JOB_CLOSE` also took Godot down when play-host itself was force-stopped.
   - No process was ever matched by name.
7. **The `editor-godot-loop.ps1` exemption holds.**
   - Backup: `editor-godot-loop.ps1.bak-01a0db5f-202609260209`, sha256 `CD60CB3C...7845`.
     The patched file's sha256 is `A09F88A5...9850`.
   - The loop was restarted by stopping its powershell pid (10888) and running
     `Start-ScheduledTask editor-godot`. The new supervisor pid is 5804.
   - The headless editor, pids 10088 and 12024 (started 2026-09-25 17:30:32 local),
     survived that restart, all 16 windowed spike Godot launches, and the 3 headless `--check-only` runs.
   - Since the restart, the loop log shows no "GUI Godot detected".
   - Separately, the log shows an earlier kill at `2026-09-25T22:30:27Z GUI Godot detected: 7428`
     with an EMPTY command line. That kill predates this spike.
   - The loop treats any Godot whose command line reads as empty as a GUI editor.
     Both the ssh session and the S4U task are elevated, so the obvious integrity-level
     explanation does not hold, and the cause is unknown.
   - Stage 3 should consider treating an empty command line as not-GUI. That is a change
     to the editor agent's loop, and was not made here.
8. **The subprocess encoder cannot force an IDR on PLI.**
   - Chrome sent 7 PLIs in the first run, when joining mid-GOP.
   - The spike uses a bounded GOP (120) plus in-band SPS/PPS before every keyframe
     (`-bsf:v dump_extra=freq=keyframe`). This matters because ffmpeg's RTP muxer
     otherwise puts them only in its SDP.
   - A viewer joining mid-GOP waits up to 2 s for a picture. `waitLive` measured 2.4 s to
     the first 10 frames.
   - Stage 3 should use a GOP of 60, or `-intra-refresh 1`, and record which one it uses.
9. **ICE worked with host candidates only.** The host offered only `<host-a>`, via
   `-host-ip`. Chrome's side arrived as prflx, so its mDNS obfuscation made no difference.
   Firewall rule `suite-play-host-udp-01a0db5f`:
   - inbound UDP 40300-40309;
   - program `C:\Users\operator\play-host\bin\play-host.exe`;
   - remote <lan>/24;
   - all profiles.
   It is the only rule created.
10. **Signalling needs no firewall rule.** HTTP is bound to 127.0.0.1:18431 and reached
    through `ssh -L`. Stage 3 replaces it with the Phoenix channel, which is outbound only.

## Reproduce

Build on linux, deploy, then run from the S4U task, NOT from ssh (ssh children die
with the session):

    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o play-host.exe ./cmd/play-host
    play-host.exe spike -source C -export image -preset p1 -tune ll -godot <console exe> \
      -project <copy of spike/godot + addons/suite_play> -ffmpeg <ffmpeg.exe> -host-ip <host-a> -logs <dir>
    ssh -N -L 18431:127.0.0.1:18431 host-a &
    node spike/driver/run-probes.mjs --cdp 127.0.0.1:19223 --signal http://127.0.0.1:18431 --n 150 --label x --out x.json
