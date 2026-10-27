package unreal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project writes a UE project's files into the rig's checkout.
func (r *rig) project(manifest string, plugins string) {
	r.t.Helper()
	up := `{"FileVersion": 3, "Plugins": ` + plugins + `}`
	os.WriteFile(filepath.Join(r.in.Dir, "Sample.uproject"), []byte(up), 0o644)
	os.MkdirAll(filepath.Join(r.in.Dir, "Scripts"), 0o755)
	os.WriteFile(filepath.Join(r.in.Dir, "Scripts", "x.py"), []byte("print('hi')\n"), 0o644)
	if manifest != "" {
		os.WriteFile(filepath.Join(r.in.Dir, ManifestName), []byte(manifest), 0o644)
	}
}

const pythonOn = `[{"Name": "PythonScriptPlugin", "Enabled": true}]`

func editorScript(t *testing.T, sc fakeScript) {
	t.Helper()
	b, _ := json.Marshal(sc)
	t.Setenv("FAKE_EDITOR", string(b))
}

func ranEditor(r *rig) []string {
	var out []string
	for _, s := range r.specs {
		out = append(out, strings.Join(s.Args, " "))
	}
	return out
}

func TestEditorScriptsRunBeforeCook(t *testing.T) {
	script(t, fullScript(40))
	editorScript(t, fakeScript{Steps: []struct {
		SleepMs int    `json:"sleep_ms"`
		Line    string `json:"line"`
	}{{60, "LogPython: running x.py"}}})
	r := newRig(t)
	r.project(`{"editor_scripts": ["Scripts/x.py"], "commandlets": [{"run": "CompileAllBlueprints"}]}`, pythonOn)
	if _, err := r.build(); err != nil {
		t.Fatal(err)
	}
	calls := ranEditor(r)
	if len(calls) != 3 {
		t.Fatalf("want script, commandlet, RunUAT; got %q", calls)
	}
	script0 := filepath.Join(r.in.Dir, "Scripts", "x.py")
	if !strings.Contains(calls[0], "-run=pythonscript") || !strings.Contains(calls[0], "-script="+script0) ||
		!strings.Contains(calls[0], "-nullrhi") || !strings.Contains(calls[0], "-unattended") || !strings.Contains(calls[0], "-stdout") {
		t.Errorf("script call: %s", calls[0])
	}
	if !strings.Contains(calls[1], "-run=CompileAllBlueprints") || strings.Contains(calls[1], "-script=") {
		t.Errorf("commandlet call: %s", calls[1])
	}
	if !strings.HasPrefix(calls[2], "BuildCookRun") {
		t.Errorf("RunUAT did not come last: %s", calls[2])
	}
	if strings.Join(r.runs, "|") != "lfs version|lfs install --local|lfs pull" {
		t.Errorf("git calls %v", r.runs)
	}
	got := strings.Join(stepOrder(r.details), ",")
	if got != "Preparing,Editor scripts,Preparing,Compiling,Cooking,Staging,Packaging,Archiving" {
		t.Errorf("steps %s (details %q)", got, r.details)
	}
}

func TestEditorScriptFailureFailsBuild(t *testing.T) {
	script(t, fullScript(1))
	editorScript(t, fakeScript{Exit: 1, Steps: []struct {
		SleepMs int    `json:"sleep_ms"`
		Line    string `json:"line"`
	}{{5, "LogPython: Error: boom in x.py"}}})
	r := newRig(t)
	r.project(`{"editor_scripts": ["Scripts/x.py"]}`, pythonOn)
	_, err := r.build()
	if err == nil || err.Error() != "Build failed during Editor scripts: LogPython: Error: boom in x.py" {
		t.Fatalf("got %v", err)
	}
	if calls := ranEditor(r); len(calls) != 1 {
		t.Fatalf("RunUAT started after a failed script: %q", calls)
	}
}

func TestManifestAbsentNoEditorRun(t *testing.T) {
	script(t, fullScript(1))
	r := newRig(t)
	r.project("", pythonOn)
	if _, err := r.build(); err != nil {
		t.Fatal(err)
	}
	calls := ranEditor(r)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "BuildCookRun") {
		t.Fatalf("calls %q", calls)
	}
	// An empty manifest is the same, even in a session that would be refused.
	r2 := newRig(t)
	r2.b.Env = fakeEnv{0, 0}
	r2.project(`{}`, pythonOn)
	if _, err := r2.build(); err != nil {
		t.Fatalf("an empty manifest consulted the guard: %v", err)
	}
	if len(ranEditor(r2)) != 1 {
		t.Fatalf("editor ran for an empty manifest: %q", ranEditor(r2))
	}
}

