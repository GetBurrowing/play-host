# Unreal build runner

A checkout with exactly one `*.uproject` at its root is an Unreal build. The
host does the following instead of the Godot import:

1. `git lfs version` (the session fails with `git lfs missing on the play host`
   if it is absent), then `git lfs install --local` and `git lfs pull`.
2. If `<archive_root>/<repo>/<sha40>/Windows/<Project>.exe` exists with a
   `.complete` marker, the build is reused ("Reusing build <sha12>").
3. Otherwise RunUAT `BuildCookRun` runs under the session's Job Object into
   `<sha40>.partial`, which is renamed only on success.
4. The packaged game is launched with
   `-windowed -ResX=<W> -ResY=<H> -WinX=0 -WinY=0 -nosplash -log`; capture and
   input attach to it as in the capture and input runbooks.

The building status detail starts with the step word: Preparing, Compiling,
Cooking, Staging, Packaging or Archiving. UAT reports no percentage, so none is
shown. A failure reads `Build failed during <Step>: <last error line>`; a
timeout reads `Build timed out during <Step>`.

## Config keys

All paths are placeholders here; real values live in the machine's
`config.json`.

- `ue_root`: the engine install root (`<ue_root>\Engine\Build\BatchFiles\RunUAT.bat`).
- `archive_root`: where packaged builds are kept, keyed by commit.
- `ddc_dir`: a persistent derived-data cache, passed as `UE-LocalDataCachePath`.
  It must NOT be under `checkouts_dir`: checkouts are deleted after every
  session, and the host refuses to start if `ddc_dir` is inside it.
- `ddc_shared_dir`: optional, passed as `UE-SharedDataCachePath`.
- `unreal_build_timeout_s`: one build's limit (default 3600). A first cook of a
  large project can take longer; raise it for that.
