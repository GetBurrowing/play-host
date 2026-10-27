//go:build !windows

package capture

// NewKeepAwake is a no-op off Windows: there is no display sleep to hold off.
func NewKeepAwake() KeepAwake { return NewKeepAwakeWith(func(uint32) bool { return true }) }
