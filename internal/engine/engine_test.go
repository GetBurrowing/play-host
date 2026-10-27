package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSelectEngineGodot(t *testing.T) {
	d := t.TempDir()
	touch(t, d, "project.godot")
	if k, err := Detect(d); err != nil || k != Godot {
		t.Fatalf("got %q, %v", k, err)
	}
}

func TestSelectEngineUnreal(t *testing.T) {
	d := t.TempDir()
	touch(t, d, "Sample.uproject")
	if k, err := Detect(d); err != nil || k != Unreal {
		t.Fatalf("got %q, %v", k, err)
	}
}

func TestSelectEngineBothOrNeitherFails(t *testing.T) {
	both := t.TempDir()
	touch(t, both, "project.godot")
	touch(t, both, "Sample.uproject")
	neither := t.TempDir()
	two := t.TempDir()
	touch(t, two, "A.uproject")
	touch(t, two, "B.uproject")
	for name, d := range map[string]string{"both": both, "neither": neither, "two uprojects": two} {
		k, err := Detect(d)
		if err == nil || k != "" {
			t.Errorf("%s: got %q, %v", name, k, err)
		}
		if err != nil && err.Error() != "checkout has both/neither project.godot and .uproject" {
			t.Errorf("%s: message %q", name, err)
		}
	}
}
