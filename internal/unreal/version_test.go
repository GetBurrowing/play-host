package unreal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEngineVersion(t *testing.T) {
	root := t.TempDir()
	if _, err := EngineVersion(root); err == nil {
		t.Fatal("missing Build.version must fail")
	}
	dir := filepath.Join(root, "Engine", "Build")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Build.version"), []byte(`{"MajorVersion":5,"MinorVersion":4,"PatchVersion":2,"Changelist":1}`), 0o644)
	if v, err := EngineVersion(root); err != nil || v != "5.4.2" {
		t.Fatalf("%q %v", v, err)
	}
	os.WriteFile(filepath.Join(dir, "Build.version"), []byte(`{"MajorVersion":5}`), 0o644)
	if _, err := EngineVersion(root); err == nil {
		t.Fatal("a partial version must fail")
	}
}
