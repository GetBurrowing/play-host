//go:build windows

package inject

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/probe"
)

// RealOptions describes the running game.
type RealOptions struct {
	Client func() capture.Rect // the game's client area, screen pixels
	Frame  func() (w, h int)   // exported frame size
	Window func() uintptr      // the game window
	Pids   func() ([]int, error)
	Logf   func(string, ...any)
}

// NewReal builds the Win32 injector: SendInput, ViGEm pads, the foreground
// guard and the click-through probe overlay. It refuses in session 0.
func NewReal(env capture.Env, o RealOptions) (*Injector, error) {
	if err := capture.Guard(env); err != nil {
		return nil, errors.New("input" + errTail(err))
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	sink := &winSink{client: o.Client, logf: logf}
	ov, err := newOverlay(o.Client)
	if err != nil {
		logf("probe overlay unavailable: %v", err)
		ov = nil
	}
	opts := Options{Frame: o.Frame, Foreground: &winForeground{pids: o.Pids, window: o.Window}, Logf: logf}
	if ov != nil {
		opts.Overlay = ov
	}
	return New(env, sink, opts)
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSendInput        = user32.NewProc("SendInput")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procGetForeground    = user32.NewProc("GetForegroundWindow")
	procSetForeground    = user32.NewProc("SetForegroundWindow")
	procGetWindowTPID    = user32.NewProc("GetWindowThreadProcessId")
)

const (
	smXVirtual, smYVirtual, smCXVirtual, smCYVirtual = 76, 77, 78, 79

	inputMouse, inputKeyboard = 0, 1

	keyeventfExtended = 0x0001
	keyeventfKeyUp    = 0x0002
	keyeventfScancode = 0x0008

	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfVirtualDsk = 0x4000
	mouseeventfAbsolute   = 0x8000
)

// kbInput and mouseInput are INPUT with a KEYBDINPUT / MOUSEINPUT union
// member, 40 bytes on amd64.
type kbInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	extra uintptr
	_     [8]byte
}

type mouseInput struct {
	typ   uint32
	_     uint32
	dx    int32
	dy    int32
	data  uint32
	flags uint32
	time  uint32
	extra uintptr
}

var (
	_ = [1]struct{}{}[unsafe.Sizeof(kbInput{})-40]
	_ = [1]struct{}{}[unsafe.Sizeof(mouseInput{})-40]
)

func sendInput(in unsafe.Pointer) {
	procSendInput.Call(1, uintptr(in), 40)
}

func metric(i uintptr) int {
	r, _, _ := procGetSystemMetrics.Call(i)
	return int(int32(r))
}

// winSink is SendInput plus ViGEm.
type winSink struct {
	client func() capture.Rect
	logf   func(string, ...any)
	vigem  vigem
}

func (w *winSink) Key(scan uint16, extended, down bool) {
	f := uint32(keyeventfScancode)
	if extended {
		f |= keyeventfExtended
	}
	if !down {
		f |= keyeventfKeyUp
	}
	in := kbInput{typ: inputKeyboard, scan: scan, flags: f}
	sendInput(unsafe.Pointer(&in))
}

func (w *winSink) MouseAbs(cx, cy int) {
	virt := capture.Rect{X: metric(smXVirtual), Y: metric(smYVirtual), W: metric(smCXVirtual), H: metric(smCYVirtual)}
	ax, ay := AbsCoords(w.client(), virt, cx, cy)
	in := mouseInput{typ: inputMouse, dx: int32(ax), dy: int32(ay), flags: mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDsk}
	sendInput(unsafe.Pointer(&in))
}

func (w *winSink) MouseButton(btn int, down bool) {
	f := uint32(mouseeventfLeftUp)
	if down {
		f = mouseeventfLeftDown
	}
	in := mouseInput{typ: inputMouse, flags: f}
	sendInput(unsafe.Pointer(&in))
}

func (w *winSink) Pad(slot int, r X360Report) error { return w.vigem.update(slot, r) }
func (w *winSink) RemovePad(slot int)               { w.vigem.remove(slot) }

// ReleaseAll removes every pad target and disconnects. Keys and buttons the
// Injector tracked are already lifted by it.
func (w *winSink) ReleaseAll() { w.vigem.closeAll() }

// vigem drives ViGEmClient.dll, loaded by full path next to play-host.exe.
type vigem struct {
	mu      sync.Mutex
	loaded  bool
	err     error
	dll     *syscall.LazyDLL
	client  uintptr
	targets map[int]uintptr
}

const vigemErrorNone = 0x20000000

