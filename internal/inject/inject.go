// Package inject delivers browser input to an engine that has no addon
// (Unreal). The translator is the existing one: input.Decode, input.Binding,
// its rate limits and counters produce the same addon lines they always
// have, and an Injector turns those lines into a Sink's calls, so slots stay
// host-held and every existing bound applies unchanged.
//
// Windows merges all SendInput into one keyboard and one mouse, so keyboard,
// mouse and touch-to-mouse belong to slot 0 only (anything else is dropped
// and counted kbm_slot). Pads get one ViGEm X360 target per slot, up to four.
package inject

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/input"
	"github.com/Startup-Suite/play-host/internal/probe"
)

// MaxPads is the XInput limit on simultaneous controllers.
const MaxPads = 4

// ErrPadUnavailable means ViGEmBus or ViGEmClient.dll is not usable.
var ErrPadUnavailable = errors.New("virtual pad unavailable")

// X360Report is an XUSB report.
type X360Report struct {
	Buttons uint16
	LT, RT  uint8
	LX, LY  int16
	RX, RY  int16
}

// XUSB button bits.
const (
	XDpadUp, XDpadDown, XDpadLeft, XDpadRight uint16 = 0x0001, 0x0002, 0x0004, 0x0008
	XStart, XBack, XLThumb, XRThumb           uint16 = 0x0010, 0x0020, 0x0040, 0x0080
	XLShoulder, XRShoulder, XGuide            uint16 = 0x0100, 0x0200, 0x0400
	XA, XB, XX, XY                            uint16 = 0x1000, 0x2000, 0x4000, 0x8000
)

// Mouse buttons.
const BtnLeft = 0

// Sink is the OS input device. The windows implementation is user32
// SendInput plus ViGEmClient.dll; the fake records calls.
type Sink interface {
	Key(scan uint16, extended, down bool)
	// MouseAbs moves the mouse to a pixel of the game's client area.
	MouseAbs(clientX, clientY int)
	MouseButton(btn int, down bool)
	// Pad creates slot's X360 target on first use and updates it.
	Pad(slot int, r X360Report) error
	RemovePad(slot int)
	// ReleaseAll lifts everything the sink holds (keys, buttons, pads).
	ReleaseAll()
}

// Foreground is the guard that keeps SendInput out of other applications.
type Foreground interface {
	// IsGame reports whether the foreground window belongs to the game.
	IsGame() bool
	// Focus asks Windows to bring the game window forward.
	Focus()
	// UIPIBlocked reports a game running at a higher integrity level.
	UIPIBlocked() bool
}

// Overlay draws a probe code where the capture sees it.
type Overlay interface {
	Show(code uint16)
	Close()
}

// Options wires an Injector.
type Options struct {
	// Frame is the exported frame size, which touch x,y are normalised to.
	Frame      func() (w, h int)
	Foreground Foreground
	Overlay    Overlay
	Logf       func(string, ...any)
}

// Counts are the counters the host reports at the end of a session.
type Counts struct {
	KBMSlot, NotForeground, PadUnavailable int64
}

func (c Counts) String() string {
	return fmt.Sprintf("kbm_slot=%d not_foreground=%d pad_unavailable=%d", c.KBMSlot, c.NotForeground, c.PadUnavailable)
}

// Injector applies translator lines to a Sink.
type Injector struct {
	sink Sink
	o    Options

	kbmSlot, notFG, padUnavail atomic.Int64

	mu        sync.Mutex
	held      map[Scan]bool
	left      bool
	pads      map[int]*X360Report
	padsOff   bool
	uipiCheck bool
}

