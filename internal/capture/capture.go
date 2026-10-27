// Package capture supplies video to the encoder for engines that have no
// in-engine readback (Unreal). There are two shapes of Source:
//
//   - ffmpeg-native (ddagrab): the GPU grabs the desktop and D3D11 frames go
//     straight to h264_nvenc, so Go never sees pixels. InputArgs is the ffmpeg
//     input and nothing is written to the pipeline's stdin.
//   - frame-pushing (Godot's link readback, Windows.Graphics.Capture): the
//     source hands RGBA frames to the host, which writes them to the existing
//     rawvideo stdin path (Pipeline.WriteFrame).
//
// Capture is for the logged-in desktop session only; Start refuses session 0.
package capture

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Startup-Suite/play-host/internal/media"
)

// Backend names the capture implementation. It is the experiment surface
// config key capture_backend.
type Backend string

const (
	BackendDDAGrab Backend = "ddagrab"
	BackendWGC     Backend = "wgc"
)

// DefaultBackend is chosen by the Oct 29 decision recorded in the runbook.
const DefaultBackend = BackendDDAGrab

// DefaultWindowTimeout is how long Start waits for the game window.
const DefaultWindowTimeout = 120 * time.Second

// Validate reports whether b is a known backend ("" means the default).
func (b Backend) Validate() error {
	switch b {
	case "", BackendDDAGrab, BackendWGC:
		return nil
	}
	return fmt.Errorf("capture_backend %q: want ddagrab or wgc", string(b))
}

// FrameFunc receives one RGBA frame from a frame-pushing source.
type FrameFunc func(rgba []byte, w, h int) error

// Source is one video source for a session.
type Source interface {
	media.InputSource
	// Name is the backend, for logs.
	Name() string
	// Native reports an ffmpeg-native source: ffmpeg grabs the pixels itself.
	Native() bool
	// Size is the encoded frame size of a native source (0, 0 when frames are
	// pushed; the size then comes with the first frame).
	Size() (w, h int)
	// Frames pushes frames to deliver until ctx ends or deliver fails. Native
	// sources return immediately.
	Frames(ctx context.Context, deliver FrameFunc) error
}

// Rect is a rectangle in pixels.
type Rect struct{ X, Y, W, H int }

func (r Rect) area() int { return r.W * r.H }

// Window is a candidate game window.
type Window struct {
	Handle  uintptr
	Visible bool
	Client  Rect // client area in screen coordinates
}

// WindowFinder lists the top-level windows owned by the given processes. The
// windows implementation is EnumWindows + GetWindowThreadProcessId; tests use
// a fake, so no desktop is needed.
type WindowFinder interface {
	Windows(pids []int) ([]Window, error)
}

// Desktop is the DXGI view of the displays: the monitor rectangles, in
// ddagrab output_idx order, in screen coordinates.
type Desktop interface {
	Monitors() ([]Rect, error)
}

// Env is the session identity the guard checks.
type Env interface {
	// SessionID is ProcessIdToSessionId for this process.
	SessionID() (uint32, error)
	// ActiveConsoleSessionID is WTSGetActiveConsoleSessionId.
	ActiveConsoleSessionID() uint32
}

// KeepAwake holds the display and system awake for the life of a capture
// session: Desktop Duplication delivers nothing when the display sleeps. The
// windows implementation calls SetThreadExecutionState on a dedicated locked
// OS thread (the state is per thread) and never touches powercfg.
type KeepAwake interface {
	Start() error
	Stop()
}

// Guard enforces constraint (a): desktop session only, never session 0.
func Guard(env Env) error {
	id, err := env.SessionID()
	if err != nil {
		return fmt.Errorf("capture refused: session id: %w", err)
	}
	if id == 0 {
		return errors.New("capture refused: session 0")
	}
	if active := env.ActiveConsoleSessionID(); id != active {
		return fmt.Errorf("capture refused: session %d is not the active console session %d", id, active)
	}
	return nil
}

// ErrWindowNotFound is the failure when no game window appears in time.
var ErrWindowNotFound = errors.New("game window not found")

// Config is everything Start needs.
type Config struct {
	Backend Backend
	FPS     int
	Timeout time.Duration // 0 = DefaultWindowTimeout
	Poll    time.Duration // 0 = 250 ms
	// Pids returns the launched Job Object's current PIDs.
	Pids    func() ([]int, error)
	Env     Env
	Finder  WindowFinder
	Desktop Desktop
	// NewWGC builds the frame-pushing backend (windows build only); nil means
	// wgc is not available.
	NewWGC func(w Window, mon Rect) (Source, error)
}

