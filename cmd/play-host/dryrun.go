package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/dryrun"
	"github.com/Startup-Suite/play-host/internal/unreal"
)

// dryrunCmd runs the real Stages end to end against a loopback core and
// writes report.json (docs/dryrun-runbook.md).
func dryrunCmd(args []string) error {
	fs := flag.NewFlagSet("dryrun", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository URL, as listed in config.json repos")
	sha := fs.String("sha", "", "40-hex commit to build")
	cfgPath := fs.String("config", "", "config.json")
	out := fs.String("out", "report.json", "report path")
	driver := fs.String("driver", "spike/driver/run-probes.mjs", "path to run-probes.mjs")
	cdp := fs.String("cdp", "127.0.0.1:19223", "Chrome remote-debugging host:port")
	node := fs.String("node", "node", "node executable")
	probes := fs.Int("probes", 150, "probes per cell")
	presets := fs.String("presets", "p1/ll,p4/ll", "preset/tune cells; the first is also the cold and warm build")
	wipe := fs.Bool("wipe-caches", false, "empty ddc_dir and drop this commit's archive before the cold run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" || *sha == "" || *cfgPath == "" {
		return fmt.Errorf("usage: play-host dryrun --repo <url> --sha <40hex> --config config.json --out report.json")
	}
	var matrix []dryrun.Preset
	for _, p := range strings.Split(*presets, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(p), "/")
		if !ok || a == "" || b == "" {
			return fmt.Errorf("--presets: %q is not preset/tune", p)
		}
		matrix = append(matrix, dryrun.Preset{Preset: a, Tune: b})
	}
	c, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if err := c.resolveExecutables(); err != nil {
		return err
	}
	lf, err := setupLogging(c.LogsDir)
	if err != nil {
		return err
	}
	defer lf.Close()
	defer logPanic()

	hc := c.hostConfig()
	hc.PionLog = log.Writer()
	r, err := hc.Build.FindRepo(*repo)
	if err != nil {
		return err
	}
	backend := c.CaptureBackend
	if backend == "" {
		backend = capture.DefaultBackend
	}
	ueVersion, err := unreal.EngineVersion(c.UERoot)
	if err != nil {
		return fmt.Errorf("ue_version: %w", err)
	}
	gpu, err := gpuModel(func() ([]byte, error) {
		return exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	})
	if err != nil {
		return fmt.Errorf("gpu: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("play-host %s dryrun: %s @ %s", Version, *repo, *sha)
	rep, err := dryrun.Run(ctx, dryrun.Options{
		Repo: *repo, SHA: *sha, Host: hc,
		Prober: dryrun.NodeProber{Node: *node, Driver: *driver, CDP: *cdp},
		Matrix: matrix, Probes: *probes, WipeCaches: *wipe,
		UEVersion: ueVersion, GPU: gpu, Backend: string(backend), Encoder: string(hc.Encoder),
		ArchiveDir: hc.Unreal.ArchiveDir(unreal.RepoKey(r.Match), *sha),
		Logf:       log.Printf,
	})
	if err != nil {
		return err
	}
	if err := rep.Write(*out); err != nil {
		return err
	}
	log.Printf("dryrun: wrote %s; the canvas leg is still to merge (dryrun-canvas), then dryrun-check", *out)
	return nil
}

// gpuModel is the first line nvidia-smi prints: the model name, never a host.
func gpuModel(run func() ([]byte, error)) (string, error) {
	b, err := run()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("nvidia-smi printed no GPU name")
	}
	return line, nil
}

// dryrunCheckCmd exits 0 only for a complete, passing report.
func dryrunCheckCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: play-host dryrun-check report.json")
		return 2
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Println("FAIL: report missing")
		return 1
	}
	if err := dryrun.Check(b); err != nil {
		fmt.Println(err)
		return 1
	}
	fmt.Println("OK")
	return 0
}

// dryrunCanvasCmd records the Burrow-driven session in a report.
func dryrunCanvasCmd(args []string) error {
	fs := flag.NewFlagSet("dryrun-canvas", flag.ContinueOnError)
	report := fs.String("report", "", "report.json to update")
	id := fs.String("session-id", "", "play session id")
	live := fs.Bool("reached-live", false, "the session reached live")
	p50 := fs.Float64("rtt-p50", 0, "rtt p50 ms from the session summary")
	p95 := fs.Float64("rtt-p95", 0, "rtt p95 ms from the session summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *report == "" || *id == "" {
		return fmt.Errorf("usage: play-host dryrun-canvas --report report.json --session-id <id> --reached-live --rtt-p50 <ms> --rtt-p95 <ms>")
	}
	return dryrun.MergeCanvas(*report, dryrun.CanvasSession{SessionID: *id, ReachedLive: *live, RTTP50: *p50, RTTP95: *p95})
}
