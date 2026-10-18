package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog collects what LoadConfig logs while fn runs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	fn()
	return buf.String()
}

func writeRawConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const urlTestRest = `"runtime_id":"r","token_file":"t","godot":"g","ffmpeg":"f","git":"git",
	"mirrors_dir":"m","checkouts_dir":"c","addon_dir":"a","logs_dir":"l","repos":[{"match":"github.com/example/x","clone_url":"y"}]`

func TestBurrowURLOnly(t *testing.T) {
	var c FileConfig
	var err error
	out := captureLog(t, func() {
		c, err = LoadConfig(writeRawConfig(t, `{"burrow_url":"wss://new.example/runtime/ws",`+urlTestRest+`}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.BurrowURL != "wss://new.example/runtime/ws" {
		t.Fatalf("burrow_url = %q", c.BurrowURL)
	}
	if out != "" {
		t.Fatalf("burrow_url alone logged %q", out)
	}
}

func TestSuiteURLOnlyFallsBackAndWarnsOnce(t *testing.T) {
	var c FileConfig
	var err error
	out := captureLog(t, func() {
		c, err = LoadConfig(writeRawConfig(t, `{"suite_url":"wss://old.example/runtime/ws",`+urlTestRest+`}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.BurrowURL != "wss://old.example/runtime/ws" {
		t.Fatalf("burrow_url = %q, want the suite_url value", c.BurrowURL)
	}
	if n := strings.Count(out, "\n"); n != 1 || !strings.Contains(out, "suite_url is deprecated") {
		t.Fatalf("want one deprecation line, got %d: %q", n, out)
	}
}

func TestBothURLsBurrowWins(t *testing.T) {
	var c FileConfig
	var err error
	out := captureLog(t, func() {
		c, err = LoadConfig(writeRawConfig(t, `{"suite_url":"wss://old.example/runtime/ws","burrow_url":"wss://new.example/runtime/ws",`+urlTestRest+`}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.BurrowURL != "wss://new.example/runtime/ws" {
		t.Fatalf("burrow_url = %q, want burrow_url to win", c.BurrowURL)
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "using burrow_url") {
		t.Fatalf("want one line naming the winner, got %q", out)
	}
	// Both set to the same value is not a conflict and says nothing.
	out = captureLog(t, func() {
		_, err = LoadConfig(writeRawConfig(t, `{"suite_url":"wss://same.example/runtime/ws","burrow_url":"wss://same.example/runtime/ws",`+urlTestRest+`}`))
	})
	if err != nil || out != "" {
		t.Fatalf("equal urls: err=%v log=%q", err, out)
	}
}

func TestNeitherURLIsRejectedNamingBurrowURL(t *testing.T) {
	_, err := LoadConfig(writeRawConfig(t, `{`+urlTestRest+`}`))
	if err == nil || !strings.Contains(err.Error(), "burrow_url is required") {
		t.Fatalf("err = %v", err)
	}
}

// The running play hosts' config.json files predate burrow_url. This is the
// shape they have (the documented macOS layout and the Windows layout), with
// placeholder hosts and paths; it must load with zero edits.
func TestDeployedConfigShapesLoadUnchanged(t *testing.T) {
	mac := `{
  "suite_url": "wss://suite.example/runtime/ws",
  "runtime_id": "play-host-mac",
  "token_file": "/Users/Shared/play-host/secrets/runtime-token",
  "godot": "/Applications/Godot.app",
  "rendering_driver": "",
  "ffmpeg": "/opt/homebrew/bin/ffmpeg",
  "encoder": "videotoolbox",
  "encoder_fallback": true,
  "git": "/opt/homebrew/bin/git",
  "ssh": "/usr/bin/ssh",
  "known_hosts": "/Users/Shared/play-host/known_hosts",
  "mirrors_dir": "/Users/Shared/play-host/mirrors",
  "checkouts_dir": "/Users/Shared/play-host/checkouts",
  "addon_dir": "/Users/Shared/play-host/suite_play",
  "logs_dir": "/Users/Shared/play-host/logs",
  "host_ip": "",
  "udp_min": 40300,
  "udp_max": 40309,
  "godot_port": 40320,
  "rtp_port": 40330,
  "audio_rtp_port": 40331,
  "import_timeout_s": 1200,
  "keep_checkouts": 3,
  "heartbeat_s": 10,
  "launch_timeout_s": 90,
  "progress_every_ms": 5000,
  "repos": [
    {"match": "github.com/example/game", "clone_url": "git@github.com:example/game.git", "ssh_key": "/Users/Shared/play-host/secrets/deploy-key"}
  ]
}`
	win := `{
  "suite_url": "wss://burrow.example.com/runtime/ws",
  "runtime_id": "play-host-a",
  "token_file": "C:\\play-host\\secrets\\runtime-token",
  "godot": "C:\\Godot\\Godot_v4.7.2-stable_win64_console.exe",
  "ffmpeg": "C:\\ffmpeg\\bin\\ffmpeg.exe",
  "git": "C:\\Program Files\\Git\\cmd\\git.exe",
  "ssh": "C:\\Program Files\\Git\\usr\\bin\\ssh.exe",
  "known_hosts": "C:\\play-host\\known_hosts",
  "mirrors_dir": "C:\\play-host\\mirrors",
  "checkouts_dir": "C:\\play-host\\checkouts",
  "addon_dir": "C:\\play-host\\addons\\suite_play",
  "logs_dir": "C:\\play-host\\logs",
  "host_ip": "<host-a>",
  "repos": [
    {"match": "github.com/example-owner/example-game", "clone_url": "git@github.com:example-owner/example-game.git", "ssh_key": "C:\\play-host\\secrets\\deploy-key"}
  ]
}`
	for name, body := range map[string]string{"mac": mac, "windows": win} {
		var c FileConfig
		var err error
		out := captureLog(t, func() { c, err = LoadConfig(writeRawConfig(t, body)) })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.BurrowURL == "" || c.BurrowURL != c.SuiteURL {
			t.Fatalf("%s: burrow_url %q not filled from suite_url %q", name, c.BurrowURL, c.SuiteURL)
		}
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("%s: want one deprecation line, got %q", name, out)
		}
	}
}
