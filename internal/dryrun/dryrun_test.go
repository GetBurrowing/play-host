package dryrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/probe"
)

func quickSteps() map[string]time.Duration {
	m := map[string]time.Duration{}
	for _, s := range unrealOrder {
		m[s] = 60 * time.Millisecond
	}
	return m
}

func runFake(t *testing.T, port int, o Options) (*Report, error) {
	t.Helper()
	o.Repo, o.SHA = "https://example.invalid/owner/sample", testSHA
	o.Host = testHostConfig(t, port)
	o.UEVersion, o.GPU, o.Backend, o.Encoder = "5.4.2", "Test GPU 1000", "ddagrab", "nvenc"
	o.Prober = &fakeProber{t: t}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return Run(ctx, o)
}

// Fake UAT, fake capture and a fake probe responder produce a report with
// numeric n, p50, p95 and per-step build_ms, and it passes the check once the
// canvas leg is merged.
func TestDryrunOnFakes(t *testing.T) {
	u := &unrealStages{t: t, session: 1, steps: quickSteps()}
	rep, err := runFake(t, 40700, Options{Stages: u})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Probe) != 2 || rep.Probe[0].Preset != "p1/ll" || rep.Probe[1].Preset != "p4/ll" {
		t.Fatalf("matrix rows: %+v", rep.Probe)
	}
	want := probe.Summarize(func() []float64 {
		var v []float64
		for i := 0; i < 150; i++ {
			v = append(v, float64(25+(i*7)%20))
		}
		return v
	}())
	for _, r := range rep.Probe {
		if r.Kind != KindHostToPhoton || r.N != 150 || r.P50 != want.P50 || r.P95 != want.P95 || r.FPS != 59.5 {
			t.Errorf("row %+v, want n=150 p50=%v p95=%v", r, want.P50, want.P95)
		}
		if r.P95 <= r.P50 {
			t.Errorf("p95 %v must exceed p50 %v for this sample", r.P95, r.P50)
		}
	}
	for name, ph := range map[string]BuildPhase{"cold": rep.BuildMS.Cold, "warm": rep.BuildMS.Warm} {
		if ph.Total <= 0 {
			t.Errorf("%s total %d", name, ph.Total)
		}
		for _, s := range unrealOrder {
			if ph.PerStep[s] <= 0 {
				t.Errorf("%s per_step[%s] = %d in %+v", name, s, ph.PerStep[s], ph.PerStep)
			}
		}
	}
	if got := u.builds.Load(); got != 3 {
		t.Errorf("builds %d, want cold + p4 session + warm = 3 Prepare calls", got)
	}
	if host, _ := os.Hostname(); host != "" && strings.Contains(rep.GPU, host) {
		t.Errorf("gpu field carries the hostname: %q", rep.GPU)
	}
	if rep.GPU != "Test GPU 1000" {
		t.Errorf("gpu %q", rep.GPU)
	}
	// The canvas leg has not run: the check must say so.
	path := filepath.Join(t.TempDir(), "report.json")
	if err := rep.Write(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if err := Check(b); err == nil || err.Error() != "FAIL: canvas_session.session_id invalid" {
		t.Fatalf("a report without the canvas leg must fail the check: %v", err)
	}
	if err := MergeCanvas(path, CanvasSession{SessionID: "s1", ReachedLive: true, RTTP50: 31.5, RTTP95: 48}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if err := Check(b); err != nil {
		t.Fatalf("merged report: %v", err)
	}
}

// Session 0 is refused through the capture guard, so the run fails.
func TestDryrunRefusesSession0(t *testing.T) {
	u := &unrealStages{t: t, session: 0, steps: quickSteps()}
	_, err := runFake(t, 40704, Options{Stages: u})
	if err == nil || !strings.Contains(err.Error(), "session 0") {
		t.Fatalf("want a session 0 refusal, got %v", err)
	}
}

// An engine=godot checkout (the addon path, no EngineStages) still produces a report.
func TestDryrunGodotFixture(t *testing.T) {
	g := &godotStages{t: t, port: 40708}
	rep, err := runFake(t, 40708, Options{Stages: g, Matrix: []Preset{{"p4", "ll"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Probe) != 1 || rep.Probe[0].N != 150 || rep.Probe[0].P95 <= rep.Probe[0].P50 {
		t.Fatalf("rows %+v", rep.Probe)
	}
	if rep.BuildMS.Cold.Total <= 0 || rep.BuildMS.Cold.PerStep["Importing"] <= 0 || rep.BuildMS.Warm.Total <= 0 {
		t.Fatalf("build %+v", rep.BuildMS)
	}
}

// A populated cache is not a cold run.
func TestDryrunColdNeedsEmptyDDC(t *testing.T) {
	ddc := t.TempDir()
	os.WriteFile(filepath.Join(ddc, "x"), []byte("x"), 0o644)
	o := Options{Probes: 150, Prober: &fakeProber{t: t}}
	o.Host.Unreal.DDCDir = ddc
	if err := coldPreflight(&o); err == nil || !strings.Contains(err.Error(), "--wipe-caches") {
		t.Fatalf("want a refusal naming --wipe-caches, got %v", err)
	}
	o.WipeCaches = true
	if err := coldPreflight(&o); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(ddc); len(es) != 0 {
		t.Fatalf("ddc not emptied: %v", es)
	}
}

func TestParseDriverOutput(t *testing.T) {
	r, err := ParseDriverOutput([]byte(`{"lost":2,"samples":[{"rtt":30},{"rtt":40}],"browser":{"framesPerSecond":58}}`))
	if err != nil || r.Lost != 2 || len(r.RTTMs) != 2 || r.RTTMs[1] != 40 || r.FPS != 58 {
		t.Fatalf("%+v %v", r, err)
	}
}