// Start runs the session-0 guard, waits for the game window, and returns the
// Source for it. It is the only place a non-Godot session obtains video.
func Start(ctx context.Context, cfg Config) (Source, error) {
	if err := Guard(cfg.Env); err != nil {
		return nil, err
	}
	backend := cfg.Backend
	if backend == "" {
		backend = DefaultBackend
	}
	if err := backend.Validate(); err != nil {
		return nil, err
	}
	win, err := WaitForWindow(ctx, cfg)
	if err != nil {
		return nil, err
	}
	mons, err := cfg.Desktop.Monitors()
	if err != nil {
		return nil, fmt.Errorf("monitors: %w", err)
	}
	idx, local, err := OutputFor(win.Client, mons)
	if err != nil {
		return nil, err
	}
	switch backend {
	case BackendWGC:
		if cfg.NewWGC == nil {
			return nil, errors.New("capture backend wgc is not built into this binary")
		}
		return cfg.NewWGC(win, mons[idx])
	default:
		return NewDDAGrab(idx, local), nil
	}
}

// WaitForWindow polls for the game window: visible, owned by the job's PIDs,
// largest client area. It fails with ErrWindowNotFound after cfg.Timeout.
func WaitForWindow(ctx context.Context, cfg Config) (Window, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultWindowTimeout
	}
	poll := cfg.Poll
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		pids, err := cfg.Pids()
		if err == nil && len(pids) > 0 {
			if ws, err := cfg.Finder.Windows(pids); err == nil {
				if w, ok := Largest(ws); ok {
					return w, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return Window{}, ctx.Err()
		case <-deadline.C:
			return Window{}, ErrWindowNotFound
		case <-tick.C:
		}
	}
}

// Largest picks the visible window with the largest non-empty client area.
func Largest(ws []Window) (Window, bool) {
	var best Window
	found := false
	for _, w := range ws {
		if !w.Visible || w.Client.area() <= 0 {
			continue
		}
		if !found || w.Client.area() > best.Client.area() {
			best, found = w, true
		}
	}
	return best, found
}

// OutputFor returns the monitor index (ddagrab output_idx) holding most of r,
// and r translated into that monitor's own coordinates, which is what
// ddagrab's offset_x/offset_y are relative to.
func OutputFor(r Rect, mons []Rect) (int, Rect, error) {
	best, bestArea := -1, 0
	for i, m := range mons {
		if a := intersect(r, m).area(); a > bestArea {
			best, bestArea = i, a
		}
	}
	if best < 0 {
		return 0, Rect{}, errors.New("game window is not on any monitor")
	}
	m := mons[best]
	return best, Rect{X: r.X - m.X, Y: r.Y - m.Y, W: r.W, H: r.H}, nil
}

func intersect(a, b Rect) Rect {
	x0, y0 := max(a.X, b.X), max(a.Y, b.Y)
	x1, y1 := min(a.X+a.W, b.X+b.W), min(a.Y+a.H, b.Y+b.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// ddaGrab is the ffmpeg-native source.
type ddaGrab struct {
	output int
	rect   Rect // monitor-local, even-sized
}

// NewDDAGrab crops output_idx to rect (monitor-local), rounding the size down
// to even as the H.264 encoder needs.
func NewDDAGrab(output int, rect Rect) Source {
	rect.W &^= 1
	rect.H &^= 1
	return &ddaGrab{output: output, rect: rect}
}

func (d *ddaGrab) Name() string { return string(BackendDDAGrab) }
func (d *ddaGrab) Native() bool { return true }
func (d *ddaGrab) Size() (int, int) {
	return d.rect.W, d.rect.H
}
func (d *ddaGrab) Frames(context.Context, FrameFunc) error { return nil }

// InputArgs is the ffmpeg input. w and h are ignored: the crop is the window.
func (d *ddaGrab) InputArgs(_, _, fps int) []string {
	return []string{"-f", "lavfi", "-i", fmt.Sprintf(
		"ddagrab=output_idx=%d:framerate=%d:draw_mouse=0:offset_x=%d:offset_y=%d:video_size=%dx%d",
		d.output, fps, d.rect.X, d.rect.Y, d.rect.W, d.rect.H)}
}

// Pushed is a frame-pushing source built from a function (WGC and tests).
type Pushed struct {
	Label string
	Run   func(ctx context.Context, deliver FrameFunc) error
}

func (p *Pushed) Name() string     { return p.Label }
func (p *Pushed) Native() bool     { return false }
func (p *Pushed) Size() (int, int) { return 0, 0 }
func (p *Pushed) InputArgs(w, h, fps int) []string {
	return media.RawInputArgs("rgba", w, h, fps)
}
func (p *Pushed) Frames(ctx context.Context, deliver FrameFunc) error { return p.Run(ctx, deliver) }
