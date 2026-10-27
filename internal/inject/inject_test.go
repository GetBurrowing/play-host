package inject

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/input"
	"github.com/Startup-Suite/play-host/internal/link"
)

type env struct{ id, active uint32 }

func (e env) SessionID() (uint32, error)     { return e.id, nil }
func (e env) ActiveConsoleSessionID() uint32 { return e.active }

type fg struct {
	game  bool
	focus bool // Focus() makes it the game
	uipi  bool
	calls int
}

func (f *fg) IsGame() bool { return f.game }
func (f *fg) Focus() {
	f.calls++
	if f.focus {
		f.game = true
	}
}
func (f *fg) UIPIBlocked() bool { return f.uipi }

type ov struct {
	codes  []uint16
	closed bool
}

func (o *ov) Show(c uint16) { o.codes = append(o.codes, c) }
func (o *ov) Close()        { o.closed = true }

func newInj(t *testing.T, o Options) (*Injector, *FakeSink) {
	t.Helper()
	s := &FakeSink{Client: capture.Rect{X: 100, Y: 50, W: 1280, H: 720}, Virtual: capture.Rect{X: 0, Y: 0, W: 2560, H: 1440}}
	if o.Frame == nil {
		o.Frame = func() (int, int) { return 1280, 720 }
	}
	if o.Foreground == nil {
		o.Foreground = &fg{game: true}
	}
	i, err := New(env{1, 1}, s, o)
	if err != nil {
		t.Fatal(err)
	}
	return i, s
}

// slotted is a binding with src 0 on slot 0 and src 1 on slot 1.
func slotted(slots map[int]int) *input.Binding {
	b := input.NewBinding()
	b.SetSlots(slots)
	return b
}

