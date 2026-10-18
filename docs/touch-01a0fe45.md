# Touch input for game_stream: rig evidence, decisions and rollout (01a0fe45 stage 5)

Task 01a0fe45 forwards touch from the Burrow `game_stream` canvas through the play host to Godot.
This document is stage 5's record. It covers the dev rig run end to end, what it measured, the
decisions the feature rests on, and the rollout. The raw driver output, host logs,
screenshots and rig scripts are not kept in this repository; this document is their summary,
and the `rig/` paths below name the scripts as they were run.

**Real-device gap, stated first:** agents have no real phone. Everything below is emulated
Chromium (headless Chrome 153 with CDP touch emulation). These are UNVERIFIED:

- iOS Safari/WebKit pointer behaviour;
- real finger jitter against the 8 px slop;
- safe-area insets;
- a real device pixel ratio and real rotation.

The ticket's real-phone acceptance is therefore **not met by agents**.

Evidence canvas: `01a0fec3-1480-7001-a603-b664eb523178`, in execution space 01a0fe45-da80-7001-b06a-a832f29ff012. The 5-line checklist for
An operator is under "Real-phone check".

## Tested versions

| | |
|---|---|
| core | `task/01a0fe45-d117-7001-a501-ad6244a19795` at **f77d1e96**. The touch runs ran on the served with-Play bundle of 480b028d. f77d1e96 changes only `game_stream.css`, and the fullscreen runs used its CSS. |
| play-host | `task/01a0fe45-d117-7001-a501-ad6244a19795` at **f3eaa22**: stage 4's addon plus this stage's dev-rig config keys. The exe was built `-ldflags -X main.Version=f3eaa22e…`; sha256 `89B7028F…3D49`, addon `EBAF4A79…2467`. |
| Godot | 4.7.2-stable (winget `Godot_v4.7.2-stable_win64_console.exe` on host-a), `--rendering-driver vulkan` |
| Chrome | Google Chrome 153.0.8010.47 (`selenium/standalone-chrome`, `--headless=new`) on host-b |
| Game | Survival Game review build **3c921947118817c47ccea446aa7c6e437531102b** (branch `task/01a0fe15-e5c7-7001-acb7-83bc3d332ede`). At the start of this stage, origin had no newer landscape build: main ad25c6b is still portrait (`git ls-remote`, 22:00Z). |

## Rig

- **Dev core on host-b:** `dev-platform-01a0fe45-…` on `<host-b>:4034`, `GAME_STREAM_ENABLED=true`.
  - **Rig fact, not a defect:** dev-on-host-b serves `apps/platform`, which is the without-modules build since ADR 0055, so it has no Play. Measured: the served `app.js` had 0 matches for `game_stream_touch`.
  - The rig therefore recreates the container from `apps/suite_release`, the with-Play release host (`rig/devplay-01a0fe45.sh`). It copies the with-Play `priv/static/assets` into core's static dir inside the container and migrates with `Platform.Release.migrate/0`.
  - The served bundle was then checked for this branch's code: `game_stream_touch`, `data-gs-touch` and `Touch the game to play` are all in `app.js`.
- **Host-a dev play-host:** `C:\Users\operator\play-host-dev-01a0fe45\`, scheduled task `suite-play-host-dev-01a0fe45`, firewall rule `suite-play-host-dev-udp-01a0fe45`, runtime `play-host-a-dev-01a0fe45`.
  - Ports: UDP 40310-40319 (mux 40310), godot 40321, **rtp 40332 / audio 40333**.
  - **Deviation from the stage text:** the text says rtp 40331. That is prod's AUDIO RTP port. Since audio (play-host PR #4), `audio_rtp_port` defaults to `rtp_port + 1`, and prod's config sets `rtp_port: 40330` and no audio port.
  - The dev token came from the host-b dev core's DB (`Federation.activate_runtime/1`), was written to the dev `secrets\` with an ACL of the operator account and SYSTEM only, and was shredded on host-d.
  - The `repos` entry reuses prod's `survival-game-deploy-ed25519` and `known_hosts` read-only. The mirror and checkouts are the dev dir's own.
- **Prod check before and after every run** (`rig/prodcheck-01a0fe45.ps1`): it asks whether a Godot or ffmpeg child points at the prod checkouts, and whether the last session line in the prod log is an end.
  - Every run read `PROD_BUSY=False` both times.
  - The check was shown to fire first: pointed at the busy dev instance, it returned `PROD_BUSY=True children=2`.
  - Prod's pid (8928) was unchanged across every dev deploy (`deploy*-01a0fe45.log`).
  - Prod's own log shows one reconnect at 22:14:44Z, `host: joined; ready for play_session_start`. That is prod's suite socket, and no process of this rig touched it.
- **Fixture:** a dev project with `repo_url` `https://github.com/example-owner/survival-game`, and a dev task whose **id equals the real Survival Game task's** (01a0fe15-…). ReviewBuild uses the task's own `task/<id>` branch, so this is what makes it resolve. The task is set `in_review`.
  - The dev container has no `gh`, so the default runner fails with `gh_unavailable`. The sha is handed in through the `:runner` option (recorded as `via` in every start), as in 01a0dbd6.
