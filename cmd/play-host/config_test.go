package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Startup-Suite/play-host/internal/media"
)

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"burrow_url":"wss://burrow.example/runtime/ws","runtime_id":"play-host-test","token_file":"token","godot":"godot","ffmpeg":"ffmpeg","git":"git","mirrors_dir":"mirrors","checkouts_dir":"checkouts","addon_dir":"addon","logs_dir":"logs","repos":[{"match":"github.com/example/repo","url":"git@example/repo.git"}]` + extra + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigPortableDefaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Encoder != media.EncoderAuto {
		t.Fatalf("encoder = %q", c.Encoder)
	}
	if got := selectEncoderForOS(c.Encoder, "windows"); got != media.EncoderNVENC {
		t.Fatalf("Windows auto = %q", got)
	}
	if got := selectEncoderForOS(c.Encoder, "darwin"); got != media.EncoderVideoToolbox {
		t.Fatalf("Darwin auto = %q", got)
	}
	if got := selectEncoderForOS(c.Encoder, "linux"); got != media.EncoderLibx264 {
		t.Fatalf("Linux auto = %q", got)
	}
	if c.UDPMin != 40300 || c.UDPMax != 40309 || c.GodotPort != 40320 || c.RTPPort != 40330 {
		t.Fatalf("port defaults: %+v", c)
	}
}

func TestLoadConfigExplicitEncoderAndDriver(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `,"encoder":"videotoolbox","encoder_fallback":true,"rendering_driver":"metal"`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Encoder != media.EncoderVideoToolbox || !c.EncoderFallback || c.RenderingDriver != "metal" {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadConfigRejectsUnknownEncoder(t *testing.T) {
	if _, err := LoadConfig(writeConfig(t, `,"encoder":"magic"`)); err == nil {
		t.Fatal("unknown encoder accepted")
	}
}

// A match without host/owner/name made the startup sweep panic in
// Config.MirrorDir; LoadConfig now refuses it and names the entry.
func TestLoadConfigRejectsRepoMatchWithoutOwnerAndName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"burrow_url":"wss://burrow.example/runtime/ws","runtime_id":"r","token_file":"t","godot":"g","ffmpeg":"f","git":"git","mirrors_dir":"m","checkouts_dir":"c","addon_dir":"a","logs_dir":"l","repos":[{"match":"github.com/example/ok","clone_url":"y"},{"match":"example","clone_url":"y"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "repos[1].match") {
		t.Fatalf("want a repos[1].match error, got %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, "")); err != nil {
		t.Fatalf("a valid match was refused: %v", err)
	}
}
