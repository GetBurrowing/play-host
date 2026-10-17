# Multiplayer game_stream: per-slot input-to-photon and the fan-out cost (01a0dbd6 stage 5)

Task 01a0dbd6 (multiplayer game_stream), plan v1, stage 5. Measured on 2026-09-29 between
00:49Z and 02:30Z. The host was host-a, running the DEV play-host instance
(`suite-play-host-dev-01a0dbd6`, runtime `play-host-a-dev`, UDP 40310-40319, godot 40321,
rtp 40331, play-host e2f0d86). The viewers were headless Google Chrome 153 on host-b, one
browser context per viewer, against the host-b dev core on :4033 (core branch
`task/01a0dbd6-76bb-7001-a322-08a81e3d7927` at abb0f5a3c). The raw per-probe samples and
counter logs are not kept in this repository; the tables below are their summary.

**Prod was idle for every run.** Before and after each run, the rig checked whether the prod
play-host was busy. It looked for a Godot or ffmpeg child whose command line points at
`C:\Users\operator\play-host\checkouts`, and at whether the last session line in the prod log was
an end. All 19 runs read `PROD_BUSY=False` both times. The check was first shown to fire by
pointing it at the busy dev instance, where both clauses returned `True`. Nothing bound UDP
40300-40309, :40320 or :40330. Every host-a reading was a read-only counter:
`Get-NetAdapterStatistics`, `Get-NetUDPEndpoint` and `nvidia-smi`.

## Result

| | |
|---|---|
| Per-slot latency vs viewer count (LAN) | **No fan-out cost is measurable up to 4 players + 2 spectators.** Pooled p50 is 40.6 ms at 1 viewer, 41.5 ms at 2 players, 42.9 ms at 4 players and 41.6 ms at 4+2. Pooled p95 is 61.6, 60.6, 60.9 and 61.9 ms, and every 90% CI overlaps the 1-viewer control's. |
| The ~60 ms p95 bar | The **1-viewer control is itself at the bar**: pooled p95 is 61.6 ms (90% CI 57.5-66.5, n=153, three runs). Under the stage's rule, 13 slots sit above the control plus its excess (11 without the host-b-loaded rp2mj). Each is listed under Findings. None is distinguishable from the control at n of about 50. |
| Relay (force_relay, host-d coturn) | 2 players pooled: p50 44.1, p95 64.9 ms (n=318, three runs). The 1-player relay control gave p50 40.7, p95 50.5 ms (n=51). **The p50 rise of about 3.4 ms is the one fan-out difference whose CI clears the control's.** |
| VRAM | **Flat: 811 MiB whole-GPU at 1, 2, 4, 2+2 and 4+2 peers (a 0 MiB spread).** For reference, 551 MiB with Godot up and no encoder, and 321 MiB with no session. |
| NVENC | 2-15% utilisation at every viewer count (one encode). |
| Upstream | **8.54 Mbps per peer, linear**, spectators included: 8.55 / 17.08 / 34.17 / 51.25 Mbps at 1 / 2 / 4 / 6 peers. |
| UDP endpoints (dev pid) | 6-8 at 1 peer, 12-13 at 2, 22-28 at 4, **28-29 at 4+2**, and up to 37 at 4+3. **At 4 peers, 8 of the 9 non-mux ports in 40311-40319 are held**, and from there on some offers carry `srflx=0` (Findings 2). |
| Joiner time to first frame | 1.05-1.54 s on LAN, from RTCPeerConnection creation to the first presented frame (n=11 joiners). Joiners on a relayed pair took 1.25-1.31 s, or 3.09-3.31 s for three of them. The first viewer takes 0.82-0.86 s on LAN, including the encoder start. |
| `slot_stats` vs the hook | **0.0 ms difference** for every slot of every run: 36 player slots over 18 runs, comparing the row with the hook's rolling window at its last push. |
| Spectators send nothing (live pages) | **0 `RTCDataChannel.send` calls** from 6 spectator pages across 3 runs, with `send` wrapped before any page script ran. The host logged no dropped input for them. Positive control: each spectator then forged 2 messages, and the host logged `dropped 1 input message(s) ... no slot (input-events)` twice per spectator peer. |
| Stage 4's "2 fps at 375px over TURN" | **Not reproduced.** A 375px mobile-emulated viewer over forced relay decoded 60.0 frames/s (`totalVideoFrames`). Its rVFC (presented frame) count was 41-55/s, the same as a desktop viewer's, and host-a sent it the full 8.54 Mbps. It is not a relay limit. See Findings 6. |

## Rig and method

### Fixture

