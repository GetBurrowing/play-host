# play-host

Windows and macOS host that turns a Godot build into a WebRTC stream for Burrow's
`game_stream` canvas (task 01a0db5f). Go, `CGO_ENABLED=0`; see
[`docs/build.md`](docs/build.md) for the Darwin/Windows build matrix and
[`docs/macos-setup.md`](docs/macos-setup.md) for LaunchAgent installation.

Encoding uses FFmpeg with NVENC on Windows, VideoToolbox on macOS, and an
optional libx264 fallback; WebRTC is pion v4.

Layout:

- `cmd/play-host` - the binary (stage 1: `play-host spike`).
- `internal/media` - ffmpeg/NVENC argument building, RTP and Annex-B forwarding.
- `internal/rtc` - pion peer: sendonly video, `input-state` + `input-events` channels.
- `internal/probe` - input-to-photon probe code (shared with the addon and the page).
- `internal/launch` - Windows Job Object / Unix process-group launcher; Stop kills only what the host started.
- `internal/spike` - stage 1 harness and the browser probe page.
- `addons/burrow_play` - the Godot autoload (probe marker, frame export; input in stage 3).
  It reads `--burrow-play-*` user args and the older `--suite-play-*` ones; the host emits both.
- `spike/godot` - throwaway scene; `spike/driver` - CDP latency driver.
- `docs/spike-01a0db5f.md` - stage 1 findings.

Input (`internal/input`): browser `key`, `pad`, `probe`, `release_all` and,
since task 01a0fe45, `touch` messages become addon lines. A touch
`{"t":"touch","src":0,"id":0..9,"ph":"down|move|up|cancel","x":0..1,"y":0..1}`
becomes `{"t":"st",...}` (InputEventScreenTouch) or `{"t":"sd",...}`
(InputEventScreenDrag) with Godot index `i = slot*10 + id`. Bad fields are
dropped as `bad touch`, a move under 4 ms after the last is dropped as
`touch rate`, and a spectator's touch as `no slot`. The host declares
`game_stream_touch`; core's `Platform.GameStream.Protocol` "Touch" is the
contract.

Config: `config.json` names core's runtime socket as `burrow_url`. A config that still has
only the older `suite_url` key loads unchanged (one deprecation line in the log); when both are
set, `burrow_url` wins. An `addon_dir` that still holds the older `suite_play.gd` is installed
under its old name.

Scrub: `scripts/scrub-check.sh` fails on any tracked file that names a particular install,
person or private host (its denylist is stored hashed).

Tests: `go test ./...`, `GOOS=darwin GOARCH=amd64 go vet ./...`, and `GOOS=windows GOARCH=amd64 go vet ./...`.
