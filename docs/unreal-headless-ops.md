# Headless editor ops for Unreal projects

Nobody opens the Unreal editor on the play machine. Agents change a project
through the repository, and the host builds it. This page says which
operations need the editor, and how an optional manifest runs the ones that do.

An asset generated before cook exists in the BUILD, not in the repository. It
is reproducible from the committed script. Committing generated assets back
needs a write key and is out of scope.

## Which operations need the editor

| Operation | Path | Why |
|---|---|---|
| C++ under `Source/`, `*.Build.cs`, `*.Target.cs` | Repo text only | Compiled by RunUAT; no editor session |
| `Config/*.ini`, the `.uproject` plugin list, CSV and JSON data | Repo text only | Read at cook or run time |
| Create or modify Blueprint assets (Python `unreal` API) | Editor, headless (`-nullrhi`) | Assets are binary and only the editor writes them |
| Import FBX, PNG, WAV (AssetTools, Interchange) | Editor, headless | Import produces `.uasset` files |
| DataTable assets from CSV | Editor, headless | The table is an asset |
| Material instances | Editor, headless | An asset created from a parent material |
| Place level actors | Editor, headless | The level is an asset |
| `-run=ResavePackages -fixupredirects` | Editor, headless | Rewrites packages |
| `-run=CompileAllBlueprints` | Editor, headless | A check; fails the build on a broken Blueprint |
| GPU Lightmass baked lighting, thumbnails, some previews | Needs a real RHI, not `-nullrhi` | Avoid; use Lumen dynamic lighting |
| Hand editing a Blueprint graph | Not headless at all | Agents write gameplay in C++ and keep Blueprints as thin data subclasses made by script |

## The manifest

An optional `burrow-play.json` at the project root (next to the `.uproject`):

    {"editor_scripts": ["Scripts/make_material.py"],
     "commandlets": [{"run": "CompileAllBlueprints"}]}

- It runs after the Git LFS pull and before RunUAT, under the session's Job
  Object, one editor process per entry: scripts first, then commandlets, in
  order. Each is `UnrealEditor-Cmd.exe <project> -run=<commandlet> -nullrhi
  -unattended -nop4 -nosplash -stdout -FullStdOutLogOutput`. Scripts run with
  `-run=pythonscript -script=<absolute path>`.
- The building status reads `Editor scripts`. A failure reads
  `Build failed during Editor scripts: <last error line>`.
- Commandlets are allowlisted: `pythonscript`, `CompileAllBlueprints`,
  `ResavePackages`, `DataValidation`. Anything else fails with
  `commandlet not allowed: <name>`. A `pythonscript` entry names its own
  `"script"`.
- Script paths must be relative and stay inside the project. `..`, absolute
  paths, drive letters and symlinks that leave the project are refused.
- If any script runs, the `.uproject` must enable PythonScriptPlugin, or the
  build fails with `PythonScriptPlugin not enabled`.
- A key the manifest does not know (a misspelt `editor_script`) is an error, so
  a script never silently fails to run.
- With no manifest, or an empty one, no editor process runs at all.
- The editor is never started in session 0, or in a session that is not the
  active console session.
- A reused archive (same commit) skips the manifest, since the build already
  holds its generated assets.