- The fixture is dev-DB task `01a0ea90-0f73-7001-8f5f-07b3db421896`, "ExampleGame four-square multiplayer fixture (01a0dbd6 stage 5)", status `in_review`. Its project is ExampleGame, with repo_url `https://github.com/example-owner/example-game`.
- Its branch is **`task/01a0ea90-0f73-7001-8f5f-07b3db421896`** on example-owner/example-game at `643edd99`: ONE commit on main (bebe173).
  - It adds `res://main/four_squares.gd`: a CanvasLayer (layer 64) with four 40 px squares, one per device 0-3, each with a `P<n> x,y` readout.
  - It adds a `FourSquares` node to `res://main/main.tscn`, the run/main_scene.
  - Each square moves by `Input.get_joy_axis(d, JOY_AXIS_LEFT_X/Y)` and by WASD in `_input`. Nothing is drawn in the top-left 64x64 px, which is the probe corner.
  - **Delete the branch once the task-level review (stage 6) is done.**
- **Deviation from the stage text, recorded on purpose.** The stage says to filter keys on `event.device == d`. On Godot 4.7.2, the `suite_play` addon delivers slot d's KEYS on `event.device == 16 + d`: Godot's keyboard device is 16, and the `ui_*` actions are bound to it (measured in stage 3). Pads arrive on `device == d`. So the fixture filters keys on `event.device - 16 == d`. With the stage's filter, only a pad could move a square, and no key from any slot would.
- **Checked live** (`fixture-*-01a0dbd6.jpg`). P1 and P2 joined from two contexts. P2 held D for 1.5 s, and in P1's view only the P2 square moved (380 to about 770). Then P1 held W, and only P1 moved.
- The session was started through the real `GameStream.start_session` path: in_review task, project repo, and `task/<full uuid>` branch.
  - The dev container has no `gh`, so the default runner cannot read the branch head. It fails with `gh_unavailable`, and the rig passed the sha through the `:runner` option instead.
  - The sha is 643edd99, read from `git ls-remote` on host-d.

### Driver

The driver is `spike/driver/run-probes-multi.mjs`. It uses Node 22+ with no dependencies, and passes `node --check`. It runs on host-d against Chrome on host-b over an ssh tunnel. Each run is one fresh session, so each run gets its own `game_stream_sessions` row.

- **One browser context per viewer**, each signed in as a distinct dev user (`/dev/login?as=<name>`). So every viewer has its own LiveView and its own RTCPeerConnection.
  - Viewers open one after another. Every viewer after the first is a joiner onto a running encoder.
  - Players click Join, then capture. They send a key every 10 s (D and A alternately) so the hook's probes never idle-stop.
  - Spectators click the video and press keys too, as a person would.
- **Raw samples.** The driver wraps `RollingStats.push` on the live hook instance, found through `liveSocket.owner`, and keeps every sample with its slot and time.
  - `lost` is probes sent (from the data-channel send log) minus samples minus probes still pending.
  - Every player collected at least 50 samples.
- **Phase jitter (`--jitter-ms 34`).** This is the one rig change to the hook's behaviour, and it is the reason the first series is an appendix.
  - The hook probes from a 1000 ms `setInterval`, which is exactly 60 frames. Its latency is read from the rVFC timestamp, which is quantized to the 16.7 ms compositor frame.
  - So every probe in a run lands at nearly the same frame phase, drifting slowly, and a run samples one or two 16.7 ms buckets. c2-lan read 47-49 ms for 34 of its 50 samples (and 16 ms for 6 of its first 7); c1-lan read only 30-32, 47-49 or 81 ms. The bucket differs run to run and slot to slot by luck.
  - With `--jitter-ms 34`, every 1000 ms interval fires after 1000 + U[0, 34) ms, so the probes sweep two frame periods.
  - Every table below uses the jittered runs. The phase-locked first series is in the appendix. Its per-slot p50 spread (32-48 ms for the SAME configuration) shows why it cannot be compared across viewer counts.
- **What the number is.** It is the Burrow hook's own measure, the same as the footer readout and `slot_stats`: probe sent to the corner decoded in rVFC. It inherits the stage 1 method's resolution of one compositor frame.
- **Counters.**
  - On host-a, a read-only sampler logged every 2 s: nvidia-smi memory, encoder and GPU utilisation, adapter byte counters, and `Get-NetUDPEndpoint -OwningProcess <dev pid>`.
  - On host-b, the rig logged whole-host CPU and the Chrome container's cgroup CPU every 3 s.
  - Each figure in the tables is taken over the probe window only: all viewers open, until the last player has n samples.
- **Host-b had to be quiet.** Other agents' `core-test` runs share host-b. The first 4-player run measured with host-b at 84% busy: Chrome at 1.2 cores, decode 1.45 ms/frame, p50 about 58 ms.
  - That run was discarded, and from then on each run waited for host-b to be under 15% busy.
  - rp2mj ran with host-b at 48% (a sibling test started mid-run). It is in the tables and flagged, but it is not used for conclusions; rp2mj2 repeats it.

## Per-slot table (jittered runs)

"Path" is the selected candidate pair in that viewer's browser. With the ICE policy "all", Chrome sometimes picked the host-d TURN relay even on the LAN runs. The CIs are 90% bootstrap intervals.

