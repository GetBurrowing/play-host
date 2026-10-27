//go:build !windows

package inject

import (
	"errors"

	"github.com/Startup-Suite/play-host/internal/capture"
)

// RealOptions describes the running game for the windows injector.
type RealOptions struct {
	Client func() capture.Rect // the game's client area, screen pixels
	Frame  func() (w, h int)   // exported frame size
	Window func() uintptr      // the game window
	Pids   func() ([]int, error)
	Logf   func(string, ...any)
}

// NewReal is windows-only; elsewhere there is nothing to inject into.
func NewReal(capture.Env, RealOptions) (*Injector, error) {
	return nil, errors.New("input injection is windows-only")
}