func (v *vigem) load() error {
	if v.loaded {
		return v.err
	}
	v.loaded = true
	exe, err := os.Executable()
	if err != nil {
		v.err = err
		return err
	}
	v.dll = syscall.NewLazyDLL(filepath.Join(filepath.Dir(exe), "ViGEmClient.dll"))
	if err := v.dll.Load(); err != nil {
		v.err = errors.Join(ErrPadUnavailable, err)
		return v.err
	}
	alloc, _, _ := v.dll.NewProc("vigem_alloc").Call()
	if alloc == 0 {
		v.err = errors.Join(ErrPadUnavailable, errors.New("vigem_alloc failed"))
		return v.err
	}
	if r, _, _ := v.dll.NewProc("vigem_connect").Call(alloc); uint32(r) != vigemErrorNone {
		v.dll.NewProc("vigem_free").Call(alloc)
		v.err = errors.Join(ErrPadUnavailable, errors.New("ViGEmBus not found"))
		return v.err
	}
	v.client, v.targets = alloc, map[int]uintptr{}
	return nil
}

// xusbReport is XUSB_REPORT (12 bytes); passed by pointer under the x64 ABI.
type xusbReport struct {
	buttons uint16
	lt, rt  uint8
	lx, ly  int16
	rx, ry  int16
}

func (v *vigem) update(slot int, r X360Report) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.load(); err != nil {
		return err
	}
	t := v.targets[slot]
	if t == 0 {
		t, _, _ = v.dll.NewProc("vigem_target_x360_alloc").Call()
		if t == 0 {
			return errors.Join(ErrPadUnavailable, errors.New("vigem_target_x360_alloc failed"))
		}
		if res, _, _ := v.dll.NewProc("vigem_target_add").Call(v.client, t); uint32(res) != vigemErrorNone {
			v.dll.NewProc("vigem_target_free").Call(t)
			return errors.Join(ErrPadUnavailable, errors.New("vigem_target_add failed"))
		}
		v.targets[slot] = t
	}
	rep := xusbReport{r.Buttons, r.LT, r.RT, r.LX, r.LY, r.RX, r.RY}
	v.dll.NewProc("vigem_target_x360_update").Call(v.client, t, uintptr(unsafe.Pointer(&rep)))
	return nil
}

func (v *vigem) remove(slot int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.client == 0 {
		return
	}
	if t := v.targets[slot]; t != 0 {
		v.dll.NewProc("vigem_target_remove").Call(v.client, t)
		v.dll.NewProc("vigem_target_free").Call(t)
		delete(v.targets, slot)
	}
}

func (v *vigem) closeAll() {
	v.mu.Lock()
	slots := make([]int, 0, len(v.targets))
	for s := range v.targets {
		slots = append(slots, s)
	}
	v.mu.Unlock()
	for _, s := range slots {
		v.remove(s)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.client != 0 {
		v.dll.NewProc("vigem_disconnect").Call(v.client)
		v.dll.NewProc("vigem_free").Call(v.client)
		v.client, v.loaded = 0, false
	}
}

// winForeground keeps input out of other applications.
type winForeground struct {
	pids   func() ([]int, error)
	window func() uintptr
}

func (f *winForeground) IsGame() bool {
	h, _, _ := procGetForeground.Call()
	if h == 0 {
		return false
	}
	var pid uint32
	procGetWindowTPID.Call(h, uintptr(unsafe.Pointer(&pid)))
	ps, err := f.pids()
	if err != nil {
		return false
	}
	for _, p := range ps {
		if uint32(p) == pid {
			return true
		}
	}
	return false
}

func (f *winForeground) Focus() {
	if h := f.window(); h != 0 {
		procSetForeground.Call(h)
	}
}

// UIPIBlocked reports a game at a higher integrity level than play-host.
func (f *winForeground) UIPIBlocked() bool {
	ps, err := f.pids()
	if err != nil || len(ps) == 0 {
		return false
	}
	mine, err1 := integrity(uint32(windows.GetCurrentProcessId()))
	theirs, err2 := integrity(uint32(ps[0]))
	return err1 == nil && err2 == nil && theirs > mine
}

func integrity(pid uint32) (uint32, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return 0, err
	}
	defer tok.Close()
	var n uint32
	_ = windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, nil, 0, &n)
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, &buf[0], n, &n); err != nil {
		return 0, err
	}
	sid := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0])).Label.Sid
	return sid.SubAuthority(uint32(sid.SubAuthorityCount() - 1)), nil
}

// ---- probe overlay: a click-through layered window over the client's
// top-left, 64x64, drawn with the probe cells for two frames.

const (
	wsPopup           = 0x80000000
	wsExLayered       = 0x00080000
	wsExTransparent   = 0x00000020
	wsExTopmost       = 0x00000008
	wsExNoActivate    = 0x08000000
	wsExToolWindow    = 0x00000080
	wmApp             = 0x8000
	wmTimer           = 0x0113
	wmPaint           = 0x000F
	wmClose           = 0x0010
	wmDestroy         = 0x0002
	swHide            = 0
	swShowNoActivate  = 4
	lwaAlpha          = 0x2
	swpNoActivate     = 0x0010
	hwndTopmost       = ^uintptr(0) // -1
	overlayHoldMillis = 34          // two frames at 60 fps
)