| viewers | run | slot | user | n | lost | p50 ms (90% CI) | p95 ms (90% CI) | path | host-b busy % |
|---|---|---|---|---|---|---|---|---|---|
| 1 (control) | c1j-lan | P1 | user-1 | 50 | 9 | 36.9 (34.8-41.4) | 57.5 (49.4-75.6) | TURN | 3.3 |
| 1 (control) | c2j-lan | P1 | user-1 | 52 | 4 | 40.9 (39.4-43.3) | 64.2 (50.8-66.7) | LAN | 3.8 |
| 1 (control) | c3j-lan | P1 | user-1 | 51 | 3 | 41.5 (37.7-44.2) | 61.6 (57.3-72.0) | LAN | 4.7 |
| 2 | p2j-lan | P1 | user-1 | 56 | 2 | 42.4 (41.0-44.1) | 64.8 (55.7-76.1) | LAN | 4.5 |
| 2 | p2j-lan | P2 | user-2 | 50 | 4 | 41.1 (39.0-43.8) | 61.4 (50.1-80.2) | LAN | 4.5 |
| 2 (P2 at 375px) | p2mj-lan | P1 | user-1 | 52 | 6 | 40.1 (37.6-44.1) | 58.8 (54.9-75.4) | TURN | 4.8 |
| 2 (P2 at 375px) | p2mj-lan | P2 | user-2 | 51 | 3 | 40.8 (38.9-43.8) | 60.2 (50.0-77.9) | LAN | 4.8 |
| 4 | p4j-lan | P1 | user-1 | 57 | 9 | 43.2 (40.4-46.6) | 70.0 (54.8-73.9) | LAN | 8.8 |
| 4 | p4j-lan | P2 | user-2 | 58 | 5 | 41.2 (39.5-44.6) | 59.5 (56.9-74.9) | LAN | 8.8 |
| 4 | p4j-lan | P3 | user-3 | 51 | 8 | 43.1 (41.1-45.6) | 60.8 (58.1-76.1) | LAN | 8.8 |
| 4 | p4j-lan | P4 | user-4 | 55 | 0 | 42.9 (41.5-45.4) | 67.5 (57.2-73.9) | LAN | 8.8 |
| 2+2 spectators | p2s2j-lan | P1 | user-1 | 55 | 1 | 43.9 (42.4-46.8) | 67.2 (56.4-84.3) | LAN | 8.9 |
| 2+2 spectators | p2s2j-lan | P2 | user-2 | 51 | 1 | 40.0 (38.3-42.5) | 65.9 (60.1-67.5) | LAN | 8.9 |
| 4+2 spectators | p4s2j-lan | P1 | user-1 | 58 | 3 | 42.5 (39.9-47.9) | 69.0 (61.6-90.2) | TURN | 19.0 |
| 4+2 spectators | p4s2j-lan | P2 | user-2 | 53 | 6 | 40.2 (37.6-42.4) | 60.8 (54.8-67.3) | LAN | 19.0 |
| 4+2 spectators | p4s2j-lan | P3 | user-3 | 55 | 0 | 40.8 (37.0-43.1) | 60.5 (56.2-62.4) | LAN | 19.0 |
| 4+2 spectators | p4s2j-lan | P4 | user-4 | 50 | 1 | 44.5 (41.6-46.8) | 61.7 (57.4-63.1) | LAN | 19.0 |
| relay 1 (control) | rc1j | P1 | user-1 | 51 | 1 | 40.7 (39.6-42.5) | 50.5 (45.9-65.6) | TURN | 4.1 |
| relay 2 | rp2j | P1 | user-1 | 53 | 3 | 43.9 (42.6-45.8) | 67.7 (52.2-69.2) | TURN | 15.8 |
| relay 2 | rp2j | P2 | user-2 | 50 | 2 | 44.4 (41.8-46.2) | 67.0 (55.7-68.9) | TURN | 15.8 |
| relay 2 | rp2j2 | P1 | user-1 | 55 | 3 | 47.9 (44.1-50.2) | 62.0 (61.6-70.8) | TURN | 5.0 |
| relay 2 | rp2j2 | P2 | user-2 | 50 | 2 | 46.7 (44.6-52.1) | 69.9 (60.7-78.2) | TURN | 5.0 |
| relay 2 (P2 at 375px) | rp2mj2 | P1 | user-1 | 58 | 0 | 41.1 (38.6-43.1) | 58.6 (52.8-63.0) | TURN | 11.4 |
| relay 2 (P2 at 375px) | rp2mj2 | P2 | user-2 | 52 | 0 | 42.9 (39.1-48.0) | 63.2 (57.7-79.2) | TURN | 11.4 |
| relay 2 (P2 at 375px), host-b loaded | rp2mj | P1 | user-1 | 51 | 15 | 45.0 (43.4-50.6) | 71.4 (69.6-73.7) | TURN | **47.9** |
| relay 2 (P2 at 375px), host-b loaded | rp2mj | P2 | user-2 | 50 | 10 | 45.0 (41.2-48.2) | 70.4 (65.2-71.2) | TURN | **47.9** |

### Pooled by viewer count, and the host-side cost as each run minus the control

