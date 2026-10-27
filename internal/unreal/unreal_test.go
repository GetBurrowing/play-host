package unreal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/launch"
)

// fakeScript drives the re-exec'd RunUAT stand-in (FAKE_UAT in the env).
type fakeScript struct {
	Steps []struct {
		SleepMs int    `json:"sleep_ms"`
		Line    string `json:"line"`
	} `json:"steps"`
	Exit int  `json:"exit"`
	Hang bool `json:"hang"`
}

func TestMain(m *testing.M) {
	if s := os.Getenv("FAKE_UAT"); s != "" {
		fakeUAT(s)
		return
	}
	os.Exit(m.Run())
}

func fakeUAT(raw string) {
	var sc fakeScript
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		os.Exit(99)
	}
	var archive string
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "-archivedirectory="); ok {
			archive = v
		}
	}
	for _, st := range sc.Steps {
		time.Sleep(time.Duration(st.SleepMs) * time.Millisecond)
		fmt.Println(st.Line)
	}
	if sc.Hang {
		time.Sleep(time.Minute)
	}
	if sc.Exit == 0 && archive != "" {
		_ = os.MkdirAll(filepath.Join(archive, "Windows"), 0o755)
		_ = os.WriteFile(filepath.Join(archive, "Windows", "Sample.exe"), []byte("x"), 0o755)
	}
	os.Exit(sc.Exit)
}

func banner(name string) string { return "********** " + name + " COMMAND STARTED **********" }

func script(t *testing.T, sc fakeScript) {
	t.Helper()
	b, _ := json.Marshal(sc)
	t.Setenv("FAKE_UAT", string(b))
}

func fullScript(gapMs int) fakeScript {
	var sc fakeScript
	for _, b := range []string{"BUILD", "COOK", "STAGE", "PACKAGE", "ARCHIVE"} {
		sc.Steps = append(sc.Steps, struct {
			SleepMs int    `json:"sleep_ms"`
			Line    string `json:"line"`
		}{gapMs, banner(b)})
	}
	sc.Steps = append(sc.Steps, struct {
		SleepMs int    `json:"sleep_ms"`
		Line    string `json:"line"`
	}{gapMs, "AutomationTool exiting with ExitCode=0 (Success)"})
	return sc
}

type rig struct {
	t       *testing.T
	b       *Builder
	in      Input
	cfg     Config
	mu      sync.Mutex
	specs   []launch.Spec
	runs    []string
	procs   []*recProc
	details []string
}

type recProc struct {
	launch.Proc
	mu     sync.Mutex
	killed int
}

func (p *recProc) Kill() error { p.mu.Lock(); p.killed++; p.mu.Unlock(); return p.Proc.Kill() }

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	checkouts := filepath.Join(root, "checkouts")
	dir := filepath.Join(checkouts, "task", "sha")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Sample.uproject"), []byte("{}"), 0o644)
	cfg := Config{
		UERoot: filepath.Join(root, "ue"), ArchiveRoot: filepath.Join(root, "archive"), DDCDir: filepath.Join(root, "ddc"),
		CheckoutsDir: checkouts, ProgressEvery: 20 * time.Millisecond,
	}
	r := &rig{t: t, cfg: cfg, in: Input{RepoKey: "owner_repo", SHA: strings.Repeat("ab", 20), Dir: dir, LogPath: filepath.Join(root, "uat.log")}}
	exe, _ := os.Executable()
	r.b = &Builder{
		Cfg: cfg, Poll: 5 * time.Millisecond,
		Run: func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
			r.mu.Lock()
			r.runs = append(r.runs, strings.Join(args, " "))
			r.mu.Unlock()
			return nil, nil
		},
		Command: func(uat string, args []string) (string, []string) { return exe, args },
		Start: func(s launch.Spec) (launch.Proc, error) {
			r.mu.Lock()
			r.specs = append(r.specs, s)
			r.mu.Unlock()
			p, err := launch.Start(s)
			if err != nil {
				return nil, err
			}
			rp := &recProc{Proc: p}
			r.mu.Lock()
			r.procs = append(r.procs, rp)
			r.mu.Unlock()
			return rp, nil
		},
	}
	return r
}

