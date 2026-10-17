package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/Startup-Suite/play-host/internal/build"
	"github.com/Startup-Suite/play-host/internal/host"
	"github.com/Startup-Suite/play-host/internal/input"
	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/media"
	"github.com/Startup-Suite/play-host/internal/suite"
)

// Version is stamped at build time with -ldflags "-X main.Version=<sha>".
var Version = "dev"

// FileConfig is the platform-neutral on-disk host configuration.
type FileConfig struct {
	BurrowURL string `json:"burrow_url"` // ws(s)://host[:port]/runtime/ws
	// SuiteURL is the deprecated name of burrow_url. A config.json that
	// still has only suite_url loads unchanged; burrow_url wins when both
	// are set.
	SuiteURL        string            `json:"suite_url,omitempty"`
	RuntimeID       string            `json:"runtime_id"` // play-host-a
	TokenFile       string            `json:"token_file"` // secrets\runtime-token (file only)
	Godot           string            `json:"godot"`      // executable, PATH name, or macOS .app bundle
	RenderingDriver string            `json:"rendering_driver"`
	FFmpeg          string            `json:"ffmpeg"`
	Encoder         media.EncoderKind `json:"encoder"`          // auto | nvenc | videotoolbox | libx264
	EncoderFallback bool              `json:"encoder_fallback"` // fallback to libx264 after chosen encoder init fails
	Git             string            `json:"git"`
	SSH             string            `json:"ssh"` // Git for Windows usr\bin\ssh.exe
	KnownHosts      string            `json:"known_hosts"`
	MirrorsDir      string            `json:"mirrors_dir"`
	CheckoutsDir    string            `json:"checkouts_dir"`
	AddonDir        string            `json:"addon_dir"`
	LogsDir         string            `json:"logs_dir"`
	HostIP          string            `json:"host_ip"`
	UDPMin          uint16            `json:"udp_min"`
	UDPMax          uint16            `json:"udp_max"`
	GodotPort       int               `json:"godot_port"`
	RTPPort         int               `json:"rtp_port"`
	AudioRTPPort    int               `json:"audio_rtp_port"`
	ImportTimeoutS  int               `json:"import_timeout_s"`
	KeepCheckouts   int               `json:"keep_checkouts"`
	Repos           []build.Repo      `json:"repos"`
	HeartbeatS      int               `json:"heartbeat_s"`
	LaunchTimeoutS  int               `json:"launch_timeout_s"`
	ProgressEveryMs int               `json:"progress_every_ms"`
	// DEV RIGS ONLY (task 01a0fe45 stage 5); prod config.json sets neither.
	// LogTouch logs every st/sd line sent to the addon. Features, when
	// present, replaces client_info.features: ["game_stream_host",
	// "game_stream_multi"] runs a host WITHOUT game_stream_touch, the
	// negative control for core's touch gate. Absent = the default set.
	LogTouch bool     `json:"log_touch"`
	Features []string `json:"features"`
}

// LoadConfig reads and checks the config file.
func LoadConfig(path string) (FileConfig, error) {
	var c FileConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	resolveBurrowURL(&c, path)
	if c.Encoder == "" {
		c.Encoder = media.EncoderAuto
	}
	if err := c.Encoder.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.RenderingDriver == "" {
		c.RenderingDriver = launch.DefaultRenderingDriver(runtime.GOOS)
	}
	if c.UDPMin == 0 {
		c.UDPMin, c.UDPMax = 40300, 40309
	}
	if c.GodotPort == 0 {
		c.GodotPort = 40320
	}
	if c.RTPPort == 0 {
		c.RTPPort = 40330
	}
	if c.AudioRTPPort == 0 {
		c.AudioRTPPort = c.RTPPort + 1
	}
	for name, v := range map[string]string{"burrow_url": c.BurrowURL, "runtime_id": c.RuntimeID, "token_file": c.TokenFile,
		"godot": c.Godot, "ffmpeg": c.FFmpeg, "git": c.Git, "mirrors_dir": c.MirrorsDir, "checkouts_dir": c.CheckoutsDir,
		"addon_dir": c.AddonDir, "logs_dir": c.LogsDir} {
		if v == "" {
			return c, fmt.Errorf("%s: %s is required", path, name)
		}
	}
	if len(c.Repos) == 0 {
		return c, fmt.Errorf("%s: repos is empty; the host would refuse every session", path)
	}
	return c, nil
}

// resolveBurrowURL applies the suite_url fallback: suite_url alone fills
// burrow_url with one deprecation line; both set and different, burrow_url
// wins and the ignored value is logged once.
func resolveBurrowURL(c *FileConfig, path string) {
	switch {
	case c.SuiteURL == "":
	case c.BurrowURL == "":
		c.BurrowURL = c.SuiteURL
		log.Printf("config %s: suite_url is deprecated; rename it to burrow_url", path)
	case c.BurrowURL != c.SuiteURL:
		log.Printf("config %s: burrow_url and suite_url differ; using burrow_url, ignoring suite_url", path)
	}
}