| configuration | runs | n | lost/sent | p50 ms (90% CI) | p95 ms (90% CI) | p50 minus control | p95 minus control |
|---|---|---|---|---|---|---|---|
| LAN 1 viewer (control) | c1j, c2j, c3j | 153 | 16/169 | 40.6 (39.2-42.1) | 61.6 (57.5-66.5) | - | - |
| LAN 2 players | p2j, p2mj | 209 | 15/224 | 41.5 (40.4-43.4) | 60.6 (57.2-64.8) | +0.9 | -1.0 |
| LAN 4 players | p4j | 221 | 22/243 | 42.9 (41.6-44.2) | 60.9 (58.1-71.4) | +2.3 | -0.7 |
| LAN 2 players + 2 spectators | p2s2j | 106 | 2/108 | 42.5 (40.0-43.9) | 66.1 (60.1-67.5) | +1.9 | +4.5 |
| LAN 4 players + 2 spectators | p4s2j | 216 | 10/226 | 41.6 (40.4-43.1) | 61.9 (60.7-63.0) | +1.0 | +0.3 |
| relay 1 viewer (control) | rc1j | 51 | 1/52 | 40.7 (39.6-42.5) | 50.5 (45.9-65.6) | - | - |
| relay 2 players | rp2j, rp2j2, rp2mj2 | 318 | 10/328 | 44.1 (42.9-45.8) | 64.9 (61.9-67.8) | +3.4 | +14.4 |

**Which side any excess sits on.**

- **Host side.** VRAM is flat, NVENC stays at 2-15%, and the upstream bytes are exactly linear. **No joiner's arrival or departure restarted the encoder.** `encoder started` appears only while no other peer was connected. In some runs it appears twice: the first viewer's `/chat` landing peer connects and closes 0.2 s later, before its canvas page connects. That is a rig artefact of `/dev/login` landing on `/chat`.
- **Viewer side.** Chrome's CPU grows with the viewer count: 0.13 cores at 1 viewer, 0.28 at 2, 0.78 at 4 and 1.28 at 4+2. Decode time grows from 0.76 to 1.0 ms/frame, on a 16-CPU host-b at 3-19% busy.
- **The p50 creep of 1-2 ms on LAN** is within the CIs.
- **The relay p50 increase of 3.4 ms**:
  - It is the only interval separated from its control.
  - It comes against a control of one run (n=51).
  - It is on a path where Chrome also picked TURN for the LAN control c1j, whose p50 was 36.9.
  - Treat it as a lead, not a result.
- **The 4-player run on a loaded host-b** (p50 about 58 ms, Chrome at 1.2 cores, decode 1.45 ms/frame) shows that viewer-side CPU contention on the measuring machine moves this number more than 4-way fan-out does.

### The 60 ms bar

**Rule applied: a slot is a finding when its p95 exceeds 60 ms plus its control's excess over 60.**

- LAN control: pooled p95 is 61.6 ms, an excess of 1.6, so the threshold is 61.6 ms.
- Relay control: rc1j's p95 is 50.5, with no excess, so the threshold is 60 ms.

Slots over the threshold, per-slot p95 with 90% CI:

- p2j-lan P1 64.8 (55.7-76.1)
- p4j-lan P1 70.0 (54.8-73.9) and P4 67.5 (57.2-73.9)
- p2s2j-lan P1 67.2 (56.4-84.3) and P2 65.9 (60.1-67.5)
- p4s2j-lan P1 69.0 (61.6-90.2); its path was TURN, and host-b was at 19% busy
- relay rp2j P1 67.7 and P2 67.0 (host-b 15.8% busy)
- rp2j2 P1 62.0 and P2 69.9
- rp2mj2 P2 63.2
- rp2mj P1 71.4 and P2 70.4 (host-b 48% busy, not used for conclusions)

One control run is itself over the LAN threshold: c2j-lan, 64.2.

What this does and does not support:

- A per-slot p95 at n=50 is the 48th of 50 samples, so 3 samples set it. Every listed CI spans the control's p95.
- These are findings by the stage's rule, reported rather than hidden.
- They are **not** evidence of a fan-out cost. The pooled p95 at 4 and at 4+2 players (60.9 and 61.9) sits on the control (61.6).
- Where the p95 sits is set by the measuring browser. The after-receive part belongs to headless software-decode Chrome (stage 1: 16-27 ms of the p50). A real desktop viewer with hardware decode is not measured here.

## Fan-out cost

| run | peers | VRAM MiB | NVENC % | host-a tx Mbps | per peer | UDP endpoints (dev pid) | host-b busy % | Chrome cores |
|---|---|---|---|---|---|---|---|---|
| c1j-lan | 1 | 811 | 3-12 | 8.55 | 8.55 | 6-7 | 3.3 | 0.13 |
| c2j-lan | 1 | 811 | 3-11 | 8.54 | 8.54 | 7-12 | 3.8 | 0.14 |
| p2j-lan | 2 | 811 | 3-12 | 17.08 | 8.54 | 12-13 | 4.5 | 0.28 |
| p4j-lan | 4 | 811 | 9-11 | 34.17 | 8.54 | 22-28 | 8.8 | 0.78 |
| p2s2j-lan | 2+2 | 811 | 8-11 | 34.16 | 8.54 | 22-23 | 8.9 | 0.70 |
| p4s2j-lan | 4+2 | 811 | 9-13 | 51.25 | 8.54 | **28-29** | 19.0 | 1.28 |
| rc1j (relay) | 1 | 811 | 2-12 | 8.54 | 8.54 | 6-7 | 4.1 | 0.13 |
| rp2j2 (relay) | 2 | 811 | 4-14 | 17.08 | 8.54 | 12-13 | 5.0 | 0.30 |

