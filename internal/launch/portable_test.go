package launch

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDefaultRenderingDriver(t *testing.T) {
	if got := DefaultRenderingDriver("darwin"); got != "" {
		t.Fatalf("darwin driver = %q", got)
	}
	for _, goos := range []string{"windows", "linux"} {
		if got := DefaultRenderingDriver(goos); got != "vulkan" {
			t.Errorf("%s driver = %q", goos, got)
		}
	}
}

func TestGodotExecutablePortability(t *testing.T) {
	macBundle := filepath.Join(string(filepath.Separator), "Applications", "Godot.app")
	wantMac := filepath.Join(macBundle, "Contents", "MacOS", "Godot")
	cases := []struct {
		name, path, goos, want string
	}{
		{"mac app bundle", macBundle, "darwin", wantMac},
		{"mac expanded", wantMac, "darwin", wantMac},
		{"path lookup", "godot", "darwin", "godot"},
		{"windows exe", "C:\\Program Files\\Godot\\Godot_v4.6-stable_win64_console.exe", "windows", "C:\\Program Files\\Godot\\Godot_v4.6-stable_win64_console.exe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GodotExecutable(tc.path, tc.goos); got != tc.want {
				t.Fatalf("GodotExecutable(%q, %q) = %q, want %q", tc.path, tc.goos, got, tc.want)
			}
		})
	}
}

func TestGodotArgsOmitsEmptyRenderingDriver(t *testing.T) {
	got := GodotArgsForDriver("/tmp/game", "", 1280, 720, "s", 40320, 60)
	if slices.Contains(got, "--rendering-driver") {
		t.Fatalf("Darwin/default args forced a rendering driver: %#v", got)
	}
}

func TestGodotArgsForDriverPreservesPathElement(t *testing.T) {
	dir := "/Users/Shared/Suite Projects/game one"
	got := GodotArgsForDriver(dir, "metal", 1920, 1080, "mac-session", 40320, 60)
	wantPrefix := []string{"--path", dir, "--rendering-driver", "metal", "--resolution", "1920x1080", "--windowed"}
	if !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("args prefix = %#v", got)
	}
	joined := strings.Join(got, " ")
	for _, marker := range []string{"-- --burrow-play-session=mac-session", "--burrow-play-port=40320", "--burrow-play-fps=60", "--suite-play-session=mac-session"} {
		if !strings.Contains(joined, marker) {
			t.Errorf("missing %q in %q", marker, joined)
		}
	}
}
