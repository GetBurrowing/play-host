//go:build windows

package capture

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	wtsapi32 = syscall.NewLazyDLL("wtsapi32.dll")

	procEnumWindows       = user32.NewProc("EnumWindows")
	procGetWindowThreadPI = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible   = user32.NewProc("IsWindowVisible")
	procGetClientRect     = user32.NewProc("GetClientRect")
	procClientToScreen    = user32.NewProc("ClientToScreen")
	procEnumDisplayMons   = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW   = user32.NewProc("GetMonitorInfoW")
	procActiveConsole     = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

var _ = wtsapi32

// Real returns a Config wired to the Win32 session, window and monitor APIs.
func Real(pids func() ([]int, error)) Config {
	return Config{Pids: pids, Env: winEnv{}, Finder: winFinder{}, Desktop: winDesktop{}}
}

type winEnv struct{}

func (winEnv) SessionID() (uint32, error) {
	var id uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &id); err != nil {
		return 0, err
	}
	return id, nil
}

func (winEnv) ActiveConsoleSessionID() uint32 {
	r, _, _ := procActiveConsole.Call()
	return uint32(r)
}

type point struct{ x, y int32 }
type rect struct{ left, top, right, bottom int32 }

type winFinder struct{}

func (winFinder) Windows(pids []int) ([]Window, error) {
	want := map[uint32]bool{}
	for _, p := range pids {
		want[uint32(p)] = true
	}
	var out []Window
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadPI.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if !want[pid] {
			return 1
		}
		vis, _, _ := procIsWindowVisible.Call(hwnd)
		var rc rect
		if r, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r == 0 {
			return 1
		}
		pt := point{0, 0}
		if r, _, _ := procClientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&pt))); r == 0 {
			return 1
		}
		out = append(out, Window{Handle: hwnd, Visible: vis != 0,
			Client: Rect{X: int(pt.x), Y: int(pt.y), W: int(rc.right - rc.left), H: int(rc.bottom - rc.top)}})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out, nil
}

type monitorInfo struct {
	size    uint32
	monitor rect
	work    rect
	flags   uint32
}

type winDesktop struct{}

// Monitors lists the displays in EnumDisplayMonitors order, which matches
// ddagrab's output_idx on a single-adapter machine (see the runbook).
func (winDesktop) Monitors() ([]Rect, error) {
	var out []Rect
	cb := syscall.NewCallback(func(h, _, _, _ uintptr) uintptr {
		mi := monitorInfo{size: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r != 0 {
			m := mi.monitor
			out = append(out, Rect{X: int(m.left), Y: int(m.top), W: int(m.right - m.left), H: int(m.bottom - m.top)})
		}
		return 1
	})
	procEnumDisplayMons.Call(0, 0, cb, 0)
	return out, nil
}