type point struct{ x, y int32 }
type rect struct{ l, t, r, b int32 }
type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menu       *uint16
	class      *uint16
	iconSm     uintptr
}
type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}
type paintStruct struct {
	hdc      uintptr
	erase    int32
	paint    rect
	restore  int32
	incUpd   int32
	reserved [32]byte
}

var (
	procRegisterClass = user32.NewProc("RegisterClassExW")
	procCreateWindow  = user32.NewProc("CreateWindowExW")
	procDefWindowProc = user32.NewProc("DefWindowProcW")
	procGetMessage    = user32.NewProc("GetMessageW")
	procDispatch      = user32.NewProc("DispatchMessageW")
	procPostMessage   = user32.NewProc("PostMessageW")
	procPostQuit      = user32.NewProc("PostQuitMessage")
	procShowWindow    = user32.NewProc("ShowWindow")
	procSetWindowPos  = user32.NewProc("SetWindowPos")
	procSetLayered    = user32.NewProc("SetLayeredWindowAttributes")
	procInvalidate    = user32.NewProc("InvalidateRect")
	procUpdateWindow  = user32.NewProc("UpdateWindow")
	procBeginPaint    = user32.NewProc("BeginPaint")
	procEndPaint      = user32.NewProc("EndPaint")
	procFillRect      = user32.NewProc("FillRect")
	procSetTimer      = user32.NewProc("SetTimer")
	procKillTimer     = user32.NewProc("KillTimer")
	procDestroyWindow = user32.NewProc("DestroyWindow")
	procGetStock      = gdi32.NewProc("GetStockObject")
	procModuleHandle  = kernel32.NewProc("GetModuleHandleW")
)

var overlayCode uint32 // read by the window procedure on the overlay thread

type winOverlay struct {
	hwnd   uintptr
	client func() capture.Rect
	once   sync.Once
}

func newOverlay(client func() capture.Rect) (*winOverlay, error) {
	o := &winOverlay{client: client}
	ready := make(chan error, 1)
	go o.run(ready)
	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
	case <-time.After(5 * time.Second):
		return nil, errors.New("overlay window did not start")
	}
	return o, nil
}

func (o *winOverlay) run(ready chan<- error) {
	runtimeLock()
	hinst, _, _ := procModuleHandle.Call(0)
	name, _ := windows.UTF16PtrFromString("BurrowProbeOverlay")
	wc := wndClassEx{size: uint32(unsafe.Sizeof(wndClassEx{})), wndProc: syscall.NewCallback(overlayProc), instance: hinst, class: name}
	procRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))
	c := o.client()
	edge := probe.Cells * probe.CellPx
	h, _, err := procCreateWindow.Call(wsExLayered|wsExTransparent|wsExTopmost|wsExNoActivate|wsExToolWindow,
		uintptr(unsafe.Pointer(name)), 0, wsPopup, uintptr(c.X), uintptr(c.Y), uintptr(edge), uintptr(edge), 0, 0, hinst, 0)
	if h == 0 {
		ready <- err
		return
	}
	o.hwnd = h
	procSetLayered.Call(h, 0, 255, lwaAlpha)
	ready <- nil
	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procDispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func overlayProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmApp:
		overlayCode = uint32(wParam)
		procInvalidate.Call(hwnd, 0, 1)
		procShowWindow.Call(hwnd, swShowNoActivate)
		procUpdateWindow.Call(hwnd)
		procSetTimer.Call(hwnd, 1, overlayHoldMillis, 0)
		return 0
	case wmTimer:
		procKillTimer.Call(hwnd, 1)
		procShowWindow.Call(hwnd, swHide)
		return 0
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		bits := probe.Encode(uint16(overlayCode))
		white, _, _ := procGetStock.Call(0) // WHITE_BRUSH
		black, _, _ := procGetStock.Call(4) // BLACK_BRUSH
		for i, on := range bits {
			x, y := int32(i%probe.Cells*probe.CellPx), int32(i/probe.Cells*probe.CellPx)
			rc := rect{x, y, x + probe.CellPx, y + probe.CellPx}
			br := black
			if on {
				br = white
			}
			procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), br)
		}
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuit.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

// Show draws code over the game's top-left for two frames. Safe from any
// goroutine: the window procedure runs on the overlay's own thread.
func (o *winOverlay) Show(code uint16) {
	c := o.client()
	procSetWindowPos.Call(o.hwnd, hwndTopmost, uintptr(c.X), uintptr(c.Y), 0, 0, swpNoActivate|0x0001) // SWP_NOSIZE
	procPostMessage.Call(o.hwnd, wmApp, uintptr(code), 0)
}

func (o *winOverlay) Close() {
	o.once.Do(func() { procPostMessage.Call(o.hwnd, wmClose, 0, 0) })
}
