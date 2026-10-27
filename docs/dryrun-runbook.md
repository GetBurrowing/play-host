# Dry-run runbook (sample Unreal project, full loop)

This measures one sample Unreal project end to end on the play machine: a cold
build, a warm build, and the host-to-photon probe at n=150 for each encoder
preset. It ends in one `report.json` that `play-host dryrun-check` accepts or
rejects. A missing report is a FAIL, not a pass.

Paths in angle brackets are placeholders; real values live in the machine's
`config.json` and in the thread that hands you this runbook. Run everything in
the **logged-in desktop session** (the console), in a normal Command Prompt,
not in the scheduled task and not over a session-0 service.

`wave` is Windows 11 Home. `query session` does not exist there, so every
session check below uses PowerShell and `tasklist`. The session number must
not be 0.

## What the run does

`play-host dryrun` runs the real stages (Git LFS, editor scripts, RunUAT,
launch, capture, encode) against a loopback core on 127.0.0.1 and drives the
spike's probe page (`internal/spike/web/probe.js`) in headless Chrome through
`spike/driver/run-probes.mjs`, 150 probes at about 10 Hz per cell.

1. cold: empty `ddc_dir`, no archive for the commit; preset p1/ll. The build
   time is session start to `connecting`, split per step.
2. probe cells: p1/ll (from the cold session) and p4/ll (a second session that
   reuses the cold archive). Only the ddagrab backend exists, so the backend
   axis has one value and the matrix is ddagrab x {p1/ll, p4/ll}.
3. warm: same `ddc_dir`, a fresh checkout, archive cache off (the build runs
   again, with a warm derived-data cache).

`build_ms.cold` and `build_ms.warm` are both in the one report.

## 0. Preconditions

1. The sample repo is listed in `config.json` `repos` (with its read-only deploy
   key), and `ue_root`, `archive_root` and `ddc_dir` are set. `ddc_dir` must
   not be under `checkouts_dir`. Raise `unreal_build_timeout_s` above 3600 if a
   first cook is long.
2. Node 22 and Chrome are installed. `<play-host checkout>` is a clone of the
   play-host repo (the driver and the probe page are read from it).
3. ViGEmBus and `ViGEmClient.dll` are installed (`docs/input-runbook.md`).
4. Stop llama-swap the usual way (service, scheduled task or its console), then
   confirm nothing is left holding the GPU:

        tasklist /fi "imagename eq llama-swap.exe"

   It must print `INFO: No tasks are running which match the specified criteria.`
5. Stop the normal play-host so its ports are free, and confirm it is gone:

        tasklist /fi "imagename eq play-host.exe"

6. Keep the display awake and stay at the console. Do not disconnect an RDP
   session: it moves the desktop out of the console session.

## 1. Session check

Start play-host from the console Command Prompt (step 3) and, from a second
Command Prompt while it runs, check both:

        powershell -NoProfile -Command "(Get-Process play-host).SessionId"
        tasklist /v /fi "imagename eq play-host.exe"

The first prints one number; the **Session#** column of the second must show the
same number. **It must not be 0.** Session 0 is non-interactive and cannot
capture; play-host refuses to start a capture there ("capture refused: session
0"). If you see 0, stop, do not continue, and report it.

To check the Command Prompt itself before you start:

        powershell -NoProfile -Command "(Get-Process -Id $PID).SessionId"

## 2. Before-state and Chrome

        nvidia-smi --query-gpu=name,memory.used,memory.total,utilization.gpu,temperature.gpu --format=csv > nvidia-before.txt
        type nvidia-before.txt

Start headless Chrome with a debugging port (leave it running):

        start "" "<chrome.exe>" --headless=new --remote-debugging-port=19223 --user-data-dir=<work dir>\dryrun-chrome about:blank

## 3. Dry run

        <play-host dir>\play-host.exe dryrun --repo <sample repo url> --sha <40-hex commit> --config <config.json> --out report.json --driver <play-host checkout>\spike\driver\run-probes.mjs --cdp 127.0.0.1:19223 --wipe-caches

`--wipe-caches` empties `ddc_dir` and removes this commit's archive, which is
what makes the first run cold. Without it a populated cache fails the run
rather than reporting a warm number as cold. The first build can take most of
an hour; progress is in `<logs_dir>\play-host.log` and the console.

When it finishes, record the after-state and keep the pre-canvas copy:

        nvidia-smi --query-gpu=name,memory.used,memory.total,utilization.gpu,temperature.gpu --format=csv > nvidia-after.txt
        type nvidia-after.txt
        copy report.json report.dryrun.json
        <play-host dir>\play-host.exe dryrun-check report.dryrun.json

Expected for `report.dryrun.json`: `FAIL: canvas_session.session_id invalid`.
Everything before that field passed (the check reports the first failing
field). Any earlier field named instead is a real failure; post it as is.

## 4. Canvas leg (the full loop through Burrow)

1. Start the normal play-host the usual way (the scheduled task, in the
   console session) and repeat the session check in section 1.
2. In the sample's Burrow project, a task is made whose branch changes the pawn
   speed constant (C++) and a colour in `Config/DefaultGame.ini`. It is moved to
   `in_review` and `play_session_start` is called; `play_session_status` is
   followed through `building` (with steps) to `live`. This part is driven by
   the task's agent.
3. When the card says live, open the session in a browser on the LAN and, at the
   same time:
   - hold W, A, S, D one after another: the pawn must move in each direction;
   - move the left stick of a pad (browser Gamepad API, press a button first
     so the browser sees the pad): the pawn must move;
   - click (or tap) the viewport: the pawn must change colour.
   Say what you saw: pawn moves yes/no, pad yes/no, colour changes yes/no, and
   whether the pawn's speed and colour match the task's change.
4. The agent reads `rtt_p50_ms` and `rtt_p95_ms` from the session summary and
   merges them (this is run by whoever has the numbers):

        <play-host dir>\play-host.exe dryrun-canvas --report report.json --session-id <session id> --reached-live --rtt-p50 <ms> --rtt-p95 <ms>
        <play-host dir>\play-host.exe dryrun-check report.json
        echo exit=%ERRORLEVEL%

   `--reached-live` is given only if the card really reached live. It must
   print `OK` and `exit=0`.

## 5. Post

Post to the agents space:

- `report.json` (the checked one) and `report.dryrun.json` (before the canvas leg);
- `nvidia-before.txt` and `nvidia-after.txt`;
- the last 200 log lines, from:

        powershell -NoProfile -Command "Get-Content <logs_dir>\play-host.log -Tail 200"

- the pawn/pad/colour answers from step 4.3.

The report carries the GPU model name only. If you see a machine name in it,
do not post it; say so instead.

## If it fails

- `capture refused: session 0`: wrong session (section 1).
- `git lfs missing on the play host`: install Git LFS.
- `PythonScriptPlugin not enabled`: the sample's `.uproject` is missing the
  plugin (see `docs/dryrun-sample-project.md`).
- `Build failed during <Step>: ...`: the step and last error line; the full log
  is `<logs_dir>\uat-<session id>.log`.
- `game window not found`: the packaged game did not open a window in
  `window_timeout_s`.
- `cold run needs an empty ddc_dir`: rerun with `--wipe-caches`.
- A probe row with `n` under 100: `dryrun-check` fails it. Post the report and
  the log; do not rerun until the cause is known.