- **Viewers:** one browser context per viewer, with `Emulation.setDeviceMetricsOverride`:
  - portrait 390x844 DPR 3 mobile;
  - landscape 844x390 DPR 3 mobile;
  - desktop 1280x800 DPR 1.

  Each context also gets `Emulation.setTouchEmulationEnabled(maxTouchPoints 5)`, and taps and drags are sent with `Input.dispatchTouchEvent`. The page saw `pointerType` touch only, plus Chrome's compat `click` on the VIDEO element after each tap, which is the duplicate risk under test.
- **Observation:**
  - `log_touch: true` (new, below) logs one `touch line {…}` per st/sd sent to the addon, releases included.
  - The game's own state is read from the decoded frame, with every frame screenshotted. The Pause button's fill changes when it latches (mean RGB 44,46,43 → 32,34,32 in all five tap runs). The HUD reads Paused/Running, People N and Food N. Add child turns disabled at 3 citizens, and Nell enables.
  - **Targets are located in the frame, not hard-coded.** The rig draws the `<video>` into a canvas and finds Survival Game's button rows by colour (fill 31,33,30 against panel 38,43,37). The game's ScrollContainer and the page both scroll, so a fixed coordinate drifts. The first attempt with fixed coordinates hit the settlement grid and activated nothing, which is the positive control for "a miss activates nothing".

## Results

Counts are per action, read from the dev host log as `st p:true / st p:false / st c:true / sd`.
The browser-side normalised x,y and the host's x,y matched exactly (0.00% difference) for every
tap. Both come from the same content rect; the real check of the mapping is that the control
under the finger activated.

