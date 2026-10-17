package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Startup-Suite/play-host/internal/media"
)

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"burrow_url":"wss://burrow.example/runtime/ws","runtime_id":"play-host-test","token_file":"token","godot":"godot","ffmpeg":"ffmpeg","git":"git","mirrors_dir":"mirrors","checkouts_dir":"checkouts","addon_dir":"addon","logs_dir":"logs","repos":[{"match":"example","url":"git@example/repo.git"}]` + extra + `}`
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
