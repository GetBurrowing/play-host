package capture

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeEnv struct {
	id     uint32
	active uint32
}

func (e fakeEnv) SessionID() (uint32, error)     { return e.id, nil }
func (e fakeEnv) ActiveConsoleSessionID() uint32 { return e.active }

type fakeFinder struct {
	mu sync.Mutex
	ws []Window
}

func (f *fakeFinder) Windows(pids []int) ([]Window, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Window(nil), f.ws...), nil
}

type fakeDesktop []Rect

func (d fakeDesktop) Monitors() ([]Rect, error) { return d, nil }

func fakeConfig(env Env, ws ...Window) Config {
	return Config{
		FPS: 60, Timeout: 200 * time.Millisecond, Poll: 5 * time.Millisecond,
		Pids: func() ([]int, error) { return []int{4242}, nil },
		Env:  env, Finder: &fakeFinder{ws: ws},
		Desktop: fakeDesktop{{0, 0, 1920, 1080}},
	}
}

func TestCaptureRefusesSession0(t *testing.T) {
	cfg := fakeConfig(fakeEnv{id: 0, active: 0}, Window{Visible: true, Client: Rect{0, 0, 1280, 720}})
	src, err := Start(context.Background(), cfg)
	if err == nil || src != nil || err.Error() != "capture refused: session 0" {
		t.Fatalf("got %v, %v", src, err)
	}
	// A desktop session that is not the active console session is refused too.
	cfg = fakeConfig(fakeEnv{id: 2, active: 1}, Window{Visible: true, Client: Rect{0, 0, 1280, 720}})
	if _, err := Start(context.Background(), cfg); err == nil || !strings.HasPrefix(err.Error(), "capture refused:") {
		t.Fatalf("non-console session: %v", err)
	}
	// And the allowed case works, so the refusals are not vacuous.
	cfg = fakeConfig(fakeEnv{id: 1, active: 1}, Window{Visible: true, Client: Rect{0, 0, 1280, 720}})
	if _, err := Start(context.Background(), cfg); err != nil {
		t.Fatalf("session 1 on console: %v", err)
	}
}

func TestDDAGrabArgsCropToClientRect(t *testing.T) {
	cfg := fakeConfig(fakeEnv{1, 1}, Window{Visible: true, Client: Rect{100, 50, 1281, 721}})
	src, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(src.InputArgs(0, 0, 60), " ")
	for _, want := range []string{"ddagrab=output_idx=0:", "framerate=60", "offset_x=100:offset_y=50:video_size=1280x720"} {
		if !strings.Contains(args, want) {
			t.Errorf("%q missing from %q", want, args)
		}
	}
	if w, h := src.Size(); w != 1280 || h != 720 || !src.Native() {
		t.Errorf("size %dx%d native=%v", w, h, src.Native())
	}
}

func TestWindowNotFoundFails(t *testing.T) {
	// Only an invisible window and an empty one exist: neither counts.
	cfg := fakeConfig(fakeEnv{1, 1}, Window{Visible: false, Client: Rect{0, 0, 800, 600}}, Window{Visible: true})
	if _, err := Start(context.Background(), cfg); !errors.Is(err, ErrWindowNotFound) || err.Error() != "game window not found" {
		t.Fatalf("got %v", err)
	}
}

func TestLargestVisibleWindowWins(t *testing.T) {
	w, ok := Largest([]Window{
		{Handle: 1, Visible: true, Client: Rect{0, 0, 200, 100}},
		{Handle: 2, Visible: true, Client: Rect{0, 0, 1280, 720}},
		{Handle: 3, Visible: false, Client: Rect{0, 0, 3000, 2000}},
	})
	if !ok || w.Handle != 2 {
		t.Fatalf("got %+v %v", w, ok)
	}
}

// A virtual display (for example a remote-play adapter) adds outputs; the
// output must be the one holding the window, with offsets local to it.
func TestOutputForPicksMonitorHoldingWindow(t *testing.T) {
	mons := []Rect{{0, 0, 1920, 1080}, {1920, 0, 1920, 1080}, {3840, 0, 1280, 720}}
	idx, local, err := OutputFor(Rect{2020, 40, 1280, 720}, mons)
	if err != nil || idx != 1 || local != (Rect{100, 40, 1280, 720}) {
		t.Fatalf("got %d %+v %v", idx, local, err)
	}
	cfg := fakeConfig(fakeEnv{1, 1}, Window{Visible: true, Client: Rect{2020, 40, 1280, 720}})
	cfg.Desktop = fakeDesktop(mons)
	src, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.InputArgs(0, 0, 60), " "); !strings.Contains(got, "output_idx=1:") || !strings.Contains(got, "offset_x=100:offset_y=40") {
		t.Fatalf("args %q", got)
	}
	if _, _, err := OutputFor(Rect{9000, 9000, 10, 10}, mons); err == nil {
		t.Fatal("off-screen window should fail")
	}
}

func TestBackendValidate(t *testing.T) {
	for _, b := range []Backend{"", BackendDDAGrab, BackendWGC} {
		if err := b.Validate(); err != nil {
			t.Errorf("%q: %v", b, err)
		}
	}
	if err := Backend("gdigrab").Validate(); err == nil {
		t.Error("gdigrab should be rejected")
	}
}

func TestKeepAwakeSetsAndClears(t *testing.T) {
	var mu sync.Mutex
	var calls []uint32
	ka := NewKeepAwakeWith(func(f uint32) bool {
		mu.Lock()
		calls = append(calls, f)
		mu.Unlock()
		return true
	})
	if err := ka.Start(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0] != ESContinuous|ESDisplayRequired|ESSystemRequired {
		t.Fatalf("after Start: %#x", calls)
	}
	mu.Unlock()
	ka.Stop()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[1] != ESContinuous {
		t.Fatalf("after Stop: %#x", calls)
	}
}

func TestKeepAwakeFailureSurfaces(t *testing.T) {
	ka := NewKeepAwakeWith(func(uint32) bool { return false })
	if err := ka.Start(); err == nil {
		t.Fatal("a failed SetThreadExecutionState must fail Start")
	}
	ka.Stop()
}
