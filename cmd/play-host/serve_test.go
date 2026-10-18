package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Task 01a0fe45 stage 5: log_touch and features are dev-rig keys. A config
// without them (host-a's prod config.json) must load exactly as before:
// LogTouch off and Features nil, which suite.Client reads as its default set.
func TestDevRigKeysDefaultOffAndParse(t *testing.T) {
	base := `"burrow_url":"ws://x/runtime/ws","runtime_id":"r","token_file":"t","godot":"g","ffmpeg":"f","git":"git",
	"mirrors_dir":"m","checkouts_dir":"c","addon_dir":"a","logs_dir":"l","repos":[{"match":"github.com/example/x","clone_url":"y"}]`
	load := func(extra string) FileConfig {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config-01a0fe45.json")
		if err := os.WriteFile(p, []byte("{"+base+extra+"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := load("")
	if c.LogTouch || c.Features != nil || c.hostConfig().LogTouch {
		t.Fatalf("absent keys: log_touch=%v features=%#v", c.LogTouch, c.Features)
	}

	c = load(`,"log_touch":true,"features":["game_stream_host","game_stream_multi"]`)
	if !c.LogTouch || !c.hostConfig().LogTouch {
		t.Fatalf("log_touch not carried to host.Config")
	}
	if want := []string{"game_stream_host", "game_stream_multi"}; !reflect.DeepEqual(c.Features, want) {
		t.Fatalf("features = %#v, want %#v", c.Features, want)
	}
}