func send(t *testing.T, i *Injector, b *input.Binding, label, msg string) input.Result {
	t.Helper()
	r, err := b.Handle(label, []byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	i.Apply(r.Lines)
	return r
}

func TestInjectRefusesSession0(t *testing.T) {
	if _, err := New(env{0, 0}, &FakeSink{}, Options{}); err == nil || !strings.Contains(err.Error(), "session 0") {
		t.Fatalf("session 0: %v", err)
	}
	if _, err := New(env{2, 1}, &FakeSink{}, Options{}); err == nil {
		t.Fatal("non-console session accepted")
	}
	if _, err := New(env{1, 1}, &FakeSink{}, Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyCodeToScancodeCoversKeyNames(t *testing.T) {
	seen := map[Scan]string{}
	for _, name := range input.KeyNames() {
		found := false
		for _, loc := range []int{input.LocNone, input.LocLeft, input.LocRight} {
			if s, ok := Scancode(name, loc); ok {
				found = true
				if prev, dup := seen[s]; dup && prev != name {
					t.Errorf("%s and %s share scancode %+v", prev, name, s)
				}
				seen[s] = name
			}
		}
		if !found {
			t.Errorf("no scancode for key name %q", name)
		}
	}
	// Spot checks against the Set-1 table.
	for _, c := range []struct {
		name string
		loc  int
		want Scan
	}{
		{"W", 0, Scan{0x11, false}}, {"A", 0, Scan{0x1E, false}}, {"Space", 0, Scan{0x39, false}},
		{"Shift", input.LocLeft, Scan{0x2A, false}}, {"Shift", input.LocRight, Scan{0x36, false}},
		{"Ctrl", input.LocRight, Scan{0x1D, true}}, {"Up", 0, Scan{0x48, true}}, {"Kp Enter", 0, Scan{0x1C, true}},
		{"F11", 0, Scan{0x57, false}}, {"1", 0, Scan{0x02, false}}, {"0", 0, Scan{0x0B, false}},
	} {
		if got, ok := Scancode(c.name, c.loc); !ok || got != c.want {
			t.Errorf("%s/%d: got %+v ok=%v want %+v", c.name, c.loc, got, ok, c.want)
		}
	}
}

func TestKeyDownUpReachSink(t *testing.T) {
	i, s := newInj(t, Options{})
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-events", `{"t":"key","code":"KeyW","down":true}`)
	send(t, i, b, "input-events", `{"t":"key","code":"KeyW","down":false}`)
	if !s.Has("key 0x11 ext=false down=true") || !s.Has("key 0x11 ext=false down=false") {
		t.Fatalf("calls %v", s.Calls)
	}
}

func TestTouchScaledToClientRect(t *testing.T) {
	i, s := newInj(t, Options{})
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-events", `{"t":"touch","src":0,"id":0,"ph":"down","x":0.5,"y":0.5}`)
	// 0.5 of the 1280x720 frame is client px (640, 360); the client sits at
	// (100, 50) on a 2560x1440 virtual desktop, so the screen pixel is
	// (740, 410) and the absolute coordinates are px*65535/(size-1).
	wantX, wantY := 18951, 18672
	if len(s.Px) != 1 || s.Px[0] != [2]int{640, 360} {
		t.Fatalf("client px %v", s.Px)
	}
	if len(s.Abs) != 1 || s.Abs[0] != [2]int{wantX, wantY} {
		t.Fatalf("absolute %v, want [%d %d]", s.Abs, wantX, wantY)
	}
	if !s.Has("button 0 down=true") {
		t.Fatalf("calls %v", s.Calls)
	}
	send(t, i, b, "input-events", `{"t":"touch","src":0,"id":0,"ph":"up","x":0.5,"y":0.5}`)
	if !s.Has("button 0 down=false") {
		t.Fatalf("no left up: %v", s.Calls)
	}
	// Scaling by the frame size alone (ignoring the client rect) would give
	// 640*65535/2559 = 16389 on x; the documented value differs.
	if wantX == 16389 {
		t.Fatal("test constants do not separate client rect from frame")
	}
}

func TestTouchOtherIDIgnored(t *testing.T) {
	i, s := newInj(t, Options{})
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-events", `{"t":"touch","src":0,"id":3,"ph":"down","x":0.5,"y":0.5}`)
	if s.Count() != 0 {
		t.Fatalf("calls %v", s.Calls)
	}
}

func TestSpectatorInputDropped(t *testing.T) {
	i, s := newInj(t, Options{})
	spectator := slotted(nil)
	for _, m := range []string{
		`{"t":"key","code":"KeyW","down":true}`,
		`{"t":"touch","src":0,"id":0,"ph":"down","x":0.5,"y":0.5}`,
	} {
		r := send(t, i, spectator, "input-events", m)
		if r.Dropped != input.DropNoSlot {
			t.Fatalf("dropped %q", r.Dropped)
		}
	}
	if s.Count() != 0 {
		t.Fatalf("sink calls %v", s.Calls)
	}
}

func TestKBMOnlySlot0(t *testing.T) {
	i, s := newInj(t, Options{})
	b := slotted(map[int]int{0: 0, 1: 1})
	send(t, i, b, "input-events", `{"t":"key","src":1,"code":"KeyW","down":true}`)
	if s.Count() != 0 || i.Counts().KBMSlot != 1 {
		t.Fatalf("calls %v counts %+v", s.Calls, i.Counts())
	}
	send(t, i, b, "input-events", `{"t":"touch","src":1,"id":0,"ph":"down","x":0.1,"y":0.1}`)
	if s.Count() != 0 || i.Counts().KBMSlot != 2 {
		t.Fatalf("touch: calls %v counts %+v", s.Calls, i.Counts())
	}
	// Slot 0 still types.
	send(t, i, b, "input-events", `{"t":"key","src":0,"code":"KeyW","down":true}`)
	if s.Count() != 1 {
		t.Fatalf("slot 0: %v", s.Calls)
	}
}

func padMsg(seq int, buttons map[int]float64, axes [4]float64) string {
	bs := make([]float64, 17)
	for k, v := range buttons {
		bs[k] = v
	}
	m, _ := json.Marshal(map[string]any{"t": "pad", "src": 0, "seq": seq, "connected": true, "buttons": bs, "axes": axes})
	return string(m)
}

func TestPadPerSlotX360(t *testing.T) {
	i, s := newInj(t, Options{})
	b0 := slotted(map[int]int{0: 0})
	b1 := slotted(map[int]int{0: 1})
	send(t, i, b0, "input-state", padMsg(1, map[int]float64{0: 1, 7: 1, 12: 1}, [4]float64{0.5, -1, 0, 1}))
	send(t, i, b1, "input-state", padMsg(1, map[int]float64{3: 1, 6: 0.5}, [4]float64{}))
	if s.Targets() != 2 || s.Created != 2 {
		t.Fatalf("targets %d created %d", s.Targets(), s.Created)
	}
	r0 := s.Reports[0][len(s.Reports[0])-1]
	want0 := X360Report{Buttons: XA | XDpadUp, RT: 255, LX: 16384, LY: 32767, RX: 0, RY: -32767}
	if r0 != want0 {
		t.Fatalf("slot 0 report %+v want %+v", r0, want0)
	}
	r1 := s.Reports[1][len(s.Reports[1])-1]
	if r1.Buttons != XY || r1.LT != 128 {
		t.Fatalf("slot 1 report %+v", r1)
	}
	// A fifth pad (slot 4) is beyond XInput's four.
	b4 := slotted(map[int]int{0: 4})
	send(t, i, b4, "input-state", padMsg(1, map[int]float64{0: 1}, [4]float64{}))
	if s.Targets() != 2 || i.Counts().PadUnavailable != 1 {
		t.Fatalf("slot 4: targets %d counts %+v", s.Targets(), i.Counts())
	}
}

func TestPadUnavailableKeepsKBM(t *testing.T) {
	i, s := newInj(t, Options{})
	s.PadsOff = true
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-state", padMsg(1, map[int]float64{0: 1}, [4]float64{}))
	send(t, i, b, "input-state", padMsg(2, map[int]float64{1: 1}, [4]float64{}))
	if i.Counts().PadUnavailable != 2 {
		t.Fatalf("counts %+v", i.Counts())
	}
	send(t, i, b, "input-events", `{"t":"key","code":"KeyW","down":true}`)
	if !s.Has("key 0x11 ext=false down=true") {
		t.Fatalf("keyboard stopped working: %v", s.Calls)
	}
}

func TestReleaseOnBindingRelease(t *testing.T) {
	i, s := newInj(t, Options{})
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-events", `{"t":"key","code":"KeyW","down":true}`)
	send(t, i, b, "input-events", `{"t":"touch","src":0,"id":0,"ph":"down","x":0.2,"y":0.2}`)
	send(t, i, b, "input-state", padMsg(1, map[int]float64{0: 1}, [4]float64{}))
	i.Apply(b.Release())
	for _, want := range []string{"key 0x11 ext=false down=false", "button 0 down=false", "removepad 0"} {
		if !s.Has(want) {
			t.Errorf("missing %q in %v", want, s.Calls)
		}
	}
	if r := s.Reports[0][len(s.Reports[0])-1]; r != (X360Report{}) {
		t.Errorf("pad not zeroed: %+v", r)
	}
	if s.Targets() != 0 {
		t.Errorf("targets left: %d", s.Targets())
	}
}

func TestCloseReleasesEverything(t *testing.T) {
	o := &ov{}
	i, s := newInj(t, Options{Overlay: o})
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-events", `{"t":"key","code":"KeyD","down":true}`)
	i.Close()
	i.Close()
	if !s.Has("key 0x20 ext=false down=false") || !s.Has("releaseall") || !o.closed {
		t.Fatalf("calls %v closed=%v", s.Calls, o.closed)
	}
}

func TestNotForegroundDropped(t *testing.T) {
	f := &fg{game: false, focus: false, uipi: true}
	var logs []string
	i, s := newInj(t, Options{Foreground: f, Logf: func(format string, a ...any) { logs = append(logs, format) }})
	b := slotted(map[int]int{0: 0})
	send(t, i, b, "input-events", `{"t":"key","code":"KeyW","down":true}`)
	send(t, i, b, "input-events", `{"t":"touch","src":0,"id":0,"ph":"down","x":0.5,"y":0.5}`)
	if s.Count() != 0 || i.Counts().NotForeground != 2 || f.calls != 2 {
		t.Fatalf("calls %v counts %+v focus calls %d", s.Calls, i.Counts(), f.calls)
	}
	uipi := 0
	for _, l := range logs {
		if strings.Contains(l, "UIPI") {
			uipi++
		}
	}
	if uipi != 1 {
		t.Fatalf("UIPI line logged %d times", uipi)
	}
	// Focus succeeding lets input through.
	f.focus = true
	send(t, i, b, "input-events", `{"t":"key","code":"KeyS","down":true}`)
	if !s.Has("key 0x1f ext=false down=true") {
		t.Fatalf("calls %v", s.Calls)
	}
	// A key-up still goes through when focus is lost: nothing may stay down.
	f.game, f.focus = false, false
	send(t, i, b, "input-events", `{"t":"key","code":"KeyS","down":false}`)
	if !s.Has("key 0x1f ext=false down=false") {
		t.Fatalf("up refused: %v", s.Calls)
	}
}

// The probe code reaching the overlay is the code the addon would draw, and
// a stamped frame decodes back to it.
func TestProbeCodeSameAsAddon(t *testing.T) {
	for _, slot := range []int{0, 1} {
		for _, seq := range []int64{0, 1023} {
			o := &ov{}
			i, _ := newInj(t, Options{Overlay: o})
			b := slotted(map[int]int{0: slot})
			send(t, i, b, "input-events", `{"t":"probe","seq":`+itoa(seq)+`}`)
			want := uint16(slot)<<10 | uint16(seq)
			if len(o.codes) != 1 || o.codes[0] != want {
				t.Fatalf("slot %d seq %d: overlay got %v, want %d", slot, seq, o.codes, want)
			}
			pix := make([]byte, 128*4*64)
			StampProbe(pix, 128, o.codes[0])
			got, err := link.DecodeCornerRGBA(pix, 128)
			if slot == 0 && seq == 0 {
				// Code 0 is the idle corner: the addon never draws it either.
				if err == nil {
					t.Fatal("code 0 must not decode")
				}
				continue
			}
			if err != nil || got != want {
				t.Fatalf("slot %d seq %d: decoded %d, %v; want %d", slot, seq, got, err, want)
			}
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// The slot is whatever the host's table says for the message's src, never
// the src itself: src 0 bound to slot 1 is a second player, not the keyboard.
func TestSlotComesFromTableNotMessage(t *testing.T) {
	i, s := newInj(t, Options{})
	b := slotted(map[int]int{0: 1})
	send(t, i, b, "input-events", `{"t":"key","src":0,"code":"KeyW","down":true}`)
	if s.Count() != 0 || i.Counts().KBMSlot != 1 {
		t.Fatalf("calls %v counts %+v", s.Calls, i.Counts())
	}
	// A browser naming a slot itself changes nothing.
	send(t, i, b, "input-events", `{"t":"key","src":0,"slot":0,"code":"KeyW","down":true}`)
	if s.Count() != 0 {
		t.Fatalf("browser-named slot honoured: %v", s.Calls)
	}
}
