package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/engine"
	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/rtc"
)

// captureProc is an Unreal game with no addon link.
type captureProc struct {
	done chan struct{}
	once sync.Once
}

func (p *captureProc) Pid() int             { return 4343 }
func (p *captureProc) Wait() (int, error)   { <-p.done; return 1, nil }
func (p *captureProc) Kill() error          { p.once.Do(func() { close(p.done) }); return nil }
func (p *captureProc) Pids() ([]int, error) { return []int{4343}, nil }

type capEnv struct{}

func (capEnv) SessionID() (uint32, error)     { return 1, nil }
func (capEnv) ActiveConsoleSessionID() uint32 { return 1 }

type capFinder struct{ ws []capture.Window }

func (f capFinder) Windows([]int) ([]capture.Window, error) { return f.ws, nil }

type capDesktop struct{}

func (capDesktop) Monitors() ([]capture.Rect, error) {
	return []capture.Rect{{X: 0, Y: 0, W: 1920, H: 1080}}, nil
}

// unrealStages is an Unreal checkout whose capture is built by capture.Start
// over fakes, with an optional frame-pushing source.
type unrealStages struct {
	t       *testing.T
	windows []capture.Window
	pushed  *capture.Pushed
	removed atomic.Bool
}

func (u *unrealStages) Prepare(ctx context.Context, s protocol.SessionStart, progress func(string)) (string, error) {
	dir := filepath.Join(u.t.TempDir(), "checkout")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Sample.uproject"), []byte("{}"), 0o644)
	return dir, nil
}
func (u *unrealStages) AssertHead(context.Context, string, string) error { return nil }
func (u *unrealStages) Remove(_ context.Context, _ protocol.SessionStart, dir string) error {
	u.removed.Store(true)
	return os.RemoveAll(dir)
}
func (u *unrealStages) Launch(string, protocol.SessionStart, string) (launch.Proc, error) {
	return &captureProc{done: make(chan struct{})}, nil
}
func (u *unrealStages) Engine(dir string) (engine.Kind, error) { return engine.Detect(dir) }
func (u *unrealStages) Capture(ctx context.Context, p launch.Proc, s protocol.SessionStart) (capture.Source, error) {
	cfg := capture.Config{
		Backend: capture.BackendWGC, FPS: s.Encoder.FPS, Timeout: 150 * time.Millisecond, Poll: 5 * time.Millisecond,
		Pids: p.Pids, Env: capEnv{}, Finder: capFinder{u.windows}, Desktop: capDesktop{},
		NewWGC: func(capture.Window, capture.Rect) (capture.Source, error) { return u.pushed, nil },
	}
	return capture.Start(ctx, cfg)
}

var theWindow = []capture.Window{{Handle: 1, Visible: true, Client: capture.Rect{X: 0, Y: 0, W: 64, H: 48}}}

func newCaptureHost(t *testing.T, port int, u *unrealStages, mod func(*Config)) (*Host, *core) {
	base, c := newTestHost(t, port, nil)
	cfg := base.cfg
	if mod != nil {
		mod(&cfg)
	}
	return New(cfg, c, u, t.Logf), c
}

// N frames pushed by a fake frame-pushing source reach the pipeline and the
// session goes live.
func TestFakeWindowStream(t *testing.T) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	var sent atomic.Int64
	u := &unrealStages{t: t, windows: theWindow}
	u.pushed = &capture.Pushed{Label: "fake", Run: func(ctx context.Context, deliver capture.FrameFunc) error {
		frame := make([]byte, 64*48*4)
		for ctx.Err() == nil {
			if err := deliver(frame, 64, 48); err != nil {
				return err
			}
			sent.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}}
	h, c := newCaptureHost(t, 40424, u, nil)
	h.OnEvent(protocol.EventSessionStart, startPayload("u1", 600))
	offer := <-c.offer
	pc, _, pkts := viewer(t, h, offer)
	defer pc.Close()
	eventually(t, "live", func() bool { return c.last("u1").State == protocol.StateLive })
	eventually(t, "video packets at the viewer", func() bool { return pkts.Load() > 3 })
	if !strings.Contains(c.last("u1").Detail, "64x48") {
		t.Errorf("live detail %q", c.last("u1").Detail)
	}
	b, _ := json.Marshal(protocol.SessionStop{SessionID: "u1", Reason: "done"})
	h.OnEvent(protocol.EventSessionStop, b)
	eventually(t, "ended", func() bool { return c.last("u1").State == protocol.StateEnded })
	eventually(t, "checkout removed", u.removed.Load)
}

func TestHostWindowNotFoundFailsSession(t *testing.T) {
	u := &unrealStages{t: t} // no windows ever
	h, c := newCaptureHost(t, 40426, u, nil)
	h.OnEvent(protocol.EventSessionStart, startPayload("u2", 600))
	eventually(t, "failed", func() bool { return c.last("u2").State == protocol.StateFailed })
	if got := c.last("u2").Detail; got != "game window not found" {
		t.Fatalf("detail %q", got)
	}
}

type fakeKeepAwake struct{ started, stopped atomic.Bool }

func (k *fakeKeepAwake) Start() error { k.started.Store(true); return nil }
func (k *fakeKeepAwake) Stop()        { k.stopped.Store(true) }

// A source that emits nothing must fail the session, never hang it; the
// display is held awake while it runs and released after.
func TestCaptureStallFailsSession(t *testing.T) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	ka := &fakeKeepAwake{}
	u := &unrealStages{t: t, windows: theWindow}
	u.pushed = &capture.Pushed{Label: "silent", Run: func(ctx context.Context, _ capture.FrameFunc) error { <-ctx.Done(); return nil }}
	h, c := newCaptureHost(t, 40428, u, func(cfg *Config) {
		cfg.FrameStallTimeout = 300 * time.Millisecond
		cfg.KeepAwake = ka
	})
	h.OnEvent(protocol.EventSessionStart, startPayload("u3", 600))
	offer := <-c.offer
	pc, _, _ := viewer(t, h, offer)
	defer pc.Close()
	eventually(t, "failed", func() bool { return c.last("u3").State == protocol.StateFailed })
	if got := c.last("u3").Detail; got != "capture stalled: no frames (display asleep?)" {
		t.Fatalf("detail %q", got)
	}
	if !ka.started.Load() {
		t.Error("keep-awake was not started")
	}
	eventually(t, "keep-awake released", ka.stopped.Load)
}
