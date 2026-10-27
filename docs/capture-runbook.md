# Capture runbook (engines without an in-engine readback)

Godot sessions get frames from the addon over the local link. Any other engine
(Unreal) is captured from the desktop: the engine is chosen per build from the
checkout (`project.godot` selects Godot, exactly one `*.uproject` selects
Unreal), and the host takes video from a capture Source.

Capture needs the logged-in desktop session. It is refused in session 0 and in
any session that is not the active console session. Run the host from the
logged-in desktop, not from a scheduled task or service.

## Config keys

- `capture_backend`: `ddagrab` (default) or `wgc`. `ddagrab` is ffmpeg-native:
  the GPU grabs the window's client area and D3D11 frames go straight to
  `h264_nvenc`. It needs the NVENC encoder. `wgc` is Windows.Graphics.Capture;
  the host pushes frames to ffmpeg's stdin.
- `window_timeout_s`: how long to wait for the game window after launch
  (default 120). On expiry the session fails with `game window not found`.
- `frame_stall_timeout_s`: how long a viewer may see no captured frame before
  the session fails with `capture stalled: no frames (display asleep?)`
  (default 10).

The monitor is chosen from the game window's position, not assumed to be
output 0. Virtual display adapters add extra DXGI outputs, so the output index
is the one holding the window. The host keeps the display awake while a
capture session runs (SetThreadExecutionState); it never changes `powercfg`.

## Checks to run on the play machine

Run these in the logged-in desktop session, with the display awake, and paste
the output.

    ffmpeg -hide_banner -version
    ffmpeg -hide_banner -filters | findstr ddagrab
    ffmpeg -hide_banner -encoders | findstr nvenc
    powershell -Command "(Get-Process explorer).SessionId"
    ffmpeg -hide_banner -f lavfi -i ddagrab=output_idx=0:framerate=60:draw_mouse=0 -t 5 -c:v h264_nvenc -preset p1 -tune ll -f null -
    echo exit=%ERRORLEVEL%
    sc query ViGEmBus
    git lfs version

The session check uses `Get-Process explorer` because `query session` does not
exist on Windows Home (it is Pro-only). Explorer's SessionId is the desktop
session and must not be 0.

If the 5 s ddagrab run never exits and logs a "dxgi output" line but no frames,
the display was asleep: Desktop Duplication emits nothing without
composition. Wake the display and rerun.

## Backend decision

Decided: `ddagrab` is the default. The play-machine probe listed the `ddagrab`
filter, and with the display held awake the 5 s run encoded 297 frames at 57 fps
and ffmpeg reported "Exiting with exit code 0". `wgc` stays a recognised value
but is not built: it returns "not built into this binary".

Two consequences of that probe:

- Keep-awake is required. With the display asleep, Desktop Duplication emitted
  no frames and the run never exited. The host holds the display awake for the
  life of a capture session, and the stall watchdog fails a session that still
  gets no frames.
- Encode the cropped game window, never the full desktop. Capturing the whole
  4K desktop ran just under realtime (0.963x). The host crops to the window's
  client area.
- ddagrab hands D3D11 frames straight to NVENC, so it needs an NVENC encoder.
  A session whose encoder resolves to libx264 fails with a clear error. Placeholders in config: `<ffmpeg path>`, `<checkouts dir>`,
`<logs dir>`.
