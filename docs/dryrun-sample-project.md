# Sample Unreal project for the dry run

Hand this to whoever creates the sample in the sample repo. The sample lives in
its own repo, never in play-host. It is deliberately tiny: the point is to
measure the loop (clone, LFS, editor script, cook, package, launch, capture,
input, probe), not the game.

## Requirements

1. **Engine:** the UE version named for the play machine (`ue_root` in its
   `config.json`), created from **Games > Blank > C++** (no starter content),
   project name `Sample`.
2. **Git LFS** on from the first commit. `.gitattributes`:

        *.uasset filter=lfs diff=lfs merge=lfs -text
        *.umap   filter=lfs diff=lfs merge=lfs -text
        *.png    filter=lfs diff=lfs merge=lfs -text
        *.wav    filter=lfs diff=lfs merge=lfs -text

   Keep `Binaries/`, `Intermediate/`, `Saved/` and `DerivedDataCache/` in
   `.gitignore`. The checkout is read with a read-only deploy key.
3. **One map**, `Content/Maps/Main`, set as `GameDefaultMap` and
   `EditorStartupMap` in `Config/DefaultEngine.ini`: a flat floor, a light, and
   the pawn placed on it, with a fixed top-down camera so movement is visible
   in the video. Dynamic lighting only (Lumen or none); no baked lighting.
4. **A C++ pawn**, `ASamplePawn`, set as the default pawn of the game mode:
   - moves on **W A S D** and the **left stick** (axis mappings `MoveForward`
     and `MoveRight`, in `Config/DefaultInput.ini`: W/S, A/D and
     `Gamepad_LeftY`, `Gamepad_LeftX`);
   - its speed is one named constant in `Source/Sample/SamplePawn.h`:

         static constexpr float PawnSpeed = 600.f;

   - **changes colour on click or tap**: the left mouse button (action
     `Recolor`; touch from the browser arrives as left-click). Each click cycles
     the pawn's material instance colour;
   - its base colour comes from a config property, so a task can change it
     without touching C++:

         UPROPERTY(Config) FLinearColor PawnBaseColor;

     read from `[/Script/Sample.SamplePawn]` in `Config/DefaultGame.ini`
     (`PawnBaseColor=(R=0.1,G=0.4,B=0.9,A=1.0)`).
   - the player controller shows the mouse cursor and enables click events.
5. **One editor script**, `Scripts/make_material_instance.py`, using the
   `unreal` Python API: it creates `/Game/Materials/MI_Pawn` as a
   MaterialInstanceConstant whose parent is
   `/Engine/BasicShapes/BasicShapeMaterial`, and sets its `Color` vector
   parameter to the pawn's base colour. It must fail loudly (raise) when the
   asset is not created. The pawn loads `MI_Pawn` at runtime.
6. **`burrow-play.json`** at the project root, the manifest stage 4 runs before
   the cook:

        {"editor_scripts": ["Scripts/make_material_instance.py"],
         "commandlets": [{"run": "CompileAllBlueprints"}]}

7. **PythonScriptPlugin enabled** in `Sample.uproject`:

        "Plugins": [{"Name": "PythonScriptPlugin", "Enabled": true}]

   Without it the build fails with `PythonScriptPlugin not enabled`.

## Acceptance (before handing over)

- `git lfs ls-files` lists the `.umap` and every `.uasset`.
- Opening the project in the editor, `Scripts/make_material_instance.py` runs
  clean (Output Log shows no error) and `MI_Pawn` appears.
- PIE: WASD and the left stick move the pawn; clicking changes its colour.
- Changing `PawnSpeed` in `SamplePawn.h` and `PawnBaseColor` in
  `DefaultGame.ini` changes the pawn's speed and colour in the next package
  (these are the two edits the canvas leg's task makes).
- A Burrow project exists for the repo, and its commit SHA is given to the
  person running `docs/dryrun-runbook.md`.
