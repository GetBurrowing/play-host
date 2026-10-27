package unreal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/launch"
)

// ManifestName is the optional file at the UE project root that names the
// editor-side steps a build runs before it is cooked.
const ManifestName = "burrow-play.json"

// Manifest is burrow-play.json:
//
//	{"editor_scripts": ["Scripts/x.py"], "commandlets": [{"run": "CompileAllBlueprints"}]}
//
// Scripts run through the pythonscript commandlet. A commandlet entry whose
// run is pythonscript names its own "script". Nothing else is accepted.
type Manifest struct {
	EditorScripts []string        `json:"editor_scripts"`
	Commandlets   []CommandletRef `json:"commandlets"`
}

// CommandletRef is one commandlet to run.
type CommandletRef struct {
	Run    string `json:"run"`
	Script string `json:"script,omitempty"`
}

// AllowedCommandlets is the whole list. A manifest is repo content, so what it
// can make the editor run is fixed here and not by the manifest.
var AllowedCommandlets = []string{"pythonscript", "CompileAllBlueprints", "ResavePackages", "DataValidation"}

// pythonPlugin is the plugin a script run needs.
const pythonPlugin = "PythonScriptPlugin"

// EditorJob is one UnrealEditor-Cmd invocation.
type EditorJob struct {
	Commandlet string // the -run= value
	Script     string // absolute path, for pythonscript
}

// LoadManifest reads burrow-play.json from dir; (nil, nil) when absent.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestName, err)
	}
	return &m, nil
}

// ResolveScript returns the absolute path of a manifest script, refusing any
// path that is absolute, uses "..", or (through a symlink) leaves the project.
func ResolveScript(projectDir, rel string) (string, error) {
	bad := fmt.Errorf("editor script path escapes the project: %s", rel)
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) ||
		(len(rel) >= 2 && rel[1] == ':') {
		return "", bad
	}
	for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", bad
		}
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
	if !within(abs, root) {
		return "", bad
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		realRoot, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			realRoot = root
		}
		if !within(real, realRoot) {
			return "", bad
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("editor script not found: %s", rel)
	}
	return abs, nil
}

// PlanEditorJobs validates the project's manifest and returns the editor jobs
// it asks for, scripts first, in order. No manifest (or an empty one) is no
// jobs, so no editor process runs at all.
func PlanEditorJobs(projectDir, uproject string) ([]EditorJob, error) {
	m, err := LoadManifest(projectDir)
	if err != nil || m == nil {
		return nil, err
	}
	var jobs []EditorJob
	for _, rel := range m.EditorScripts {
		abs, err := ResolveScript(projectDir, rel)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, EditorJob{Commandlet: "pythonscript", Script: abs})
	}
	for _, c := range m.Commandlets {
		if !commandletAllowed(c.Run) {
			return nil, fmt.Errorf("commandlet not allowed: %s", c.Run)
		}
		job := EditorJob{Commandlet: c.Run}
		if c.Run == "pythonscript" {
			if c.Script == "" {
				return nil, errors.New("commandlet pythonscript needs a script")
			}
			abs, err := ResolveScript(projectDir, c.Script)
			if err != nil {
				return nil, err
			}
			job.Script = abs
		} else if c.Script != "" {
			return nil, fmt.Errorf("commandlet %s takes no script", c.Run)
		}
		jobs = append(jobs, job)
	}
	for _, j := range jobs {
		if j.Commandlet == "pythonscript" {
			if err := requirePythonPlugin(uproject); err != nil {
				return nil, err
			}
			break
		}
	}
	return jobs, nil
}

func commandletAllowed(name string) bool {
	for _, a := range AllowedCommandlets {
		if a == name {
			return true
		}
	}
	return false
}

// requirePythonPlugin fails unless the .uproject enables PythonScriptPlugin.
func requirePythonPlugin(uproject string) error {
	b, err := os.ReadFile(uproject)
	if err != nil {
		return err
	}
	var p struct {
		Plugins []struct {
			Name    string `json:"Name"`
			Enabled bool   `json:"Enabled"`
		} `json:"Plugins"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(uproject), err)
	}
	for _, pl := range p.Plugins {
		if pl.Name == pythonPlugin && pl.Enabled {
			return nil
		}
	}
	return errors.New("PythonScriptPlugin not enabled")
}

// EditorArgs is UnrealEditor-Cmd's command line for one job.
func EditorArgs(uproject string, j EditorJob) []string {
	a := []string{uproject, "-run=" + j.Commandlet}
	if j.Script != "" {
		a = append(a, "-script="+j.Script)
	}
	return append(a, "-nullrhi", "-unattended", "-nop4", "-nosplash", "-stdout", "-FullStdOutLogOutput")
}

func (b *Builder) editorCommand(exe string, args []string) (string, []string) {
	if b.EditorCommand != nil {
		return b.EditorCommand(exe, args)
	}
	return exe, args
}

// runEditorJobs runs the manifest's jobs, in order, under the session Job.
// The step word is "Editor scripts". It runs after the LFS pull and before
// RunUAT; with no jobs no editor process is started and the guard is not
// consulted.
func (b *Builder) runEditorJobs(ctx context.Context, in Input, uproject string, jobs []EditorJob, progress func(string)) error {
	if len(jobs) == 0 {
		return nil
	}
	// Session 0 has no desktop, and an editor needs a real session. Refusing
	// without an Env is deliberate: no guard configured is no guard.
	if b.Env == nil {
		return errors.New("editor scripts refused: no session guard configured")
	}
	if err := capture.Guard(b.Env); err != nil {
		return fmt.Errorf("editor scripts %s", strings.TrimPrefix(err.Error(), "capture "))
	}
	exe := filepath.Join(b.Cfg.UERoot, "Engine", "Binaries", "Win64", "UnrealEditor-Cmd.exe")
	for i, j := range jobs {
		path, args := b.editorCommand(exe, EditorArgs(uproject, j))
		spec := launch.Spec{Path: path, Args: args, Dir: in.Dir, LogPath: fmt.Sprintf("%s.editor%d", in.LogPath, i), Env: append(b.Cfg.Env(), b.ExtraEnv...)}
		b.logf("unreal: editor job %d/%d: -run=%s", i+1, len(jobs), j.Commandlet)
		if err := b.runJob(ctx, spec, "UnrealEditor-Cmd", StepEditor, false, progress); err != nil {
			return err
		}
	}
	return nil
}
