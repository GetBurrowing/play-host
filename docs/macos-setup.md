# macOS play-host setup

This installs play-host as a per-user LaunchAgent on a logged-in macOS development machine. The agent runs only in an Aqua login session, starts at login, and is restarted by launchd after an unexpected exit.

## Prerequisites

Install Go 1.25, Godot 4, and Homebrew FFmpeg:

```sh
brew install ffmpeg git
/opt/homebrew/bin/ffmpeg -hide_banner -encoders | grep -E 'h264_videotoolbox|libx264'
```

Homebrew's FFmpeg uses Apple's VideoToolbox framework without a separate formula option. Both `h264_videotoolbox` and `libx264` must appear above. Install the Godot macOS app in `/Applications/Godot.app`; play-host expands that bundle path to `/Applications/Godot.app/Contents/MacOS/Godot`.

Build and stage the host:

```sh
cd /path/to/play-host
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o play-host ./cmd/play-host
sudo install -d -o "$USER" -g staff /Users/Shared/play-host/{bin,logs,mirrors,checkouts,burrow_play,secrets}
install -m 0755 play-host /Users/Shared/play-host/bin/play-host
```

On Apple Silicon, omit `GOARCH=amd64` for a native arm64 binary. See [build.md](build.md) for the complete verification matrix.

## Configuration

Save this as `/Users/Shared/play-host/config.json` and put only the runtime token in `/Users/Shared/play-host/secrets/runtime-token` (mode 0600):

```json
{
  "burrow_url": "wss://burrow.example/runtime/ws",
  "runtime_id": "play-host-mac",
  "token_file": "/Users/Shared/play-host/secrets/runtime-token",
  "godot": "/Applications/Godot.app",
  "rendering_driver": "",
  "ffmpeg": "/opt/homebrew/bin/ffmpeg",
  "encoder": "videotoolbox",
  "encoder_fallback": true,
  "git": "/opt/homebrew/bin/git",
  "ssh": "/usr/bin/ssh",
  "known_hosts": "/Users/Shared/play-host/known_hosts",
  "mirrors_dir": "/Users/Shared/play-host/mirrors",
  "checkouts_dir": "/Users/Shared/play-host/checkouts",
  "addon_dir": "/Users/Shared/play-host/burrow_play",
  "logs_dir": "/Users/Shared/play-host/logs",
  "host_ip": "",
  "udp_min": 40300,
  "udp_max": 40309,
  "godot_port": 40320,
  "rtp_port": 40330,
  "audio_rtp_port": 40331,
  "import_timeout_s": 1200,
  "keep_checkouts": 3,
  "heartbeat_s": 10,
  "launch_timeout_s": 90,
  "progress_every_ms": 5000,
  "repos": [
    {"match": "github.com/example/game", "url": "git@github.com:example/game.git"}
  ]
}
```

- `encoder` accepts `auto`, `nvenc`, `videotoolbox`, or `libx264`. `auto` selects VideoToolbox on Darwin, NVENC on Windows, and libx264 elsewhere.
- `encoder_fallback: true` probes the selected encoder when streaming begins and uses libx264 only if initialization fails.
- Keep `rendering_driver` empty on macOS so Godot chooses its native default. Windows continues to default to Vulkan.
- Paths are native argv values. Do not add shell quoting inside JSON.
- `burrow_url` was called `suite_url` before the Burrow rename. A config with only `suite_url` still loads, with one deprecation line in the log; when both are set, `burrow_url` wins.
- `addon_dir` holds `burrow_play.gd` (copy `addons/burrow_play` from this repository). A directory that still holds the older `suite_play.gd` keeps working and is installed under its old name.

## LaunchAgent

Save the following as `~/Library/LaunchAgents/com.startupsuite.play-host.plist`. Create the log directory before bootstrap; launchd does not create parent directories for log files.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.startupsuite.play-host</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/Shared/play-host/bin/play-host</string>
    <string>serve</string>
    <string>-config</string>
    <string>/Users/Shared/play-host/config.json</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>
  <key>StandardOutPath</key>
  <string>/Users/Shared/play-host/logs/launchd.stdout.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/Shared/play-host/logs/launchd.stderr.log</string>
</dict>
</plist>
```

Validate, install, restart, inspect, and uninstall with these exact commands:

```sh
mkdir -p "$HOME/Library/LaunchAgents" /Users/Shared/play-host/logs
plutil -lint "$HOME/Library/LaunchAgents/com.startupsuite.play-host.plist"
launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.startupsuite.play-host.plist" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.startupsuite.play-host.plist"
launchctl enable "gui/$(id -u)/com.startupsuite.play-host"
launchctl kickstart -k "gui/$(id -u)/com.startupsuite.play-host"
launchctl print "gui/$(id -u)/com.startupsuite.play-host"

# uninstall
launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.startupsuite.play-host.plist"
```

A LaunchAgent unload sends SIGTERM to its leader but is not, by itself, a child-process ownership guarantee. play-host handles SIGTERM, launches each game in an owned process group, and kills that group on session stop. A pipe watchdog kills the captured group if play-host itself dies before cleanup (including SIGKILL). After `Wait`, play-host retires the pgid so a reused identifier can never be signalled. The owned process group—not launchd—is the source of truth for Godot and any supervisor/FFmpeg descendants.

After a normal stop or a crash/restart exercise, verify that no tagged game or host-owned FFmpeg remains:

```sh
pgrep -af 'suite-play-session|/Users/Shared/play-host/bin/play-host|[f]fmpeg' || true
```

Interpret FFmpeg matches carefully if the machine runs unrelated encoders; compare the PID/group snapshot in `play-host.log` rather than killing by name.

## Firewall and ports

Allow the play-host binary through the macOS application firewall when it is enabled:

```sh
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add /Users/Shared/play-host/bin/play-host
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp /Users/Shared/play-host/bin/play-host
```

Permit inbound UDP `40300-40309` on any host/network firewall for the configured ICE range. `40320`, `40330`, and `40331` are loopback-only Godot/video/audio links by default and should not be exposed. TURN may still be needed outside the LAN.
