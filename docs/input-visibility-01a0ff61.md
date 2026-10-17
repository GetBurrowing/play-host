# Per-peer input visibility (task 01a0ff61, stage 1)

Before this change, the host log could not tell three situations apart: the
browser sent nothing, the browser sent messages that did not parse, and the
peer was a spectator whose input was dropped. `onInput` returned silently
when `Decode` failed. No line said a data channel had opened. `SetSlots`
logged nothing. The drop line kept only the last reason.

Five INFO lines now answer those questions. Each goes through `s.logf`, so
it carries the `[<session>]` prefix. **None of them contains a value from a
message.** They log only key names (bounded and sanitised), sizes, counts,
our own type names and our own Go field names.

## The lines

The examples are taken from the test log. `<peer>` is core's peer_id, or `v1`
for the implicit peer of an old core.

| Line (stable substring in bold) | Example | Question it answers |
|---|---|---|
| **`peer <peer>: data channel <label> open, slots <table>`** | `[t1] peer s: data channel input-events open, slots []` | Did the browser's channel come up? Was it a spectator? `[]` means a spectator. `[src0:1]` means src 0 drives slot 1. A peer with no open line never got a working channel. |
| **`peer <peer>: slot <S> bound (src <N>)`** / **`unbound`** | `[t1] peer p: slot 1 bound (src 0)` | When did the peer gain or lose a slot? This line is logged only on an actual change: reconcile calls `SetSlots` for every peer on every play_slots, and an unchanged table logs nothing. |
| **`peer <peer>: first <type> message on <label>`**, then `slot <S>` or `no slot (<reason>)` | `[t1] peer s: first touch message on input-events, no slot (its src has no entry in the slot table)` | Did a message of this type arrive at all? It is logged before the drop branch, so a spectator's first touch shows up too, without a slot. The type is one of a fixed set: key, pad, probe, touch, release_all, other. That caps it at 6 lines per peer. A player's first message that was still dropped for another reason reads `slot 1, dropped (bad touch)`. |
| **`peer <peer>: unparseable input message on <label> (<size> bytes, keys <keys>, <errclass>); later ones are counted`** | `[t1] peer p: unparseable input message on input-events (28 bytes, keys not-json, syntax@2); later ones are counted` | Did the browser send something the host could not decode? This is the first such message only, and it is logged immediately so a live session shows it. `<keys>` is `not-json`, `not-an-object`, or `[a,t,x]`. `<errclass>` is `syntax@<offset>`, `type:<Go field>` or `other`, and is never the decoder's error text (see below). |
| **`peer <peer>: input summary key=… pad=… probe=… touch=… release_all=… other=… unparseable=… drops no_slot=… probe_no_space=… bad_touch=… touch_rate=…`** and, when unparseable>0, ` first_unparseable=<size>B keys <keys>` | `[t1] peer p: input summary key=0 pad=0 probe=0 touch=33 release_all=0 other=0 unparseable=2 drops no_slot=0 probe_no_space=0 bad_touch=1 touch_rate=30 first_unparseable=28B keys not-json` | What did the peer send over its lifetime, and why was each drop dropped? It is printed once per peerState, from closePeer and from session cleanup, guarded by an atomic. |

Reading the three cases:
- **Nothing sent**: the open lines are present, but there is no `first … message` line and the summary is all zeros.
- **Sent but unparseable**: an `unparseable input message` line appears, and the summary shows `unparseable=N`.
- **Spectator**: the open line shows `slots []`, there are no `bound` lines, the first message reads `no slot (…)`, and the summary counts the messages under `no_slot`.

The existing throttled `dropped N input message(s), M in total: <last reason>`
line is unchanged.

## What is bounded and why it is safe

