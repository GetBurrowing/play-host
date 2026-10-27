//go:build !windows

package capture

import "errors"

// Real returns a Config whose Env, Finder and Desktop refuse: window capture
// exists only on Windows. Tests use fakes.
func Real(pids func() ([]int, error)) Config {
	return Config{Pids: pids, Env: unsupported{}, Finder: unsupported{}, Desktop: unsupported{}}
}

type unsupported struct{}

func (unsupported) SessionID() (uint32, error) {
	return 0, errors.New("window capture is windows-only")
}
func (unsupported) ActiveConsoleSessionID() uint32 { return 0 }
func (unsupported) Windows([]int) ([]Window, error) {
	return nil, errors.New("window capture is windows-only")
}
func (unsupported) Monitors() ([]Rect, error) {
	return nil, errors.New("window capture is windows-only")
}
