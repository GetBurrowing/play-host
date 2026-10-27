package dryrun

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/report_golden.json")

func sampleReport() Report {
	return Report{
		SHA: "0123456789abcdef0123456789abcdef01234567", UEVersion: "5.4.2", GPU: "Test GPU 1000",
		CaptureBackend: "ddagrab", Encoder: "nvenc",
		BuildMS: BuildMS{
			Cold: BuildPhase{Total: 1800000, PerStep: map[string]int64{"Compiling": 600000, "Cooking": 1200000}},
			Warm: BuildPhase{Total: 400000, PerStep: map[string]int64{"Compiling": 100000, "Cooking": 300000}},
		},
		Probe: []ProbeRow{
			{Backend: "ddagrab", Preset: "p1/ll", Kind: KindHostToPhoton, N: 150, Lost: 0, P50: 31, P95: 44, FPS: 59.5},
			{Backend: "ddagrab", Preset: "p4/ll", Kind: KindHostToPhoton, N: 150, Lost: 1, P50: 33, P95: 47, FPS: 59.1},
		},
		CanvasSession: CanvasSession{SessionID: "", ReachedLive: false},
	}
}

// What dryrun writes, before the canvas leg, is pinned byte for byte.
func TestDryrunReportSchemaGolden(t *testing.T) {
	r := sampleReport()
	path := t.TempDir() + "/r.json"
	if err := r.Write(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if *update {
		os.WriteFile("testdata/report_golden.json", got, 0o644)
	}
	want, err := os.ReadFile("testdata/report_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may have turned the golden's LF into CRLF.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if string(got) != string(want) {
		t.Fatalf("report schema changed:\n%s\nwant:\n%s", got, want)
	}
	// reached_live is always written, even when false.
	var m map[string]map[string]any
	json.Unmarshal(got, &m)
	if _, ok := m["canvas_session"]["reached_live"]; !ok {
		t.Fatal("reached_live omitted")
	}
	if err := Check(got); err == nil || err.Error() != "FAIL: canvas_session.session_id invalid" {
		t.Fatalf("golden before the canvas leg: %v", err)
	}
}

func passing() map[string]any {
	r := sampleReport()
	r.CanvasSession = CanvasSession{SessionID: "s1", ReachedLive: true, RTTP50: 30, RTTP95: 50}
	b, _ := json.Marshal(r)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func row(m map[string]any, i int) map[string]any { return m["probe"].([]any)[i].(map[string]any) }

func checkErr(m map[string]any) string {
	b, _ := json.Marshal(m)
	if err := Check(b); err != nil {
		return err.Error()
	}
	return ""
}

func TestDryrunCheckPasses(t *testing.T) {
	if e := checkErr(passing()); e != "" {
		t.Fatal(e)
	}
}

func TestDryrunCheckFailsOnMissingP95(t *testing.T) {
	m := passing()
	delete(row(m, 1), "p95_ms")
	if e := checkErr(m); e != "FAIL: probe[1].p95_ms missing" {
		t.Fatalf("%q", e)
	}
	m = passing()
	row(m, 0)["p95_ms"] = "44"
	if e := checkErr(m); e != "FAIL: probe[0].p95_ms invalid" {
		t.Fatalf("%q", e)
	}
	m = passing()
	row(m, 0)["p95_ms"] = 10.0 // below p50: a p50 copied into the wrong field reads the same way
	if e := checkErr(m); e != "FAIL: probe[0].p95_ms invalid" {
		t.Fatalf("%q", e)
	}
}

func TestDryrunCheckFailsOnN(t *testing.T) {
	for _, n := range []float64{0, 99} {
		m := passing()
		row(m, 0)["n"] = n
		if e := checkErr(m); e != "FAIL: probe[0].n invalid" {
			t.Fatalf("n=%v: %q", n, e)
		}
	}
	m := passing()
	row(m, 0)["n"] = 100.0
	if e := checkErr(m); e != "" {
		t.Fatalf("n=100 must pass: %q", e)
	}
	m = passing()
	delete(row(m, 0), "n")
	if e := checkErr(m); e != "FAIL: probe[0].n missing" {
		t.Fatalf("%q", e)
	}
}

func TestDryrunCheckFailsOnCanvasAndShape(t *testing.T) {
	m := passing()
	delete(m["canvas_session"].(map[string]any), "reached_live")
	if e := checkErr(m); e != "FAIL: canvas_session.reached_live missing" {
		t.Fatalf("%q", e)
	}
	m = passing()
	m["canvas_session"].(map[string]any)["reached_live"] = false
	if e := checkErr(m); e != "FAIL: canvas_session.reached_live invalid" {
		t.Fatalf("%q", e)
	}
	m = passing()
	row(m, 0)["kind"] = "rtt"
	if e := checkErr(m); e != "FAIL: probe[0].kind invalid" {
		t.Fatalf("%q", e)
	}
	m = passing()
	delete(m["build_ms"].(map[string]any), "warm")
	if e := checkErr(m); e != "FAIL: build_ms.warm missing" {
		t.Fatalf("%q", e)
	}
	m = passing()
	m["build_ms"].(map[string]any)["cold"].(map[string]any)["per_step"].(map[string]any)["Cooking"] = "slow"
	if e := checkErr(m); e != "FAIL: build_ms.cold.per_step.Cooking invalid" {
		t.Fatalf("%q", e)
	}
	m = passing()
	delete(m, "gpu")
	if e := checkErr(m); e != "FAIL: gpu missing" {
		t.Fatalf("%q", e)
	}
	if err := Check([]byte("{")); err == nil {
		t.Fatal("garbage passed")
	}
}