- **VRAM.** Measured with nvidia-smi, whole GPU; WDDM reports no per-process figure.
  - 321 MiB before any session. No llama-swap model was resident, and prod was idle.
  - 551 MiB with the fixture's Godot running and no encoder.
  - 811 MiB with the encoder running, at every peer count.
  - **The claim is flat within about 50 MiB. It holds, with a spread of 0 MiB.** One encode feeds every peer.
- **Upstream.** Taken from the delta of `Ethernet 2` `SentBytes` over the probe window, divided by the peer count. The 8000 kbps CBR stream plus RTP/SRTP overhead costs 8.54 Mbps per peer, and a spectator costs exactly what a player does. So the relay path in `max_spectators: 4` is about 8.5 Mbps per relayed viewer, as stage 2 bounded it.
- **UDP endpoints held at 4+2** (p4s2j, the last sample in the window): 29 in total.
  - `<host-a>:40310`, the ICE mux, carrying every peer's host candidate.
  - `127.0.0.1:40331`, the RTP loopback from ffmpeg.
  - 8 of `0.0.0.0:40311-40319`.
  - 18 OS-ephemeral sockets, 3 per peer, for the TURN relay allocations.
  - `0.0.0.0:5353`, mDNS.
  - At 4+3 (p4s4j, a failed run, see Findings 3), all 9 of 40311-40319 were held, with 37 endpoints in total.
- **Joiner time to first frame.**
  - It is measured in each page from `RTCPeerConnection` construction to the first `requestVideoFrameCallback`.
  - On LAN, 11 joiners took 1.05-1.54 s. The play-host logged `PLI/FIR ... no forced IDR in the subprocess encoder; next IDR within GOP 120` for the joiners (5-29 PLI lines per multi-viewer run).
  - GOP 120 at 60 fps means waiting up to 2 s for an IDR, so 1-1.5 s is inside the expected bound.
  - The first viewer took 0.82-0.86 s. The encoder starts on its connect, so its first frame is an IDR.
  - Relayed joiners took 1.25-1.31 s, and three took 3.09-3.31 s. Measured from page open, those three took 3.5-3.7 s.
  - In the same windows, the host logged offers `gathered in 5.03s partial=true srflx=2` for some peers (Findings 2).
- **`slot_stats` vs the hook's samples.** For every run, the row's per-slot `rtt_p50_ms` and `rtt_p95_ms` equal the hook's rolling-window p50 and p95 at its last `game_stream_stats` push: a difference of 0.0 ms over 34 player slots. (The driver releases capture, waits one push and then stops the session, so the window is frozen.)
  - `peak_players` and `peak_spectators` matched each run: for example 4/2 at 4+2 and 2/2 at 2+2.
  - One caveat for readers of the row: the rolling window is the last 60 samples, not the whole run. That is what the hook reports, and it is why the raw-sample p95 in the tables can differ from the row when n > 60.
