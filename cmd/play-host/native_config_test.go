package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeConfigNeedsNoEncoderOrEngineInstall(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	data := `{"burrow_url":"wss://example/runtime/ws","runtime_id":"native","token_file":"/private/runtime","logs_dir":"/logs","native":{"enabled":true,"state_dir":"/private/native","public_host":"native.example","admission_url":"https://example/api/ext/play/admit","images":["registry/game@sha256:` + strings.Repeat("a", 64) + `"],"ports":[24973]}}`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(p)
	if e != nil {
		t.Fatal(e)
	}
	if !c.Native.Enabled || c.Native.RuntimeID != "native" || c.FFmpeg != "" || c.Godot != "" {
		t.Fatal("native initialized stream dependencies")
	}
}