- **Per-peer state is fixed-size and atomic.** It consists of one counter and one first-seen `atomic.Bool` per type, held in arrays sized by `input.MsgTypes`. It adds one counter per drop reason, `badParse`, one `atomic.Pointer` to the first bad message's shape (stored once), and a `summarized` bool. Any unknown `t` collapses into `other`, so a hostile page cannot grow a map. The pion goroutines for the two channels write the state concurrently, and `go test -race` passes on it.
- **The error class is not the error text.** A `*json.SyntaxError`'s message quotes a byte of the input (`invalid character 'o' …`), so it is never logged; only `syntax@<offset>` is. For a `*json.UnmarshalTypeError`, the JSON path is mapped to the `Browser` field it names (`type:X`), or `?` when it names no field. The field names therefore come from our struct, never from the browser.
- **`input.Shape` returns key names only.** It lists at most 16 keys (then `+N more`) and cuts each to 32 bytes. Each byte outside printable ASCII becomes `?`, and so does each space, comma and square bracket. Stage 1 added that last rule beyond the plan: it stops a key from forging the list syntax or a `] viewer…` fragment.
- **The host-a idle check.** None of the new lines can match `\] (status |session ended|offer|viewer)` (prodcheck-01a0fe45.ps1). Every new line has `peer ` right after the `] ` prefix. The parts a browser influences are the key names in `<keys>`, and they cannot contain `]` or a space. The session-level tests check every captured new line against that regex, together with a positive control.
- **`Result.Type` is new in this stage.** It did not exist on main. `Binding.Handle` now sets it to `MsgType(m.T)` for every message that decoded. `Handle` was split into `Handle` (decode, then set Type) and `handle` (the unchanged switch).
- **The `SetSlots` signature changed.** It now returns `(lines []Out, bound, unbound []SlotChange)`, sorted by src then slot. A pair whose slot or src changed appears as one unbind plus one bind. v1 returns no changes. Every caller was updated: openPeer, reconcile, binding_test.go and input_test.go.

## Tests

- `internal/host/host_visibility_01a0ff61_test.go`, on ports 40420 and 40422 (mux ports 31420 and 31422):
  - `TestInputVisibilityPlayerAndSpectator` checks:
    - both open lines for p and s, with `slots []` for s;
    - exactly one `slot 1 bound (src 0)` for p and no slot line at all for s;
    - p's first touch on `slot 1` and s's first touch on `no slot`;
    - the summaries `touch=1` for p and `touch=1 no_slot=1` for s;
    - still one summary per peer after the session ends, because cleanup runs too.
  - `TestInputVisibilityCountsAndUnparseable` sends two touch downs, a bad touch (id 42) and 30 back-to-back moves, then checks:
    - exactly one first-touch line;
    - `bad_touch=1` and `touch_rate` in 1..30;
    - two unparseable messages carrying `SENTINEL-01a0ff61-A` and `-B` give `unparseable=2`, one unparseable line showing `28 bytes, keys not-json`, and `first_unparseable=28B keys not-json`;
    - no captured line contains `SENTINEL`;
    - a slot change from `{"0":1}` to `{}` logs `slot 1 unbound (src 0)`.
- `internal/input`:
  - `TestSetSlotsReportsChanges` covers unchanged, added, src-moved-slot, slot-moved-src, negative entries, spectator, and v1.
  - `TestShapeNeverCarriesAValue` covers not-json, empty, array, string, number, object, empty object, 19 keys, a 40-byte key, and a non-printable key.
  - `TestMsgTypeIsAFixedSet` and `TestErrClassNeverQuotesTheInput` cover the type mapping and the error class.
- The new host tests plus the existing spectator-touch and LogTouch tests passed 10 times in a row under `-race` (`-count=10`).

## Evidence

The log files named below were the raw run output; they are not kept in this repository, and these lines are their summary.

- `go-checks-01a0ff61-2927941.log`: `gofmt -l .` printed nothing, `go vet ./...` and `GOOS=windows go vet ./...` both exited 0, and `go test -count=1 ./...` passed in every package. These checks ran on host-d; this stage is Go, not mix. They ran on the working tree that was then committed. The commit added only this doc and the logs, with no code change.
- `mutation-m1-01a0ff61-2929890.log`: **M1** replaced `n := ps.badParse.Add(1)` with `ps.badParse.Load() + 1`, which deletes the increment. `TestInputVisibilityCountsAndUnparseable` then failed with `host_visibility_01a0ff61_test.go:184: timed out: 2 unparseable`. host.go was restored by cp-back and is byte-identical to the original, sha256 `289dfcec3b7e44c844e52896fa35b925b438d70e79d0f9618875b2b39a0a3fc0`.
- `mutation-m2-01a0ff61-2930301.log`: **M2** replaced the first-seen `CompareAndSwap` guard with an unconditional `Store`. The same test then failed with `host_visibility_01a0ff61_test.go:178: first-touch lines = 33 after 33 touches, want exactly 1`. The restore is byte-identical, with the same sha256.

## Host-a swap (stage 2)

Prod runs the **unmerged branch tip**. If a later merge changes the tree, the deployer must swap again.

