# per_slot: one encoder per joined player slot plus a shared one (01a1a358 stage 4)

This is the cost sheet and the procedure. **The results table below is empty on purpose.**
The numbers are measured on the two LAN play machines (host-a and host-b, as in
`docs/multiplayer-01a0dbd6.md`); nothing here was measured and no figure in the table is
an estimate. `per_slot` stays default off until the table is filled and the acceptance
rule below holds.

## What per_slot changes, and what it does not

Without `per_slot`, one H.264 encoder reads the game's root viewport and every peer gets
the same stream (the 01a0dbd6 arrangement). With `per_slot: true` and a game that
registers a SubViewport per player, the host runs one encoder per joined player slot and
one shared encoder; each player's peer receives its own slot's stream, spectators receive
the shared one.

- **Unchanged: per-peer upstream.** Each peer still receives one stream at 8.54 Mbps. The
  cost moves, it does not appear on the wire.
- **Changed: encoder sessions.** N joined slots plus the shared view are N+1 encoders: 3
  for two seats, 5 for four.
- **Changed: VRAM and NVENC.** Each encoder and its pion set costs memory and NVENC time.
- **Changed: the addon link.** The addon exports one frame per registered view, not one
  per root frame.
- **Not changed:** no second UDP mux, no re-offer on join (a joiner is a new peer on an
  already-running view), no cropping of frames.

## Settings

| key | default | meaning |
|---|---|---|
| `per_slot` | `false` | run one encoder per joined player slot plus a shared one, and declare `game_stream_per_slot` (only together with `game_stream_multi`) |
| `per_slot_bitrate_kbps` | `0` | each slot encoder's bitrate in kbps; 0 = the session preset's |

## The addon contract (summary)

Authoritative text: the "Per-view export" comment block in `addons/burrow_play/burrow_play.gd`.

- A game calls, on the `BurrowPlay` autoload (feature-detect with `has_method`):
  `register_slot_view(slot, viewport)` for slot 0..31, `register_shared_view(viewport)`,
  and `unregister_view(viewport)`.
- With nothing registered the root viewport is exported as `SPF1` (20 byte header), as before.
- With any registration, one `SPF2` record per registered view is exported instead of the
  root: `"SPF2"`, u32 width, height, kind, length, view (little-endian, 24 byte header).
  View 0 is the shared view, `1+slot` is a slot. Kind 2 is the RenderingDevice texture
  export (`texture_get_data_async`).
- The probe marker is drawn into every registered view; the game keeps the top-left
  96x96 px of each view clear. A registered slot's touch is mapped to its SubViewport's
  pixel size.
- A malformed view id is skipped whole and logged; the link stays up.

## Baseline (one encoder, from docs/multiplayer-01a0dbd6.md)

| quantity | one shared encode |
|---|---|
| NVENC utilisation | 2-15% |
| VRAM | 551 MiB Godot alone, 811 MiB with the encoder; flat across peer count |
| per encoder + pion set | about 260 MiB |
| input-to-photon, pooled, LAN, 4 players + 2 spectators | p50 about 41 ms, p95 about 61 ms (1-viewer control p95 61.6 ms) |
| joiner first frame | 1.05-1.54 s |

## Predicted cost (a prediction, not a measurement)

- **Sessions:** N+1 concurrent encode sessions. Two seats plus the shared view is 3; four is 5.
- **VRAM:** about 260 MiB per extra encoder + pion set, so roughly 1.3 GiB over Godot alone at 5.
- **NVENC:** roughly 10-75% at 5 encodes (2-15% per encode scaled by 5; the range is wide
  because the baseline range is).
- **Addon link:** 221 MB/s per 720p RGBA view, so about 1.1 GB/s at four slots plus the
  shared view. This is the cost most likely to bite: measure the link, and if it is the
  limit consider RD texture export (kind 2) or a smaller per-slot view size.
- **NVENC session cap:** consumer drivers cap concurrent NVENC sessions. When a slot
  encoder is refused the host logs `per-slot encoder <n> refused (concurrent encode session
  cap on this driver)`. The cap is a property of the driver, so it is read from the actual
  host:
  1. note the driver version (`nvidia-smi --query-gpu=driver_version --format=csv,noheader`);
  2. with `per_slot: true`, join player slots one by one (and open the shared view) until
     the log line `per-slot encoder <n> refused` appears;
  3. record the driver version and `n` (the cap is `n-1` sessions) in the cap line below.