func (r *rig) build() (string, error) {
	return r.b.Build(context.Background(), r.in, func(d string) {
		r.mu.Lock()
		r.details = append(r.details, d)
		r.mu.Unlock()
	})
}

// stepOrder is the distinct step words of the reported details, in order.
func stepOrder(details []string) []string {
	var out []string
	for _, d := range details {
		for _, s := range Steps {
			if strings.HasPrefix(d, s) && (len(out) == 0 || out[len(out)-1] != s) {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestUnrealBuildProgressOrder(t *testing.T) {
	script(t, fullScript(60))
	r := newRig(t)
	exe, err := r.build()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Preparing", "Compiling", "Cooking", "Staging", "Packaging", "Archiving"}
	if got := stepOrder(r.details); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps %v, want %v (details %q)", got, want, r.details)
	}
	for _, d := range r.details {
		if strings.Contains(d, "%") {
			t.Errorf("detail carries a percentage: %q", d)
		}
		if len(d) > 500 {
			t.Errorf("detail over 500 bytes")
		}
	}
	wantExe := filepath.Join(r.cfg.ArchiveRoot, "owner_repo", r.in.SHA, "Windows", "Sample.exe")
	if exe != wantExe {
		t.Fatalf("exe %s want %s", exe, wantExe)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.ArchiveRoot, "owner_repo", r.in.SHA+".partial")); err == nil {
		t.Error(".partial left behind")
	}
	if _, err := os.Stat(filepath.Join(r.cfg.ArchiveRoot, "owner_repo", r.in.SHA, ".complete")); err != nil {
		t.Error("no .complete marker")
	}
}

func TestUnrealBuildRunsLFSPull(t *testing.T) {
	script(t, fullScript(1))
	r := newRig(t)
	if _, err := r.build(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.runs, " | ")
	if got != "lfs version | lfs install --local | lfs pull" {
		t.Fatalf("git lfs calls: %s", got)
	}
}

func TestUnrealBuildFailureDetail(t *testing.T) {
	sc := fakeScript{Exit: 25}
	add := func(line string) {
		sc.Steps = append(sc.Steps, struct {
			SleepMs int    `json:"sleep_ms"`
			Line    string `json:"line"`
		}{5, line})
	}
	add(banner("BUILD"))
	add(banner("COOK"))
	add("LogCook: Error: Failed to load /Game/Maps/Missing")
	add("LogInit: shutting down")
	add("AutomationTool exiting with ExitCode=25 (Error_UnknownCookFailure)")
	script(t, sc)
	r := newRig(t)
	_, err := r.build()
	if err == nil || err.Error() != "Build failed during Cooking: LogCook: Error: Failed to load /Game/Maps/Missing" {
		t.Fatalf("got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(r.cfg.ArchiveRoot, "owner_repo", r.in.SHA+".partial")); serr == nil {
		t.Error(".partial kept after a failure")
	}
	if _, serr := os.Stat(filepath.Join(r.cfg.ArchiveRoot, "owner_repo", r.in.SHA)); serr == nil {
		t.Error("a failed build left an archive")
	}
}

func TestUnrealBuildFailureLineIsCapped(t *testing.T) {
	long := "LogCook: Error: " + strings.Repeat("x", 600)
	sc := fakeScript{Exit: 1}
	sc.Steps = append(sc.Steps, struct {
		SleepMs int    `json:"sleep_ms"`
		Line    string `json:"line"`
	}{1, long})
	script(t, sc)
	r := newRig(t)
	_, err := r.build()
	if err == nil || len(err.Error()) > len("Build failed during Preparing: ")+MaxErrBytes {
		t.Fatalf("got %d bytes: %v", len(err.Error()), err)
	}
}

func TestUnrealBuildTimeoutKillsJob(t *testing.T) {
	sc := fakeScript{Hang: true}
	sc.Steps = append(sc.Steps, struct {
		SleepMs int    `json:"sleep_ms"`
		Line    string `json:"line"`
	}{1, banner("BUILD")})
	script(t, sc)
	r := newRig(t)
	r.b.Cfg.Timeout = 400 * time.Millisecond
	r.cfg.Timeout = r.b.Cfg.Timeout
	type res struct {
		exe string
		err error
	}
	ch := make(chan res, 1)
	go func() { e, err := r.build(); ch <- res{e, err} }()
	select {
	case got := <-ch:
		if got.err == nil || got.err.Error() != "Build timed out during Compiling" {
			t.Fatalf("got %q, %v", got.exe, got.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the build outlived its timeout")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.procs) != 1 || r.procs[0].killed == 0 {
		t.Fatalf("the job was not killed: %+v", r.procs)
	}
	if ids, _ := r.procs[0].Pids(); len(ids) != 0 {
		t.Fatalf("job still has processes: %v", ids)
	}
}

func TestUnrealArchiveReuseSkipsBuild(t *testing.T) {
	r := newRig(t)
	final := r.cfg.ArchiveDir("owner_repo", r.in.SHA)
	os.MkdirAll(filepath.Join(final, "Windows"), 0o755)
	os.WriteFile(filepath.Join(final, "Windows", "Sample.exe"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(final, ".complete"), nil, 0o644)
	r.b.Start = func(launch.Spec) (launch.Proc, error) {
		t.Error("RunUAT started for a cached build")
		return nil, fmt.Errorf("no")
	}
	r.b.Run = func(context.Context, string, string, ...string) ([]byte, error) {
		t.Error("git ran for a cached build")
		return nil, nil
	}
	exe, err := r.build()
	if err != nil || exe != ExePath(final, "Sample") {
		t.Fatalf("%s %v", exe, err)
	}
	if len(r.details) != 1 || r.details[0] != "Reusing build "+r.in.SHA[:12] {
		t.Fatalf("details %q", r.details)
	}
	// An archive without the marker is not trusted.
	os.Remove(filepath.Join(final, ".complete"))
	script(t, fullScript(1))
	r.b.Start = func(s launch.Spec) (launch.Proc, error) { return launch.Start(s) }
	r.b.Run = func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil }
	if _, err := r.build(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(final, ".complete")); err != nil {
		t.Fatal("rebuild did not complete the archive")
	}
}

func TestDDCOutsideCheckouts(t *testing.T) {
	r := newRig(t)
	bad := r.cfg
	bad.DDCDir = filepath.Join(bad.CheckoutsDir, "task", "DerivedDataCache")
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "ddc_dir") {
		t.Fatalf("a DDC under the checkouts dir was accepted: %v", err)
	}
	same := r.cfg
	same.DDCDir = same.CheckoutsDir
	if same.Validate() == nil {
		t.Fatal("DDC == checkouts accepted")
	}
	script(t, fullScript(1))
	if _, err := r.build(); err != nil {
		t.Fatal(err)
	}
	var got string
	for _, e := range r.specs[0].Env {
		if v, ok := strings.CutPrefix(e, "UE-LocalDataCachePath="); ok {
			got = v
		}
	}
	if got != r.cfg.DDCDir || within(got, r.cfg.CheckoutsDir) || within(got, r.in.Dir) {
		t.Fatalf("RunUAT got DDC %q (want %q, outside %q)", got, r.cfg.DDCDir, r.cfg.CheckoutsDir)
	}
}

func TestLFSMissingFails(t *testing.T) {
	r := newRig(t)
	r.b.Run = func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "version" {
			return nil, fmt.Errorf("git: 'lfs' is not a git command")
		}
		return nil, nil
	}
	r.b.Start = func(launch.Spec) (launch.Proc, error) {
		t.Error("RunUAT started without git lfs")
		return nil, fmt.Errorf("no")
	}
	_, err := r.build()
	if err == nil || err.Error() != "git lfs missing on the play host" {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchArchivedExePath(t *testing.T) {
	exe := filepath.Join("archive", "owner_repo", "sha", "Windows", "Sample.exe")
	s := LaunchSpec(exe, 1280, 720, "game.log")
	if s.Path != exe || strings.Join(s.Args, " ") != "-windowed -ResX=1280 -ResY=720 -WinX=0 -WinY=0 -nosplash -log" || s.LogPath != "game.log" {
		t.Fatalf("%+v", s)
	}
}

func TestUATArgs(t *testing.T) {
	got := strings.Join(UATArgs("proj/Sample.uproject", "arch.partial"), " ")
	want := "BuildCookRun -project=proj/Sample.uproject -noP4 -utf8output -unattended -platform=Win64 -clientconfig=Development -build -cook -stage -pak -archive -archivedirectory=arch.partial"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}
