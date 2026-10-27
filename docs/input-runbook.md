# Input runbook (engines without an addon)

Unreal sessions have no in-engine input receiver. The host injects input into
the logged-in desktop session instead:

- Keyboard and mouse use user32 `SendInput` (scancodes; absolute mouse mapped
  onto the virtual desktop). Windows merges all `SendInput` into one keyboard
  and one mouse, so keyboard, mouse and touch-to-mouse belong to **slot 0
  only**. Key and touch input from any other slot is dropped and counted
  `kbm_slot`.
- Touch id 0 on slot 0 drives the mouse (down = move and left press, move =
  move, up or cancel = left release), scaled to the captured window's client
  area. Other touch ids are ignored.
- Pads: one virtual Xbox 360 controller per slot (slots 0 to 3, the XInput
  limit), through ViGEmBus and `ViGEmClient.dll`. A target is created on the
  slot's first pad input and removed when the slot is released or its peer
  closes.
- A real desktop mouse (pointer lock, mouselook) needs a new browser message,
  which is a protocol change and not part of this build.

Input is refused in session 0 and in any session that is not the active
console session, exactly like capture. Input is only injected while the
foreground window belongs to the game; otherwise it is dropped and counted
`not_foreground`. If ViGEmBus or the DLL is missing, pads are disabled with one
log line and counted `pad_unavailable`; keyboard and mouse keep working.

At the end of a session the log has one line:
`input injection: kbm_slot=<n> not_foreground=<n> pad_unavailable=<n>`.

## Probe

The latency probe uses the same code and the same browser script as the
Godot addon. With no addon to draw it, play-host draws the 4x4 cell code (16 px
cells, a 64x64 corner) for two frames in a click-through topmost layered
window over the game's client top-left, and ddagrab captures it. This figure
is labelled **host_to_photon**: it runs from the host receiving the probe to
the photon, and it excludes the game's own input-to-render time, which the
Godot figure included. The two are never compared without that label.

## Install on the play machine

Run these in the logged-in desktop session, in a normal (non-elevated)
Command Prompt unless a step says otherwise. Paths are placeholders:
`<play-host dir>` is the folder holding `play-host.exe`, `<work dir>` is any
scratch folder.

1. Check ViGEmBus. If the state is `RUNNING`, skip to step 3.

        sc query ViGEmBus

2. ViGEmBus is retired upstream; its last release is v1.22.0. Download the
   installer, run it, accept the prompts, and reboot if it asks:

        curl.exe -L -o <work dir>\ViGEmBus_1.22.0_x64_x86_arm64.exe https://github.com/nefarius/ViGEmBus/releases/download/v1.22.0/ViGEmBus_1.22.0_x64_x86_arm64.exe
        <work dir>\ViGEmBus_1.22.0_x64_x86_arm64.exe
        sc query ViGEmBus

3. ViGEmClient publishes no binary release, so build the DLL from source. The
   repository is retired too; build the pinned last commit. Use the
   "x64 Native Tools Command Prompt for VS 2022" (it has `cmake` and `git`):

        git clone https://github.com/nefarius/ViGEmClient.git <work dir>\ViGEmClient
        cd /d <work dir>\ViGEmClient
        git checkout b66d02d57e32cc8595369c53418b843e958649b4
        cmake -S . -B build -G "Visual Studio 17 2022" -A x64 -DViGEmClient_DLL=ON
        cmake --build build --config Release --target ViGEmClientShared
        dir build\Release\ViGEmClient.dll

4. Put the x64 DLL next to the executable:

        copy build\Release\ViGEmClient.dll <play-host dir>\ViGEmClient.dll

5. Restart play-host in the desktop session. Start an Unreal session, connect
   a browser with a pad, press a pad button, then open Game Controllers and
   confirm a "Controller (XBOX 360 For Windows)" appears and its buttons react:

        joy.cpl

6. Paste the last `input injection:` line of the play-host log into the agents
   space. If it shows `pad_unavailable` above 0, paste the log line starting
   `virtual pads disabled`.

The upstream URLs above (ViGEmBus v1.22.0 release asset, the ViGEmClient
repository and its `ViGEmClient_DLL` CMake option) were checked against the
upstream repositories on the day this was written. The DLL build itself has
not been run by the author.