- **Spectators on live pages** (the reviewer's item 2). In p2s2-lan, p2s2j-lan and p4s2j-lan, the 6 spectator pages made **0** `RTCDataChannel.send` calls between page start and the end of the run: no key, pad, release_all or probe. That held although each clicked the video and received the same key presses as the players.
  - The host logged no `dropped` line for any spectator peer in those runs. The line is written only when the per-peer counter is above 0.
  - Positive control, `--forge-spectator` in p2s2j-lan: after the honest count was read, each spectator sent two forged key messages (`slot: 0`) on the host's `input-events` channel. The host logged `peer <id>: dropped 1 input message(s), 1 in total: no slot (input-events)` and then `2 in total` for each spectator peer. So the counter fires when a spectator sends, and it did not fire before.

## Relay and the 375px viewer (the reviewer's item 1)

rp2mj2 ran force_relay with P2 in a 375x812 mobile-emulated context. The P2 viewer's inbound
stats show 60.0 decoded frames/s (`totalVideoFrames` over the window). Its rVFC presented-frame
count was a median of 55/s. It got the full 8.54 Mbps (host-a's per-peer upstream is identical
to the desktop peer's), and its decode time was 0.83 ms/frame. Its p95 of 63.2 ms (57.7-79.2)
matches the desktop relay peer's 58.6 ms (52.8-63.0).

**The 2 fps seen in stage 4 is therefore not a relay throughput limit, and not a cost of
mobile emulation.** In every run here, rVFC presented 35-56 frames/s (per-viewer medians) while 60 were decoded.
Presented frames depend on the headless compositor, not on the stream. Hypothesis, not
measured: stage 4's 375px context was a target Chrome did not paint while another target was
foremost. rVFC fires only for painted frames, so the footer's fps then counts almost nothing
while the decoder runs at 60. That readout is the hook's `fps`, and it is rVFC-based.

## Findings

1. **A joiner can stay on Connecting forever because its offer never takes effect** (2 of about 60 viewer opens).
   - In p2s2j-lan (first attempt, 01:20Z) and p4s4j-lan (first attempt, 01:46Z), the host logged `play_peer_open <peer>` and then `offer (peer <peer>): gathered in 3xms`, and never `viewer connected` for that peer.
   - The page held the panel `connecting`, and the hook had no peer connection for 30 s.
   - In the second case the DIAG (`superseded/run-p4s4j-lan-01a0dbd6.log`) shows a `peer_id`, server state `live`, and **no RTCPeerConnection ever constructed** in that page.
   - The hook never answered. Its "Still connecting" plus Reload path is the only way out.
   - Cause not established. The instrumentation that logs every `game_stream` socket frame and the hook's `onServer` calls from page start was added after the second occurrence. The stall did not recur in the 7 runs after it (19 viewer opens).
   - The next person to see it should read `DIAG.ws` and `DIAG.server`. They tell whether the offer frame reached the socket, reached the hook, and was dropped by `onServer`'s `active()` / `session_id` guard.
2. **The 10-port UDP range fills at about 4 peers, and later offers carry no srflx candidate.**
   - The ICE mux does carry every peer's host candidate on 40310. But each peer's server-reflexive gathering still binds ports from the ephemeral range 40311-40319, and holds them while the peer lives.
   - At 4 players, 8 of the 9 were held. Offers then came as `srflx=0` (2 of 12 offers in the 4+2 run, 5 of 12 in the 4+3 run), and some took `gathered in 5.03s partial=true`.
   - On the LAN, the host candidate and the relay candidates still connect everyone, so no viewer failed from this.
   - Off-LAN, a viewer with no TURN route that needs the srflx pair would not connect. And the 5 s partial gathers add 5 s to a joiner's first frame.
   - Stage 3's "one mux socket for 3 connected peers" is true of host candidates only.
   - The smallest fix is on the play-host: give srflx its own mux (pion `SettingEngine.SetICEUDPMux` covers host only; srflx has `UDPMuxSrflx`), or widen `udp_max`. This is not done here.
3. **The last spectator seat is refused when the same user's previous page still holds a seat.**
   - In p4s4j-lan (second attempt), with 4 players and 3 spectators attached and `max_spectators: 4`, the 8th viewer's canvas page got a refused attach: panel `failed`, no `peer_id`, and the roster read "4 watching".
   - Inference, not measured: that user's `/dev/login` landing page (`/chat`, which renders the canvas inline and attaches as a spectator) still held a seat when the canvas page attached.
   - Refused is final for that page: it shows "This session is full" and does not retry when the stale seat frees.
   - The rig tripped it because every viewer lands on `/chat` first. A real user navigating from chat to the canvas page would trip it the same way at the cap.
4. **The dev DB had never run migration 20260928200000** (`add_player_dimensions_to_game_stream_sessions`, stage 2). Session rows from the stage 1-4 dev rigs failed to insert (`column g0.slot_stats does not exist`). The migration was run on the dev DB at 00:37Z, before any measured run. This is a rig fact, not a code defect.
5. **The probe cadence is phase-locked to the frame clock.** The hook probes every 1000 ms, which is 60 frames, and reads latency at the 16.7 ms compositor frame. So within a run every sample lands in the same bucket, and the footer's p50 and `slot_stats` for one session can differ by a whole frame (16.7 ms) from the next session's for no physical reason. Seen in the appendix. Jittering the probe tick in the hook by U[0, 1 frame) would make the readout represent the phase distribution. That is a core change, not made here.
6. **The footer's fps under-reports in headless Chrome.** It counts rVFC-presented frames, 40-55/s, while 60 are decoded. On a real display it should track decode. It explains the reviewer's item 1.

## Stage 6: the runtime socket breaks were the dev rig's port forwarder

The task-level review saw the host's runtime socket break twice in about 35 minutes of relay runs. The first was Bandit's `Received unexpected binary frame (RFC6455§5.4)`, close 1002. The second was a stall: an offer the host logged never reached core, and the host's heartbeat timed out 51 s later. Each break ended the session for every viewer. The hypothesis was a second writer interleaving a frame between the fragments of a >4096-byte offer. **That hypothesis is wrong.**

**The mechanism, measured.** podman's `rootlessport`, which publishes the host-b dev core on `<host-b>:4033` (podman 5.6.2, rootlesskit 3.0.0, kernel 6.16.8-200.fc42), sometimes hands the container different bytes from the ones the host sent, when traffic flows in both directions. Between the two capture points are the LAN (covered by TCP checksums), the host-b NIC and kernel, and rootlessport; the control below shares every hop except rootlessport and was clean. Evidence (raw outputs in `multiplayer-01a0dbd6-results/stage6/`):

- **Both ends captured.** pktmon ran at host-a's NIC, filtered to TCP 4033. tcpdump ran in the dev core's network namespace (`--network container:`). For the same connections, the stream host-a sent parses clean under a strict RFC 6455 reassembler (`wsframes-01a0dbd6.py`, Bandit's rules plus UTF-8/JSON validation of every unmasked message). The stream the container received has one frame whose payload goes wrong part-way: offsets 2069-2778 into a 6.8-16 KB frame, with bytes masked under a different key. The next "header" is then garbage: RSV bits, opcode 13, a "binary frame", or a length that stalls the reader. `streamdiff-01a0dbd6.py` on one case: identical total length; a 13,887-byte region differs; every later frame sits at the same offset on both sides.
- **The same on the play host's own socket.** In the 60-min run with the new host, the 04:57:24 and 05:13:53 offers arrived corrupted in the container and intact at host-a's NIC. Each was followed by a heartbeat miss (stall), then a resume.
- **Without any websocket code.** `spike/tcpcheck` sends a PRNG stream in writes of 1-16 KB from host-a, and a sink verifies every byte.
  - One-way traffic, 1.2 GB through rootlessport and 1.2 GB straight to the host-b host netns: both clean, and still clean with a slow reader.
  - With 200 B replies every 5 ms flowing back: 2 of 4 connections through rootlessport were corrupted in seconds (at bytes 11,273,679 and 28,023,954). The same traffic straight to the host netns: 0 of 4.
  - Later bidirectional batches: 1 of 16 connections corrupted, then 0 of 48. It is intermittent.
- **Fragmentation is not the trigger.** On the old binary (gorilla's 4096-byte buffer), 150 of the host's 295 messages in 25 min of relay runs were fragmented offers, and all were intact. The stress bursts broke at 16 KB single frames as well (7 drops in 8 min; none in the 4 min at 4096, but the checker's size comparison, 0 of 24 connections against 0 of 24, shows no size effect).
- **Not a concurrent writer.** Every write goes through `Client.write` under one mutex. Removing that lock makes gorilla PANIC ("concurrent write to websocket connection") under `TestConcurrentFragmentedPushesNeverInterleave`, rather than emit an interleaved frame. The host process never crashed in the review.

**Re-run the raw-TCP reproduction** (`spike/tcpcheck`; ports 47101/47102 are free on host-b and far from the prod play-host's 40300-40330):

```
# build (any box; Go 1.25)
CGO_ENABLED=0 go build -o tcpcheck ./spike/tcpcheck
GOOS=windows go build -o tcpcheck.exe ./spike/tcpcheck
# host-b: one sink behind rootlessport (a rootless bridge container), one on the host netns
podman run -d --name tcpcheck-rlp --network dev -p <host-b>:47101:9000 -v $PWD:/w:z \
  docker.io/library/alpine:latest /w/tcpcheck serve -addr :9000 -slow 1ms -read 8192 -reply 5ms -reply-bytes 200
./tcpcheck serve -addr <host-b>:47102 -slow 1ms -read 8192 -reply 5ms -reply-bytes 200 &
# host-a (or any LAN client): 4 connections x 40 MB each way, writes of 1-16 KB
tcpcheck.exe send -addr <host-b>:47101 -bytes 40000000 -conns 4 -min 1000 -max 16000 -pause 1ms
tcpcheck.exe send -addr <host-b>:47102 -bytes 40000000 -conns 4 -min 1000 -max 16000 -pause 1ms
# a corrupted connection logs, on the sink:  MISMATCH conn ... at byte N ... got .. want ..
# and on the sender:                         conn K seed S: server says MISMATCH N
```

Drop `-reply` to see the one-way control (clean). The corruption is intermittent: repeat the send a few times.

**What changed in play-host.** None of it depends on the forwarder being fixed. A real network can drop a socket too.

- A dropped suite socket keeps the session for 30 s (`host.Config.ResumeGrace`). The viewers' media never passes through core, so their video does not stop. On the rejoin the host sends one `play_session_status {resume: true, peers}` and re-offers every peer that is not connected: an offer or answer lost with the socket is replaced. Core (task branch, `Platform.GameStream.Session`, "A host that drops") waits 20 s, then re-sends `play_peer_open` / `play_peer_close` / `play_slots` for what the host missed. A resume core cannot place is answered with `play_session_stop`.
- The heartbeat miss check keys on when the beat was written. The old "reply before the next tick" rule killed a healthy socket when a beat's write waited behind other writes for longer than an interval. A reply that beats its pending mark is no longer lost. The default interval is 10 s, not 30 s, so a stalled socket is found in 10-20 s.
- The Dialer's write buffer is 64 KiB, so a signalling frame is one websocket frame. This is tidiness only; it does not prevent the corruption.

**Live, relay, new host (ce949a9).**

- Deliberate blip: core's RuntimeSocket was killed while three viewers were live. The host rejoined in 0.7 s and resumed with 3 peers, re-offering 0. All three stayed live on the same pc (max frame gap 251 ms), and a reload afterwards went live in 1.26 s.
- 60-min run: 3 real stalls from the forwarder, each resumed in about 1 s with the stuck joiner re-offered. All 53 case-4 runs passed until the harness session idled out at its 30-min idle timeout (no input in the repro, panel and reload steps; not a fault).

**Remaining gap, stated.** The corruption itself is still there on this rig. A stall is found by the heartbeat (10-20 s). In that window a joiner waits, and the host's frames are lost until the reconnect. Removing the cause is infra work, outside this task: publish the dev core without rootlessport (host network or pasta port forwarding), or fix or upgrade rootlessport. The other prod core on host-b is published the same way (`0.0.0.0:4000`); whether real clients reach it through rootlessport is for the infra owner to check.

### Correction (2026-09-29, after merge): rootlessport is not the sole cause

An independent reproduction refuted the attribution above. The heading and "The mechanism, measured" name rootlessport as the cause. Read them as **a byte-corruption fault on the host-a to host-b path, cause under investigation.** The resume-on-reconnect work under "What changed in play-host" stands either way.

- **The host-netns path also corrupts.** A host-b container on `--network host`, which never goes through rootlessport, corrupted 2 of 300 connections from host-a. The signature is the same: a region is replaced by the connection's own bytes from 97-1,355 bytes earlier, and the total length is unchanged.
- **rootlessport raises the rate but is not the sole cause.** Through rootlessport, 24 of 548 connections from host-a were corrupted. Through pasta, 0 of 200 were.
- **Only host-a triggered it.** Clients on host-d and on host-b itself had 0 corrupted connections on every path, rootlessport included (about 21 GB of client-to-server data).
- **host-a to host-d was clean:** 0 of 201. That run was interleaved with a rootlessport run from host-a that corrupted 13 of 200.
- **Version fix.** podman's rootlessport bundles rootlesskit v2.3.5, not the 3.0.0 stated above.
- **The stage-6 control could not tell the two causes apart.** A 0 of 4 result on the host netns cannot distinguish them at a rate of about 1-4%.
- **Current hypothesis, NOT measured:** the host-a and host-b pair. Both have RTL8125 NICs, and host-a has large send offload and checksum offload on. The missing measurement is a capture on host-b's `enp6s0` during a host-a run.
- **Raw data:** host-d `~/sources/tmp/tcpcheck-rlp/RESULTS-652388.txt`.

The "Remaining gap" remedy (publish the dev core without rootlessport) should therefore lower the rate, but it is not expected to remove the fault.

## Appendix: the phase-locked first series (no jitter)

These runs are valid in every other respect: prod was idle, host-b was quiet, n ≥ 50 and the rows match. But their samples fall in one or two 16.7 ms buckets per slot, so they are shown only to document Finding 5.

| run | slot | n | lost | p50 | p95 | samples (ms, rounded, first 12) |
|---|---|---|---|---|---|---|
| c1-lan | P1 | 50 | 0 | 32.1 | 48.9 | 32 49 49 32 49 32 32 32 49 32 49 49 |
| c2-lan | P1 | 50 | 0 | 48.2 | 49.3 | 16 16 16 16 33 16 16 49 49 49 49 49 |
| p2-lan | P1 / P2 | 59 / 51 | 2 / 8 | 34.0 / 45.4 | 50.7 / 62.3 | P2: 46 46 46 46 46 46 46 46 46 46 46 63 |
| p4-lan | P1-P4 | 61 / 58 / 50 / 53 | 4 / 1 / 8 / 1 | 46.1 / 47.5 / 41.7 / 46.3 | 47.3 / 64.0 / 58.3 / 79.6 | P4: 47 47 47 47 47 80 47 47 47 47 47 47 |
| p2s2-lan | P1 / P2 | 55 / 50 | 0 / 1 | 43.7 / 45.6 | 44.8 / 46.5 | |

## Reproduce

The rig scripts were kept with the raw run output, which is not in this repository. They were:

- `run-s5-01a0dbd6.sh <label> <players> <spectators> <relay 0|1> [mobile idx|-] [n] [jitter]` runs one session end to end: the prod check, the host-b quiet wait, session start through the dev BEAM, both samplers, the driver, the row and the host log.
- `analyze-01a0dbd6.py <labels...>` builds the tables.

The driver alone:

```
node spike/driver/run-probes-multi.mjs --cdp 127.0.0.1:9223 --app http://<host-b>:4033 \
  --canvas <uuid> --players 4 --spectators 2 --n 50 --jitter-ms 34 --label p4s2 \
  --out p4s2-01a0dbd6.json [--forge-spectator] [--mobile 1]
```

Chrome on host-b is Google Chrome, not headless-shell (spike 01a0db5f item 5):
`podman run -d --name chrome-01a0dbd6-s5 --network host --shm-size 2g --entrypoint /usr/bin/google-chrome docker.io/selenium/standalone-chrome --headless=new --no-sandbox --remote-debugging-port=9223 ...`,
reached with `ssh -L 9223:127.0.0.1:9223 user@host-b.local`.
