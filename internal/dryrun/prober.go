package dryrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// NodeProber drives spike/driver/run-probes.mjs (headless Chrome over CDP,
// internal/spike/web/probe.js) against the loopback core. Chrome must already
// be running with --remote-debugging-port.
type NodeProber struct {
	Node   string // node executable ("" = node)
	Driver string // path to run-probes.mjs
	CDP    string // host:port of Chrome's debugging port
	// GapMS is run-probes' --gap. probe.js waits gap + random(0..gap) between
	// probes, so the mean interval is 1.5 x gap; 67 gives the 10 Hz of the spec.
	GapMS int
}

// Probe implements Prober.
func (p NodeProber) Probe(ctx context.Context, signalURL, label string, n int) (ProbeResult, error) {
	node, gap := p.Node, p.GapMS
	if node == "" {
		node = "node"
	}
	if gap == 0 {
		gap = 67
	}
	dir, err := os.MkdirTemp("", "dryrun-probe-")
	if err != nil {
		return ProbeResult{}, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "probe.json")
	cmd := exec.CommandContext(ctx, node, p.Driver, "--cdp", p.CDP, "--signal", signalURL,
		"--n", fmt.Sprint(n), "--gap", fmt.Sprint(gap), "--label", label, "--out", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return ProbeResult{}, fmt.Errorf("run-probes.mjs: %w: %s", err, tailBytes(b, 400))
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return ProbeResult{}, err
	}
	return ParseDriverOutput(b)
}

// ParseDriverOutput reads the file run-probes.mjs writes with --out.
func ParseDriverOutput(b []byte) (ProbeResult, error) {
	var d struct {
		Lost    int `json:"lost"`
		Samples []struct {
			RTT float64 `json:"rtt"`
		} `json:"samples"`
		Browser struct {
			FPS float64 `json:"framesPerSecond"`
		} `json:"browser"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return ProbeResult{}, err
	}
	r := ProbeResult{Lost: d.Lost, FPS: d.Browser.FPS}
	for _, s := range d.Samples {
		r.RTTMs = append(r.RTTMs, s.RTT)
	}
	return r, nil
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