func selectEncoderForOS(requested media.EncoderKind, goos string) media.EncoderKind {
	if requested != "" && requested != media.EncoderAuto {
		return requested
	}
	switch goos {
	case "windows":
		return media.EncoderNVENC
	case "darwin":
		return media.EncoderVideoToolbox
	default:
		return media.EncoderLibx264
	}
}

func resolveExecutable(name, configured string) (string, error) {
	clean := filepath.Clean(configured)
	resolved, err := exec.LookPath(clean)
	if err != nil {
		return "", fmt.Errorf("%s executable %q: %w", name, configured, err)
	}
	if filepath.IsAbs(resolved) {
		return resolved, nil
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("%s executable %q: %w", name, configured, err)
	}
	return resolved, nil
}

func (c *FileConfig) resolveExecutables() error {
	var err error
	c.Godot = launch.GodotExecutable(c.Godot, runtime.GOOS)
	for name, value := range map[string]*string{"godot": &c.Godot, "ffmpeg": &c.FFmpeg, "git": &c.Git, "ssh": &c.SSH} {
		if *value == "" {
			continue
		}
		if *value, err = resolveExecutable(name, *value); err != nil {
			return err
		}
	}
	return nil
}

func (c FileConfig) hostConfig() host.Config {
	return host.Config{
		Godot: c.Godot, RenderingDriver: c.RenderingDriver,
		FFmpeg: c.FFmpeg, Encoder: selectEncoderForOS(c.Encoder, runtime.GOOS), EncoderFallback: c.EncoderFallback, LogsDir: c.LogsDir, HostIP: c.HostIP, UDPMin: c.UDPMin, UDPMax: c.UDPMax,
		GodotPort: c.GodotPort, RTPPort: c.RTPPort, AudioRTPPort: c.AudioRTPPort,
		ImportTimeout: time.Duration(c.ImportTimeoutS) * time.Second,
		LinkTimeout:   time.Duration(c.LaunchTimeoutS) * time.Second,
		ProgressEvery: time.Duration(c.ProgressEveryMs) * time.Millisecond,
		LogTouch:      c.LogTouch,
		Build: build.Config{Git: c.Git, SSH: c.SSH, KnownHosts: c.KnownHosts, MirrorsDir: c.MirrorsDir,
			CheckoutsDir: c.CheckoutsDir, AddonDir: c.AddonDir, Repos: c.Repos, KeepCheckouts: c.KeepCheckouts},
	}
}

func serve(args []string) error {
	if len(args) != 2 || args[0] != "-config" {
		return fmt.Errorf("usage: play-host serve -config <config.json>")
	}
	c, err := LoadConfig(args[1])
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
	log.Printf("play-host %s serve: runtime %s via %s", Version, c.RuntimeID, c.BurrowURL)
	if c.LogTouch || c.Features != nil {
		log.Printf("play-host: dev config: log_touch=%v features=%v", c.LogTouch, c.Features)
	}

	hc := c.hostConfig()
	hc.PionLog = log.Writer()
	h := host.New(hc, nil, nil, log.Printf)
	sweepCtx, sweepDone := context.WithTimeout(context.Background(), 2*time.Minute)
	h.SweepCheckouts(sweepCtx)
	sweepDone()
	cl := suite.New(suite.Config{URL: c.BurrowURL, RuntimeID: c.RuntimeID, TokenFile: c.TokenFile, Product: "play-host",
		Version: Version, Heartbeat: time.Duration(c.HeartbeatS) * time.Second, Features: c.Features}, h)
	h.SetSender(cl)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cl.Run(ctx)
	if s := h.Current(); s != nil {
		h.Shutdown("play-host stopping")
		time.Sleep(2 * time.Second)
	}
	return nil
}

// setupLogging opens <logs>/play-host.log for the standard logger AND for
// the Go runtime's crash output (stage 6). The scheduled task that runs
// play-host does not redirect stderr, so before this an unrecovered panic
// on any goroutine killed the process (exit code 2) with its message and
// stack written nowhere. debug.SetCrashOutput makes the runtime copy every
// fatal panic, with all goroutine stacks, into the log file as well.
func setupLogging(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(filepath.Join(dir, "play-host.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(os.Stderr, lf))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
	if err := debug.SetCrashOutput(lf, debug.CrashOptions{}); err != nil {
		log.Printf("play-host: crash output not redirected: %v", err)
	}
	return lf, nil
}

// logPanic is the main goroutine's deferred recover: it logs the panic and
// its stack, then exits 2 as an unrecovered panic would, so the scheduled
// task still sees a failure. Panics on other goroutines are covered by
// SetCrashOutput (and the session's own guards turn most of them into a
// failed session instead).
func logPanic() {
	if r := recover(); r != nil {
		log.Printf("play-host: PANIC: %v\n%s", r, debug.Stack())
		os.Exit(2)
	}
}

func keynames() error {
	return json.NewEncoder(os.Stdout).Encode(input.KeyNames())
}
