//go:build windows

package inject

import "runtime"

// runtimeLock pins the calling goroutine to its OS thread for good: a window
// and its message loop must live on one thread.
func runtimeLock() { runtime.LockOSThread() }