// New refuses in session 0 (or any session that is not the active console
// session), exactly as capture does: SendInput there reaches no one.
func New(env capture.Env, sink Sink, o Options) (*Injector, error) {
	if err := capture.Guard(env); err != nil {
		return nil, errors.New("input" + errTail(err))
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &Injector{sink: sink, o: o, held: map[Scan]bool{}, pads: map[int]*X360Report{}}, nil
}

func errTail(err error) string {
	const p = "capture"
	s := err.Error()
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return ": " + s
}

// Counts is a snapshot of the counters.
func (i *Injector) Counts() Counts {
	return Counts{KBMSlot: i.kbmSlot.Load(), NotForeground: i.notFG.Load(), PadUnavailable: i.padUnavail.Load()}
}

// foreground reports whether input may be injected now, counting a refusal.
func (i *Injector) foreground() bool {
	fg := i.o.Foreground
	if fg == nil {
		return true
	}
	if !i.uipiCheck {
		i.uipiCheck = true
		if fg.UIPIBlocked() {
			i.o.Logf("UIPI blocks SendInput: the game runs at a higher integrity level than play-host")
		}
	}
	if fg.IsGame() {
		return true
	}
	fg.Focus()
	if fg.IsGame() {
		return true
	}
	i.notFG.Add(1)
	return false
}

// Apply turns translator lines into sink calls.
func (i *Injector) Apply(lines []input.Out) {
	i.mu.Lock()
	defer i.mu.Unlock()
	touched := map[int]bool{}
	for _, l := range lines {
		slot, _ := num(l["d"])
		switch l["t"] {
		case "key":
			i.key(l, slot)
		case "st", "sd":
			i.touch(l, slot)
		case "jb", "ja":
			i.padLine(l, slot)
			touched[slot] = true
		case "release":
			i.releasePad(slot)
			delete(touched, slot)
		case "release_all":
			i.releaseAll()
			touched = map[int]bool{}
		case "probe":
			if seq, ok := num(l["seq"]); ok && i.o.Overlay != nil {
				i.o.Overlay.Show(uint16(seq))
			}
		}
	}
	for slot := range touched {
		i.flushPad(slot)
	}
}

func num(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func (i *Injector) key(l input.Out, slot int) {
	name, _ := l["k"].(string)
	loc, _ := num(l["loc"])
	down, _ := l["p"].(bool)
	s, ok := Scancode(name, loc)
	if !ok {
		return
	}
	if slot != 0 {
		// A release is never refused: a key must not stay down.
		if down {
			i.kbmSlot.Add(1)
		}
		return
	}
	if down {
		if !i.foreground() {
			return
		}
		i.held[s] = true
	} else {
		delete(i.held, s)
	}
	i.sink.Key(s.Code, s.Extended, down)
}

func (i *Injector) touch(l input.Out, slot int) {
	idx, _ := num(l["i"])
	if idx-slot*input.TouchIDs != 0 {
		return // only touch id 0 drives the mouse
	}
	pressed, _ := l["p"].(bool)
	isStart := l["t"] == "st"
	if slot != 0 {
		if !isStart || pressed {
			i.kbmSlot.Add(1)
		}
		return
	}
	x, _ := l["x"].(float64)
	y, _ := l["y"].(float64)
	cancel, _ := l["c"].(bool)
	switch {
	case isStart && !pressed: // up or cancel: lifting is never refused
		if i.left {
			if !cancel {
				i.moveTo(x, y)
			}
			i.left = false
			i.sink.MouseButton(BtnLeft, false)
		}
	case !i.foreground():
	case isStart: // down
		i.moveTo(x, y)
		i.left = true
		i.sink.MouseButton(BtnLeft, true)
	default: // drag
		i.moveTo(x, y)
	}
}

// moveTo scales x,y (fractions of the exported frame) to client pixels.
func (i *Injector) moveTo(x, y float64) {
	w, h := 1, 1
	if i.o.Frame != nil {
		w, h = i.o.Frame()
	}
	i.sink.MouseAbs(scale(x, w), scale(y, h))
}

func scale(f float64, n int) int {
	p := int(math.Floor(f * float64(n)))
	return max(0, min(p, n-1))
}

// Godot joypad button -> XUSB bit (the inverse of the translator's buttonMap).
var xusbButton = map[int]uint16{
	0: XA, 1: XB, 2: XX, 3: XY, 4: XBack, 5: XGuide, 6: XStart, 7: XLThumb, 8: XRThumb,
	9: XLShoulder, 10: XRShoulder, 11: XDpadUp, 12: XDpadDown, 13: XDpadLeft, 14: XDpadRight,
}

func (i *Injector) padLine(l input.Out, slot int) {
	if slot < 0 || slot >= MaxPads || i.padsOff {
		return // counted once per Apply, in flushPad
	}
	r := i.pads[slot]
	if r == nil {
		r = &X360Report{}
		i.pads[slot] = r
	}
	switch l["t"] {
	case "jb":
		b, _ := num(l["b"])
		p, _ := l["p"].(bool)
		if bit, ok := xusbButton[b]; ok {
			if p {
				r.Buttons |= bit
			} else {
				r.Buttons &^= bit
			}
		}
	case "ja":
		a, _ := num(l["a"])
		v, _ := l["v"].(float64)
		switch a {
		case 0:
			r.LX = stick(v)
		case 1:
			r.LY = stick(-v) // W3C Y is down-positive; XUSB is up-positive
		case 2:
			r.RX = stick(v)
		case 3:
			r.RY = stick(-v)
		case 4:
			r.LT = trigger(v)
		case 5:
			r.RT = trigger(v)
		}
	}
}

func stick(v float64) int16 {
	v = math.Max(-1, math.Min(1, v))
	return int16(math.Round(v * 32767))
}

func trigger(v float64) uint8 {
	v = math.Max(0, math.Min(1, v))
	return uint8(math.Round(v * 255))
}

func (i *Injector) flushPad(slot int) {
	if slot < 0 || slot >= MaxPads || i.padsOff {
		i.padUnavail.Add(1)
		return
	}
	r := i.pads[slot]
	if r == nil {
		return
	}
	if err := i.sink.Pad(slot, *r); err != nil {
		i.padsOff = true
		i.padUnavail.Add(1)
		i.o.Logf("virtual pads disabled (%v); keyboard and mouse still work", err)
		delete(i.pads, slot)
	}
}

func (i *Injector) releasePad(slot int) {
	if r := i.pads[slot]; r != nil {
		*r = X360Report{}
		if !i.padsOff {
			_ = i.sink.Pad(slot, *r)
			i.sink.RemovePad(slot)
		}
		delete(i.pads, slot)
	}
}

// releaseAll lifts every held key and button, zeroes and removes every pad.
func (i *Injector) releaseAll() {
	for s := range i.held {
		i.sink.Key(s.Code, s.Extended, false)
	}
	i.held = map[Scan]bool{}
	if i.left {
		i.left = false
		i.sink.MouseButton(BtnLeft, false)
	}
	for slot := range i.pads {
		i.releasePad(slot)
	}
	i.sink.ReleaseAll()
}

// Close releases everything and closes the overlay. Idempotent.
func (i *Injector) Close() {
	i.mu.Lock()
	i.releaseAll()
	ov := i.o.Overlay
	i.o.Overlay = nil
	i.mu.Unlock()
	if ov != nil {
		ov.Close()
	}
}

// StampProbe paints a probe code into the top-left 64x64 of a tightly packed
// RGBA frame, as the addon draws it: 16 px cells, white = 1.
func StampProbe(pix []byte, width int, code uint16) {
	bits := probe.Encode(code)
	edge := probe.Cells * probe.CellPx
	for y := 0; y < edge; y++ {
		for x := 0; x < edge; x++ {
			v := byte(0)
			if bits[(y/probe.CellPx)*probe.Cells+x/probe.CellPx] {
				v = 255
			}
			o := (y*width + x) * 4
			pix[o], pix[o+1], pix[o+2], pix[o+3] = v, v, v, 255
		}
	}
}

// AbsCoords maps a client pixel to SendInput's 0..65535 virtual-desktop
// space: client is the game's client rect and virt the virtual desktop, both
// in screen pixels.
func AbsCoords(client, virt capture.Rect, cx, cy int) (int, int) {
	sx, sy := client.X+cx, client.Y+cy
	return absAxis(sx-virt.X, virt.W), absAxis(sy-virt.Y, virt.H)
}

func absAxis(p, size int) int {
	if size <= 1 {
		return 0
	}
	return int(math.Round(float64(p) * 65535 / float64(size-1)))
}