| run (viewport) | Pause | drag x2 (scroll) | Add child | Food shortage | drag x2 back | Reset | letterbox/pillar bar tap | total lines |
|---|---|---|---|---|---|---|---|---|
| portrait 390x844 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | no bar (the well takes the frame's aspect) | 82 |
| **landscape 844x390** | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | no bar | 80 |
| desktop 1280x800 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | no bar | 82 |
| desktop fullscreen (letterboxed: 1280x674, frame 303 px wide) | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | **0 lines, 0 touch sends** | 82 |
| **landscape fullscreen** (844x264, frame 119 px wide) | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | 1/1/0/0 | 1/1/0/10 each | 1/1/0/0 | **0 lines, 0 touch sends** | 82 |

- **Every control activated exactly once in every viewport**, read from the game itself:
  - Pause latched. A second activation would have unlatched it.
  - Add child took People 2 → 3, enabled Nell and disabled itself.
  - Food shortage took Food to 0 and Hunger to 24.
  - Reset took People back to 2, Tick to about 0 and the state to Running.
- **Every tap was one st down and one st up at one index (0), with no sd**, so each tap stayed inside the 8 px slop.
- **Every drag was 10 sd lines on one stable index** and scrolled the game's ScrollContainer in the drag direction. The drags start in the frame's right margin (x 0.975), off the settlement view.
- No mouse line was ever emitted. The addon builds none.
- Burrow's click-to-capture never engaged: hint `""`, no ring. The canvas sidebar never opened, page zoom stayed 1, and the page did not scroll during in-video drags (`scrollY` 0 throughout).
- **Rotation without reload** (portrait → landscape mid-session): one Pause tap in each orientation gave 1/1/0/0 each. The landscape tap mapped to the 140 px wide well at x 351.8, as recomputed per event.
- The latency probe still ticks once a second on `input-events` (`probe`), as before. It is the only send a bar tap coincided with.

### Spectator: zero writes, with a forged positive control (run `spectator`)

- A spectator phone context (user-3) tapped Pause, Reset and an empty spot, then dragged. Its well had no `data-gs-touch` and `touch-action: auto`, so the drag scrolled the Burrow page.
- **Its data-channel sends: 0.**
- It then sent two forged `{"t":"touch",…}` messages (one carrying `"slot":0`) on its real `input-events` channel. The host dropped both, `peer 01a0feb8-b657…: dropped 1 input message(s), … no slot (input-events)`, and counted 2 in total.
- **Positive control in the same run:** P1's Pause tap reached the host (1/1/0/0 at index 0) and latched the game.

### Keyboard and gamepad regression (run `keypad`)

- Desktop context D (user-2, no touch) joined as P2 and captured the keyboard.
- **Keys** Space, Space, C, F, R gave the game states Paused, Running, People 3, Food 0, People 2 (`08-keyboard-gamepad`). Every key went out as a `key` message.
- **Gamepad** (a fake standard pad through `navigator.getGamepads`): Start, Start, Y, X, Back gave Paused, Running, People 3, (Food already 0), People 2. They went out as `pad` messages.
- **Totals:** `key` 10, `pad` 11, `release_all` 1 on Escape. P1's touch still worked in the same session (2 taps, 2/2/0/0).
- The host does not log key or pad lines, so "device 16+slot / device=slot" is not re-measured here. It is unchanged code (stage 3's unmodified key/pad tests) and the game reacted.

### Capability gate, negative control (run `notouch`)

The same dev host was redeployed with `features: [game_stream_host, game_stream_multi]`. Core
then reported `touch: %{enabled: false}` for the session. On a phone as P1:

- there is no `data-gs-touch` and `touch-action` is `auto`;
- the footer reads `Connect a controller to play on this device`, and the `Click to play` panel shows;
- a tap sent no touch message (only the probe);
- a vertical swipe on the video **scrolled the Burrow page** (container scrollTop 0 → 276);
- the host logged 0 touch lines.

## Findings

1. **Fixed here (core f77d1e96): fullscreen showed a blank frame for a portrait stream.**
   - Stage 2's portrait rule sets `margin-inline: auto` and `max-height: 80svh`. The fullscreen rule reset only `aspect-ratio`, and its comment claimed it "wins". It wins only for what it sets.
   - `margin-inline: auto` on a column flex item shrinks it to its content, and the video is absolute, so the well was 0 px wide. Measured: `videoRect` width 0, height 640 at 1280x800 (`05-fullscreen-before-480b028d`). After the fix the video is 1280x674 with the 303 px frame pillarboxed (`06-…after-f77d1e96`), and both fullscreen tap runs above passed.
   - Guard: `assets/test/game_stream_css.test.js` fails if the portrait rule gains a property the fullscreen rule does not reset. Mutation: dropping `margin-inline: 0` fails 2 of its 3 tests (log `mutation-css-01a0fe45-*.log`).
2. **Not fixed, needs a decision: the inline well does not fit a phone's visible area, and landscape is the worse case.**
   - The 80svh cap ignores the app chrome. Measured with `elementFromPoint` down the well's centre line:
     - **portrait 390x844:** well 303x675 at top 182. The bottom nav covers it from 787, so at load the touchable band is frame y 0-0.90. A 41 px page scroll widened it to 0-0.96. The band can be up to about 702 px (787 minus the sticky header's bottom, about 85, measured in landscape), so the 675 px well fits once scrolled.
     - **landscape 844x390:** well 140x312 at top 166. At load only frame y 0-0.54 is touchable. The band is about 248 px, measured as 0.13-0.93 at scrollTop 123 and 0.07-0.87 at 102, so **at most about 80% of the frame is touchable at any page scroll position**. The whole frame never is. The buttons are about 43x14 CSS px, below the 44 px target size.
   - The page does scroll by a touch swipe beside the well (the rig does this, `page_swipes` in the JSON), so every control was reachable. A person has to know to do it.
   - The game is the other half. **A landscape game in a landscape phone is the case the operator asked for.** On the stage 2 design that is an `aspect-video` well about 780x439 in a visible band of about 239 px, which has the same problem.
   - Fullscreen fixes it on Chrome/Android (landscape fullscreen: 844x264, all controls 1/1). iPhone Safari has no Element fullscreen.
   - A follow-up should cap the well to the scroll container's visible height (the hook can measure it), for 16:9 wells too. That is a layout decision beyond this stage.
3. **Rig infra:** the host-d-to-host-b sync watcher for this worktree exits as soon as it starts its inotify watch (`dev-on-host-b-sync@01a0fe45-….service` failed, 5 restarts at 16:56). The initial rsync works. The one CSS edit was copied by hand. This is for the infra desk, not a product defect.

## Wire frames (the contract is core's `Platform.GameStream.Protocol` moduledoc, "Touch")

- Browser to host, on `input-events` (reliable, ordered): `{"t":"touch","src":0,"id":0..9,"ph":"down|move|up|cancel","x":0..1,"y":0..1}`. x,y are normalised to the displayed video content (the decoded frame), not the element.
- Host to addon:
  - `{"t":"st","d":slot,"i":slot*10+id,"p":bool,"c":bool,"x":nx,"y":ny}` builds an InputEventScreenTouch (`c` = canceled);
  - `{"t":"sd","d":slot,"i":…,"x":…,"y":…}` builds an InputEventScreenDrag.

  `release` and `release_all` lift held touches as `st p:false c:true` at the last position. The slot comes only from the host's table, never from the message.
- Measured in this run: `{"c":false,"d":0,"i":0,"p":true,"t":"st","x":0.178,"y":0.1354}`, then `{"d":0,"i":0,"t":"sd",…}` ×10 for a drag.

## Decisions

- **Capability gate.** Core enables touch only for a host that declares `game_stream_touch`, and fails closed on an undeclared feature list. A host without it leaves the browser exactly as main, as the negative control above shows. **So core can ship before the host-a swap.**
- **Experiment surface `game_stream.touch`** (subject task, target `play_session.start`), payload `slop_px` 8 (0..32) and `move_hz` 60 (15..120).
  - **Named metric gap:** no metric judges it. Touch never transits core, and the host reports no tap/drag counter, so the arm is recorded only in session state and public status. A judging metric is a follow-up.
- **Rate limit.** Before this task there was **no input rate limit**. play-host `onInput` had none, and the only throttle was `play_slot_activity` at 4/s. `move_hz` (browser) and the host's 4 ms per-(slot, id) move bound (`DropTouchRate`) are new bounds, not preserved ones.
- **Multi-player mouse emulation.** The addon never builds a mouse event. Controls activate through Godot's `emulate_mouse_from_touch`, which follows one touch at a time.
  - With two players touching at once, only the first touch drives mouse-only Controls.
  - `BaseButton` in 4.7.2 also reads raw ScreenTouch, so a second player's tap does press a Button, once (stage 4, measured).
  - A game that turns emulation off must handle ScreenTouch itself.
  - The addon sets `Input.emulate_touch_from_mouse` on the first touch line, because on Windows without a digitizer that is what makes ScrollContainer drag-scroll. Without it, measured, the scroll is 0.
- **Portrait well.** A stream taller than 16:9 gets a well with the frame's aspect, capped at 80svh. A 9:16 game pillarboxed in a 16:9 well is about 119 px wide on a 390 px phone, which is untappable, and iPhone Safari has no Element fullscreen to escape it. Landscape streams keep `aspect-video`. See Finding 2 for where the cap falls short.

## What changed in play-host this stage (f3eaa22)

Two config keys, for dev rigs only. Host-a's prod `config.json` sets neither, and absent keys behave
exactly as before (`TestDevRigKeysDefaultOffAndParse`).

- `log_touch` (bool): one `touch line <json>` per st/sd sent to the addon, releases included. `TestLogTouchLogsEverySentTouchLine` covers both on and off. Mutation: dropping the LogTouch condition turns the off case red (3 lines logged, want 0).
- `features` ([]string): replaces `client_info.features`. This is how the negative-control host above ran.

## Rollout (binding text for the deployer)

1. **Core ships through the auto-injected deploy stage.** It is SAFE ALONE: a host without `game_stream_touch` leaves the browser exactly as main (stage 1's gate test, and the negative control above). That now includes f77d1e96's fullscreen CSS fix, which only touches the portrait well inside fullscreen.
2. **The deployer also merges the play-host PR** from `task/01a0fe45-d117-7001-a501-ad6244a19795`. play-host has no CI, so record the stage 3/4/5 test results in the PR (go checks, selftest and the stage 5 go test run).
3. **Merging play-host deploys NOTHING.**
   - An infra-dispatched executor swaps `play-host.exe` AND `addons/suite_play/suite_play.gd` into host-a `C:\Users\operator\play-host\`.
   - **The operator's standing go-ahead (2026-10-02): swap as soon as the idle check passes. No human window.** The idle check is no running prod game_stream session: `prodcheck-01a0fe45.ps1` reads `PROD_BUSY=False`, and core lists no live session for play-host-a.
   - Back up both files as `*.bak-01a0fe45-<stamp>`. Build the exe from the merged sha.
   - Restart the scheduled task `suite-play-host`.
   - Confirm that the prod core play-host-a connects to, **burrow.example.com** (its `suite_url`), lists `game_stream_touch` in play-host-a's `client_info.features`.
   - Do not add `log_touch` or `features` to prod's config.
4. **Then** an operator does the real-phone check below. After that, an agent with a grant on Survival Game space 01a0fd55-542f-7001-b7ec-3f50b659d6d8 applies the guide text below to canvas 01a0fe3b-19fd-7001-9bfa-281c5051d282.

## Real-phone check (an operator, after the host-a swap)

1. Open the Survival Game playtest canvas on the phone, **hold the phone landscape**, tap Join, and check that the footer says "Touch the game to play".
2. Tap **Pause** once. It should stay latched ("Paused"). Tap it again to resume.
3. Swipe up inside the game to scroll it. Tap **Add child** once (People 3), then **Food shortage** once (Food 0).
4. Swipe back down inside the game and tap **Reset** once (People 2, Running). The Burrow page itself must not scroll or zoom during any in-game swipe.
5. If the lower controls sit under the bottom bar, swipe beside the video to move the page (Finding 2), and note it. Also note any tap that did nothing, or did something twice.

## Drafted guide text for canvas 01a0fe3b (apply ONLY after the swap is live)

It replaces the guide card's last paragraph, which begins "**Phone limitation:**". Applying it
earlier would be false, because the prod host does not take touch yet.

> **On a phone:** join as a player and **touch the game**: tap a button to press it and swipe
> inside the game to scroll it. Hold the phone landscape; if the lower buttons are under the
> bottom bar, swipe beside the video to move the page, or use fullscreen (Android). Keyboard and
> controller still work as above. All players share one household. No save/load yet.

## Retiring the rig

**The rig is left up on purpose for stage 6.** The task-level review's setup names it: "Stage 5
dev rig … Dev host logs one line per forwarded st/sd". Without it the reviewer would have to
rebuild it. Retire it after that review with `rig/retire-01a0fe45.sh`, which does all of:

- removes the host-b dev core and its DB (`teardown-dev-on-host-b.sh <wt> --drop-db`) and the uploads volume `dev-uploads-<wt>`;
- on host-a, stops and unregisters `suite-play-host-dev-01a0fe45`, kills processes under the dev dir only, removes firewall rule `suite-play-host-dev-udp-01a0fe45`, and deletes `C:\Users\operator\play-host-dev-01a0fe45\`;
- needs nothing done to the token itself: it dies with the dev DB.

These stage 5 rig pieces were removed at the end of this stage:

- Chrome container `chrome-01a0fe45-s5` on host-b;
- the host-d ssh tunnel on 9245;
- every `core-test-task_01a0fe45*` container;
- `C:\Users\operator\survey-01a0fe45.ps1`.

## Reproduce

- `rig/run-01a0fe45.sh <label> <scenario> <vp> <canvas>`: prod check, driver, host-log slice, per-step line table.
- Scenarios are `taps`, `fullscreen`, `rotate`, `spectator`, `keypad` and `notouch`.
- Driver: `node rig/touch-01a0fe45.mjs --cdp 127.0.0.1:9245 --app http://<host-b>:4034 --canvas <uuid> --scenario taps --vp landscape --label x --out <dir>`. CDP is reached with `ssh -L 9245:127.0.0.1:9245 user@host-b.example`.
- Chrome on host-b: `podman run -d --name chrome-01a0fe45-s5 --network host --shm-size 2g --entrypoint /usr/bin/google-chrome docker.io/selenium/standalone-chrome --headless=new --no-sandbox --remote-debugging-port=9245 --remote-debugging-address=127.0.0.1 …`.
- Session control: `rig/rpc-01a0fe45.sh fixture|token|hosts|start|canvas|report|stop|row`, evaluating `rig/rig-01a0fe45.exs` in the dev BEAM. The `token` mode prints a secret: pipe it straight to the host-a file and never to a log.
- Host-a: `rig/host-a/deploy-01a0fe45.ps1 -Sha <sha> [-NoTouch]` and `rig/host-a/prodcheck-01a0fe45.ps1 [-Dir <instance>]`.