- **Built from** play-host `d32cb808a08719000187845c1e45ddd873fac4b1`, tree `d5e42584b5e9a8b3e7cf0997a4541e71ad8728d4` (`git ls-remote` returned the same tip). It was cross-compiled on host-d with `GOOS=windows GOARCH=amd64 go build -ldflags "-X main.Version=<sha>" -o play-host-01a0ff61.exe ./cmd/play-host`.
- **New exe** sha256 `DBA4517260B9E1C0EACD18247E9A0A9F33BB01983F5DE81C470467060AE4BC7C`.
- **Old exe** (0664fa0, the 01a0fe45 build) sha256 `9DE64D7A63920846648B12695A9339377799BA7D170021BEC08FC72A22A852AE`.
- **Backup:** `C:\Users\operator\play-host\bin\play-host.exe.bak-01a0ff61-20261003T020955Z`, sha256 equal to the old exe. A second copy with the same hash is in `.bak-01a0ff61-20261003T020933Z` (see the first attempt below).
- **Addon:** this task does not ship it. `addons/suite_play/suite_play.gd` on host-a read `EBAF4A793C50801FAFED752D9DE2957D1AF346B4DFF668E7B9050C8F21AA2467` before and after the swap, which equals the branch copy.
- Prod `config.json` was not touched.

### Idle check (both halves, read-only)

- **(a) host-a.** `prodcheck-01a0fe45.ps1` read `PROD_BUSY=False children=0 prod_udp_binds=0`. The last session line was `[01a0ff66-…] session ended: Ended by Operator` (01:41:32Z).
  - **Positive control:** `prodcheck-control-01a0ff61-*.ps1` runs the same check on a scratch directory whose log ends in an `offer` line followed by one of this task's new `peer …: data channel … open` lines. It read `PROD_BUSY=True`. After a `session ended` line was appended, it read `False`. So the check fires, and the new lines do not mask a live session.
- **(b) core.** host-a's `suite_url` is `wss://burrow.example.com/runtime/ws`. On host-b, the prod `core-platform` container has `PHX_HOST=burrow.example.com`; a second core container there is a different install. `corecheck-01a0ff61-*.exs` was run with `bin/platform rpc`. It reads presence, the Registry and the DynamicSupervisor count, with no writes. Results: play-host-a present, `session_for_host` nil, 0 live sessions. The last `game_stream_sessions` row for play-host-a is 01a0ff66, `ended`.
  - Not shown: the core half was never seen returning a non-nil session, because no session was live to read.

### Features before and after

| | features | client version |
|---|---|---|
| before | `["game_stream_host", "game_stream_multi", "game_stream_touch"]` | `0664fa03fb0179ab761069c90af03aaf65dc18e8` |
| after | `["game_stream_host", "game_stream_multi", "game_stream_touch"]` | `d32cb808a08719000187845c1e45ddd873fac4b1` |

### Confirm

- The running prod process (pid 12860, `C:\Users\operator\play-host\bin\play-host.exe`) has image sha256 `DBA45172…AE4BC7C`, equal to the built exe.
- The log shows `play-host d32cb808a08719000187845c1e45ddd873fac4b1 serve: runtime play-host-a via wss://burrow.example.com/runtime/ws`, then `host: joined; ready for play_session_start`.
- prodcheck afterwards read `PROD_BUSY=False`.

### First attempt (20261003T020933Z) aborted mid-swap; corrected

`deploy-01a0ff61-20261003T020933Z.ps1` had two PowerShell bugs:
- **Variable name clash.** PowerShell variable names are case-insensitive, so `$staged = <hash>` overwrote the path in `$Staged`. As a result, `Copy-Item` failed after prod was already stopped.
- **CIM count.** `(ProdPids).Count` is `$null` for one CIM instance in PowerShell 5.1, so the stop-wait loop exited at once. The follow-up `Stop-Process` on the exact prod image path ended the process instead.

The old exe was never overwritten. The task's restart policy brought the old build back at 02:09:39Z, about 5 s down, with no session live. The corrected script (`@(ProdPids).Count`, `$stagedHash`) ran at 20261003T020955Z and reached `DEPLOY_OK`. Both scripts and both transcripts are committed.

### Rollback

On host-a, `powershell -NoProfile -ExecutionPolicy Bypass -File C:\Users\operator\play-host-01a0ff61-scratch\rollback-01a0ff61.ps1 -Stamp 20261003T020955Z`. A copy of the script is committed as `rollback-01a0ff61-20261003T020955Z.ps1`.
- It stops `suite-play-host`, copies `.bak-01a0ff61-20261003T020955Z` back and starts the task.
- Afterwards, check that the image hash reads `9DE64D7A…852AE`, and that core lists the same three features with version 0664fa0.