func TestCommandletAllowlist(t *testing.T) {
	for _, c := range []string{"CompileAllBlueprints", "ResavePackages", "DataValidation"} {
		r := newRig(t)
		r.project(`{"commandlets": [{"run": "`+c+`"}]}`, pythonOn)
		if _, err := PlanEditorJobs(r.in.Dir, filepath.Join(r.in.Dir, "Sample.uproject")); err != nil {
			t.Errorf("%s refused: %v", c, err)
		}
	}
	r := newRig(t)
	r.project(`{"commandlets": [{"run": "pythonscript", "script": "Scripts/x.py"}]}`, pythonOn)
	if jobs, err := PlanEditorJobs(r.in.Dir, filepath.Join(r.in.Dir, "Sample.uproject")); err != nil || len(jobs) != 1 {
		t.Errorf("pythonscript: %v %v", jobs, err)
	}
	for _, bad := range []string{"GatherText", "Cook", "../x", "compileallblueprints", ""} {
		r := newRig(t)
		r.project(`{"commandlets": [{"run": "`+bad+`"}]}`, pythonOn)
		script(t, fullScript(1))
		_, err := r.build()
		if err == nil || err.Error() != "commandlet not allowed: "+bad {
			t.Errorf("%q: got %v", bad, err)
		}
		if len(r.specs) != 0 || len(r.runs) != 0 {
			t.Errorf("%q: something ran before the refusal: %v %v", bad, r.specs, r.runs)
		}
	}
}

func TestScriptPathEscapeRefused(t *testing.T) {
	up := ""
	for _, bad := range []string{"../x.py", "Scripts/../../x.py", "/etc/x.py", `\x.py`, `C:\x.py`, "Scripts\\..\\..\\x.py"} {
		r := newRig(t)
		r.project(`{"editor_scripts": [`+jsonString(bad)+`]}`, pythonOn)
		up = filepath.Join(r.in.Dir, "Sample.uproject")
		_, err := PlanEditorJobs(r.in.Dir, up)
		if err == nil || !strings.Contains(err.Error(), "escapes the project") {
			t.Errorf("%q: got %v", bad, err)
		}
	}
	r := newRig(t)
	r.project(`{"editor_scripts": ["Scripts/x.py"]}`, pythonOn)
	if _, err := PlanEditorJobs(r.in.Dir, filepath.Join(r.in.Dir, "Sample.uproject")); err != nil {
		t.Errorf("a script inside the project was refused: %v", err)
	}
	// A symlink out of the project is an escape too.
	outside := filepath.Join(t.TempDir(), "evil.py")
	os.WriteFile(outside, []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(r.in.Dir, "Scripts", "link.py")); err == nil {
		r.project(`{"editor_scripts": ["Scripts/link.py"]}`, pythonOn)
		if _, err := PlanEditorJobs(r.in.Dir, filepath.Join(r.in.Dir, "Sample.uproject")); err == nil || !strings.Contains(err.Error(), "escapes the project") {
			t.Errorf("symlink escape: %v", err)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestPythonPluginPreflight(t *testing.T) {
	for name, plugins := range map[string]string{
		"absent":   `[]`,
		"disabled": `[{"Name": "PythonScriptPlugin", "Enabled": false}]`,
		"other":    `[{"Name": "OtherPlugin", "Enabled": true}]`,
	} {
		r := newRig(t)
		r.project(`{"editor_scripts": ["Scripts/x.py"]}`, plugins)
		script(t, fullScript(1))
		_, err := r.build()
		if err == nil || err.Error() != "PythonScriptPlugin not enabled" {
			t.Errorf("%s: got %v", name, err)
		}
		if len(r.specs) != 0 {
			t.Errorf("%s: a process started before the preflight", name)
		}
	}
	// A manifest with no script needs no Python plugin.
	r := newRig(t)
	r.project(`{"commandlets": [{"run": "CompileAllBlueprints"}]}`, `[]`)
	if _, err := PlanEditorJobs(r.in.Dir, filepath.Join(r.in.Dir, "Sample.uproject")); err != nil {
		t.Errorf("commandlet-only manifest: %v", err)
	}
}

func TestEditorRunnerRefusesSession0(t *testing.T) {
	script(t, fullScript(1))
	r := newRig(t)
	r.project(`{"editor_scripts": ["Scripts/x.py"]}`, pythonOn)
	r.b.Env = fakeEnv{0, 0}
	_, err := r.build()
	if err == nil || err.Error() != "editor scripts refused: session 0" {
		t.Fatalf("got %v", err)
	}
	if len(r.specs) != 0 {
		t.Fatalf("a process started in session 0: %q", ranEditor(r))
	}
	// No guard configured is no editor, too.
	r2 := newRig(t)
	r2.project(`{"editor_scripts": ["Scripts/x.py"]}`, pythonOn)
	r2.b.Env = nil
	if _, err := r2.build(); err == nil || len(r2.specs) != 0 {
		t.Fatalf("an unguarded editor run: %v %v", err, r2.specs)
	}
	// A desktop session that is not the console session is refused as well.
	r3 := newRig(t)
	r3.project(`{"editor_scripts": ["Scripts/x.py"]}`, pythonOn)
	r3.b.Env = fakeEnv{2, 1}
	if _, err := r3.build(); err == nil || len(r3.specs) != 0 {
		t.Fatalf("non-console session: %v", err)
	}
}

func TestUnknownManifestKeyIsRefused(t *testing.T) {
	r := newRig(t)
	r.project(`{"editor_script": ["Scripts/x.py"]}`, pythonOn)
	if _, err := LoadManifest(r.in.Dir); err == nil {
		t.Fatal("a misspelt key was ignored, so its script would silently not run")
	}
}
