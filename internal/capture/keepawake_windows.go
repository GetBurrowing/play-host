//go:build windows

package capture

import "syscall"

var procSetThreadExecutionState = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// NewKeepAwake is the Win32 keep-awake.
func NewKeepAwake() KeepAwake {
	return NewKeepAwakeWith(func(flags uint32) bool {
		r, _, _ := procSetThreadExecutionState.Call(uintptr(flags))
		return r != 0
	})
}
