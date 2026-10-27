package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/rtc"
	"github.com/Startup-Suite/play-host/internal/unreal"
)

// TestHelperUAT is the RunUAT stand-in: the test binary re-executed with
// FAKE_UAT set. It prints UAT banners with controlled timing and exits.
func TestHelperUAT(t *testing.T) {
	raw := os.Getenv("FAKE_UAT_HOST")
	if raw == "" {
		return
	}
	var sc struct {
		Lines []string `json:"lines"`
		Gap   int      `json:"gap_ms"`
		Exit  int      `json:"exit"`
	}
	_ = json.Unmarshal([]byte(raw), &sc)
	var archive string
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "-archivedirectory="); ok {
			archive = v
		}
	}
	for _, l := range sc.Lines {
		time.Sleep(time.Duration(sc.Gap) * time.Millisecond)
		fmt.Println(l)
	}
	if sc.Exit == 0 && archive != "" {
		_ = os.MkdirAll(filepath.Join(archive, "Windows"), 0o755)
		_ = os.WriteFile(filepath.Join(archive, "Windows", "Sample.exe"), []byte("x"), 0o755)
	}
	os.Exit(sc.Exit)
}

// buildStages is an Unreal checkout whose Prepare runs the real unreal
// builder against the fake RunUAT.
type buildStages struct {
	*unrealStages
	root string
}

func (b *buildStages) Prepare(ctx context.Context, s protocol.SessionStart, progress func(string)) (string, error) {
	dir, _ := b.unrealStages.Prepare(ctx, s, progress)
	exe, _ := os.Executable()
	cfg := unreal.Config{
		UERoot: filepath.Join(b.root, "ue"), ArchiveRoot: filepath.Join(b.root, "archive"), DDCDir: filepath.Join(b.root, "ddc"),
		CheckoutsDir: filepath.Join(b.root, "checkouts"), ProgressEvery: 20 * time.Millisecond,
	}
	ub := &unreal.Builder{Cfg: cfg, Poll: 5 * time.Millisecond, ExtraEnv: []string{"FAKE_FFMPEG=0"},
		Run: func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil },
		Command: func(_ string, args []string) (string, []string) {
			return exe, append([]string{"-test.run=^TestHelperUAT$"}, args...)
		},
	}
	_, err := ub.Build(ctx, unreal.Input{RepoKey: "r", SHA: s.SHA, Dir: dir, LogPath: filepath.Join(b.root, "uat.log")}, progress)
	return dir, err
}

func uatScript(t *testing.T, exit int, lines ...string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"lines": lines, "gap_ms": 90, "exit": exit})
	t.Setenv("FAKE_UAT_HOST", string(b))
}

func building(c *core, id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, st := range c.statuses {
		if st.SessionID == id && st.State == protocol.StateBuilding {
			out = append(out, st.Detail)
		}
	}
	return out
}

func banners() []string {
	var l []string
	for _, b := range []string{"BUILD", "COOK", "STAGE", "PACKAGE", "ARCHIVE"} {
		l = append(l, "********** "+b+" COMMAND STARTED **********")
	}
	return append(l, "AutomationTool exiting with ExitCode=0 (Success)")
}

// The status stream a Unreal session shows: the building details start with
// each step word in order, then the session goes to connecting.
func TestUnrealBuildProgressOrder(t *testing.T) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	uatScript(t, 0, banners()...)
	u := &unrealStages{t: t, windows: theWindow}
	u.pushed = &capture.Pushed{Label: "idle", Run: func(ctx context.Context, _ capture.FrameFunc) error { <-ctx.Done(); return nil }}
	h, c := newCaptureHost(t, 40432, nil, nil)
	h.stages = &buildStages{unrealStages: u, root: t.TempDir()}
	h.OnEvent(protocol.EventSessionStart, startPayload("b1", 600))
	<-c.offer
	var seen []string
	for _, d := range building(c, "b1") {
		for _, w := range unreal.Steps {
			if strings.HasPrefix(d, w) && (len(seen) == 0 || seen[len(seen)-1] != w) {
				seen = append(seen, w)
			}
		}
	}
	want := "Preparing,Compiling,Cooking,Staging,Packaging,Archiving"
	if strings.Join(seen, ",") != want {
		t.Fatalf("steps %v, want %s (details %q)", seen, want, building(c, "b1"))
	}
	eventually(t, "connecting", func() bool { return c.last("b1").State == protocol.StateConnecting })
}

// A failing cook fails the session with the step and the line.
func TestUnrealBuildFailureReachesStatus(t *testing.T) {
	uatScript(t, 25, "********** BUILD COMMAND STARTED **********", "********** COOK COMMAND STARTED **********",
		"LogCook: Error: bad asset", "AutomationTool exiting with ExitCode=25 (Error_UnknownCookFailure)")
	u := &unrealStages{t: t, windows: theWindow}
	h, c := newCaptureHost(t, 40434, nil, nil)
	h.stages = &buildStages{unrealStages: u, root: t.TempDir()}
	h.OnEvent(protocol.EventSessionStart, startPayload("b2", 600))
	eventually(t, "failed", func() bool { return c.last("b2").State == protocol.StateFailed })
	if got := c.last("b2").Detail; got != "build: Build failed during Cooking: LogCook: Error: bad asset" {
		t.Fatalf("detail %q", got)
	}
}

// The real Launch starts the archived exe with the documented arguments.
func TestLaunchArchivedExePath(t *testing.T) {
	base, c := newTestHost(t, 40436, nil)
	h := New(base.cfg, c, nil, t.Logf)
	rs := h.stages.(*realStages)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Sample.uproject"), []byte("{}"), 0o644)
	exe := filepath.Join(t.TempDir(), "Windows", "Sample.exe")
	rs.archives = map[string]string{dir: exe}
	var got launch.Spec
	rs.start = func(s launch.Spec) (launch.Proc, error) { got = s; return &captureProc{done: make(chan struct{})}, nil }
	st := protocol.SessionStart{SessionID: "l1"}
	st.Encoder.Width, st.Encoder.Height = 1280, 720
	if _, err := rs.Launch(dir, st, "game.log"); err != nil {
		t.Fatal(err)
	}
	if got.Path != exe || strings.Join(got.Args, " ") != "-windowed -ResX=1280 -ResY=720 -WinX=0 -WinY=0 -nosplash -log" {
		t.Fatalf("launched %q %q", got.Path, got.Args)
	}
}