## The bench tool

```
play-host bench [-config config.json] [-seconds 60] [-driver results.json]
```

Samples once a second for `-seconds` (0 = one sample) and prints one table:

- **encoders:** the number of `ffmpeg` processes that are children of a running
  `play-host` process (process listing: `ps` on Unix, a CIM query on Windows). `n/a` if no
  play-host is running.
- **NVENC % and VRAM MiB:** from `nvidia-smi --query-gpu=utilization.encoder,memory.used
  --format=csv,noheader,nounits -l 1` (one GPU is assumed; with several, the last line
  wins). `n/a (reason)` when nvidia-smi is absent, e.g. a macOS videotoolbox host.
- **with `-driver`:** per-slot input-to-photon p50 and p95, pooled from the raw probe
  samples in the CDP driver's output (`spike/driver/run-probes-multi.mjs --out`), grouped
  by the `slot` each sample carries, plus every viewer's `ttff_ms` (joiner first frame).
- **Not measured by bench:** link MB/s. The host keeps no link byte counter; compute it
  from the frame size in the log (`width x height x 4 x fps` per view) and confirm with
  the OS's own counters during the run, and say which in the table.

## Procedure (host-a for the host, host-b for the viewers, as in 01a0dbd6)

1. Run the dev play-host, never prod; check prod is idle before and after as 01a0dbd6 did.
2. Set `per_slot: true` in the dev `config.json` (optionally `per_slot_bitrate_kbps`) and
   start `play-host serve -config <config.json>`.
3. Use a game that registers per-slot SubViewports. Start a session with the multi feature.
4. Join two players, then a spectator (the 2-slot row). Repeat later with four players plus a
   spectator (the 4-slot row), a fresh session each.
5. Run the driver from host-b with the CDP browser, for example
   `node spike/driver/run-probes-multi.mjs --cdp 127.0.0.1:9223 --app <core url> --canvas
   <canvas uuid> --players 2 --spectators 1 --n 50 --jitter-ms 34 --label ps2 --out
   results/ps2.json`.
6. Run `play-host bench -config <config.json> -seconds 60` on host-a while the driver is
   probing (the cost columns), then `play-host bench -config <config.json> -seconds 0
   -driver results/ps2.json` once the driver has written its file (the per-slot latency and
   joiner first frame columns).
7. Run the 1-viewer control with `per_slot` off the same day. Paste each bench output into
   the table below, with the driver version, date and commit.

## Results (EMPTY: to be filled on host-a / host-b)

NVENC concurrent session cap on the host driver: driver version `610.47` (host-a, read
2026-11-03), cap `____` (first refusal at encoder `____`: not reached yet, since no
per-slot session has run on this driver).

| row | encoder count | NVENC % | VRAM MiB | link MB/s | per-slot p50 ms | per-slot p95 ms | joiner first frame ms |
|---|---|---|---|---|---|---|---|
| idle, per_slot on, no session (host-a, 60 s) | 0 / 0 / 0 | 0 / 0 / 0 | 3244 / 3244 / 3244 | n/a (no session) | n/a | n/a | n/a |
| idle, per_slot on, no session (host-b, 5 s) | 0 / 0 / 0 | n/a (no nvidia-smi; videotoolbox) | n/a | n/a (no session) | n/a | n/a | n/a |
| control, per_slot off, 1 viewer | | | | | | | |
| 2-slot + 1 spectator | | | | | | | |
| 4-slot + 1 spectator | | | | | | | |

Idle rows (2026-11-03, commit b27021b, both hosts running `serve` with `per_slot: true`
and reconnected to the install, which listed `game_stream_per_slot` for each): the cells are
`play-host bench -seconds N` summary min / median / max. They are the host at rest, with no
session and therefore no encoders. They are **not** the control, 2-slot or 4-slot rows. Those
rows need a live session with joined players and the CDP driver (steps 3-6), and are still
empty. The 3244 MiB on host-a is VRAM held by other GPU users on that machine; it is not
play-host's.

## Acceptance rule

Per-slot p95 must not regress beyond the existing budget: the pooled p95 of the 1-viewer
control, 61.6 ms in 01a0dbd6, same rule and same statistics as that doc. The 4-slot row must
also fit inside the concurrent NVENC session cap. `per_slot` stays default off until this
table is filled and the rule holds for both rows.
