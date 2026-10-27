// Package dryrun is the stage 5 harness: it runs the real build, launch,
// capture and encode Stages end to end against a loopback "core", measures the
// build and the host-to-photon probe, and writes report.json. Check validates
// a report; a missing or partial report is a FAIL, never a pass.
package dryrun

import (
	"encoding/json"
	"os"
)

// KindHostToPhoton labels every probe row. It measures from probe receipt to
// photon and excludes the game's own input-to-render time, so it is never to
// be compared with the Godot figure that included it.
const KindHostToPhoton = "host_to_photon"

// Report is report.json. The key set and order are pinned by
// testdata/report_golden.json.
type Report struct {
	SHA            string        `json:"sha"`
	UEVersion      string        `json:"ue_version"`
	GPU            string        `json:"gpu"` // the model name only, never a hostname
	CaptureBackend string        `json:"capture_backend"`
	Encoder        string        `json:"encoder"`
	BuildMS        BuildMS       `json:"build_ms"`
	Probe          []ProbeRow    `json:"probe"`
	CanvasSession  CanvasSession `json:"canvas_session"`
}

// BuildMS is the cold and the warm build.
type BuildMS struct {
	Cold BuildPhase `json:"cold"`
	Warm BuildPhase `json:"warm"`
}

// BuildPhase is one build from session start to connecting.
type BuildPhase struct {
	Total   int64            `json:"total"`
	PerStep map[string]int64 `json:"per_step"`
}

// ProbeRow is one backend x preset cell. N counts answered probes; Lost the
// ones that never came back.
type ProbeRow struct {
	Backend string  `json:"backend"`
	Preset  string  `json:"preset"`
	Kind    string  `json:"kind"`
	N       int     `json:"n"`
	Lost    int     `json:"lost"`
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	FPS     float64 `json:"fps"`
}

// CanvasSession is the Burrow-driven session. dryrun leaves it empty and
// reached_live false; MergeCanvas fills it from the session summary.
type CanvasSession struct {
	SessionID   string  `json:"session_id"`
	ReachedLive bool    `json:"reached_live"`
	RTTP50      float64 `json:"rtt_p50_ms"`
	RTTP95      float64 `json:"rtt_p95_ms"`
}

// Write stores r as indented JSON.
func (r *Report) Write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// MergeCanvas records the canvas leg in the report at path.
func MergeCanvas(path string, c CanvasSession) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	r.CanvasSession = c
	return r.Write(path)
}
