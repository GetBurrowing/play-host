// Package input turns the browser's messages into the low-level lines the
// burrow_play addon feeds to Input.parse_input_event. All mapping and
// diffing lives here, in Go, so it is unit-tested; the addon only resolves
// a Godot key name to a keycode and builds the InputEvent.
//
// Browser -> host, on the two data channels:
//
//	input-events (reliable, ordered):
//	  {"t":"key","code":"KeyW","down":true,"repeat":false,"src":0}
//	  {"t":"release_all"}
//	  {"t":"probe","seq":N,"src":0}
//	  {"t":"touch","src":0,"id":0,"ph":"down","x":0.42,"y":0.77}   (task 01a0fe45)
//	input-state (unordered, maxRetransmits 0; each message is a full,
//	idempotent snapshot, so a lost one is corrected by the next):
//	  {"t":"pad","src":0,"seq":N,"connected":true,
//	   "buttons":[17 numbers 0..1],"axes":[4 numbers -1..1]}   (W3C standard mapping)
//
// Host -> addon, newline-delimited JSON on the 127.0.0.1 link:
//
//	{"t":"key","d":0,"k":"W","loc":0,"p":true,"e":false}   d: slot; k: Godot key name; loc 0 none, 1 left, 2 right
//	{"t":"jb","d":0,"b":0,"p":true,"v":1}                 InputEventJoypadButton
//	{"t":"ja","d":0,"a":0,"v":-0.5}                      InputEventJoypadMotion
//	{"t":"st","d":0,"i":0,"p":true,"c":false,"x":0.42,"y":0.77}   InputEventScreenTouch
//	{"t":"sd","d":0,"i":0,"x":0.45,"y":0.70}                      InputEventScreenDrag
//	{"t":"release","d":0}   releases what device d holds (multi-peer)
//	{"t":"release_all"}  {"t":"probe","seq":N}  {"t":"export","on":true}
//
// The addon puts slot d's keys on InputEvent device <default key device> + d
// (16 + d in Godot 4.7, so P1's keys keep the device the built-in ui_*
// actions are bound to) and its pad events on device d; see burrow_play.gd.
//
// SLOTS ARE HOST-HELD (task 01a0dbd6). `src` is the browser's local
// controller ordinal (0 = keyboard and the first pad; absent means 0). The
// player slot, which becomes the Godot device index `d`, comes ONLY from the
// table core sends in play_slots for (peer_id, src): see Binding. A `slot`
// key in a browser message is not even decoded, so a browser cannot name a
// slot. A peer whose table holds no slot for the message's src has the
// message dropped and counted (a spectator holds none at all).
//
// TOUCH (task 01a0fe45; the contract is core's Platform.GameStream.Protocol
// moduledoc, "Touch"). id is the browser's pointer id in 0..9; ph is down,
// move, up or cancel; x, y are in [0, 1] of the displayed video frame. The
// Godot touch index i is slot*10+id, so two players' id 0 never collide in
// Godot's global touch index space. No mouse line is ever emitted.
package input

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Out is one line for the addon.
type Out map[string]any

// Browser is a decoded browser message.
type Browser struct {
	T         string    `json:"t"`
	Code      string    `json:"code,omitempty"`
	Down      bool      `json:"down,omitempty"`
	Repeat    bool      `json:"repeat,omitempty"`
	Src       int       `json:"src,omitempty"`
	Seq       int64     `json:"seq,omitempty"`
	Connected *bool     `json:"connected,omitempty"`
	Buttons   []float64 `json:"buttons,omitempty"`
	Axes      []float64 `json:"axes,omitempty"`
	// Touch (task 01a0fe45). X and Y are pointers so a missing coordinate
	// is told apart from 0.
	ID int      `json:"id,omitempty"`
	Ph string   `json:"ph,omitempty"`
	X  *float64 `json:"x,omitempty"`
	Y  *float64 `json:"y,omitempty"`
}

// Key locations, as Godot's KeyLocation enum.
const (
	LocNone  = 0
	LocLeft  = 1
	LocRight = 2
)

type keyName struct {
	name string
	loc  int
}

// keyMap is KeyboardEvent.code -> Godot key name (as understood by
// OS.find_keycode_from_string) and location. Physical positions on a US
// layout; the addon sets both keycode and physical_keycode from it.
var keyMap = func() map[string]keyName {
	m := map[string]keyName{
		"Escape": {"Escape", 0}, "Tab": {"Tab", 0}, "Backspace": {"Backspace", 0}, "Enter": {"Enter", 0},
		"Space": {"Space", 0}, "CapsLock": {"CapsLock", 0}, "NumLock": {"NumLock", 0}, "ScrollLock": {"ScrollLock", 0},
		"Pause": {"Pause", 0}, "PrintScreen": {"Print", 0}, "ContextMenu": {"Menu", 0},
		"Insert": {"Insert", 0}, "Delete": {"Delete", 0}, "Home": {"Home", 0}, "End": {"End", 0},
		"PageUp": {"PageUp", 0}, "PageDown": {"PageDown", 0},
		"ArrowLeft": {"Left", 0}, "ArrowUp": {"Up", 0}, "ArrowRight": {"Right", 0}, "ArrowDown": {"Down", 0},
		"ShiftLeft": {"Shift", LocLeft}, "ShiftRight": {"Shift", LocRight},
		"ControlLeft": {"Ctrl", LocLeft}, "ControlRight": {"Ctrl", LocRight},
		"AltLeft": {"Alt", LocLeft}, "AltRight": {"Alt", LocRight},
		"MetaLeft": {"Meta", LocLeft}, "MetaRight": {"Meta", LocRight},
		"Minus": {"Minus", 0}, "Equal": {"Equal", 0}, "BracketLeft": {"BracketLeft", 0}, "BracketRight": {"BracketRight", 0},
		"Backslash": {"BackSlash", 0}, "Semicolon": {"Semicolon", 0}, "Quote": {"Apostrophe", 0}, "Backquote": {"QuoteLeft", 0},
		"Comma": {"Comma", 0}, "Period": {"Period", 0}, "Slash": {"Slash", 0},
		"NumpadAdd": {"Kp Add", 0}, "NumpadSubtract": {"Kp Subtract", 0}, "NumpadMultiply": {"Kp Multiply", 0},
		"NumpadDivide": {"Kp Divide", 0}, "NumpadDecimal": {"Kp Period", 0}, "NumpadEnter": {"Kp Enter", 0},
	}
	for c := 'A'; c <= 'Z'; c++ {
		m["Key"+string(c)] = keyName{string(c), 0}
	}
	for d := '0'; d <= '9'; d++ {
		m["Digit"+string(d)] = keyName{string(d), 0}
		m["Numpad"+string(d)] = keyName{"Kp " + string(d), 0}
	}
	for f := 1; f <= 12; f++ {
		m[fmt.Sprintf("F%d", f)] = keyName{fmt.Sprintf("F%d", f), 0}
	}
	return m
}()

// KeyNames lists every Godot key name the map can produce (for the addon's
// resolve check on the host).
func KeyNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keyMap {
		if !seen[k.name] {
			seen[k.name] = true
			out = append(out, k.name)
		}
	}
	return out
}

// W3C standard gamepad mapping -> Godot JoyButton, or an axis for triggers.
const (
	joyA, joyB, joyX, joyY               = 0, 1, 2, 3
	joyBack, joyGuide, joyStart          = 4, 5, 6
	joyLeftStick, joyRightStick          = 7, 8
	joyLeftShoulder, joyRightShoulder    = 9, 10
	joyDpadUp, joyDpadDown               = 11, 12
	joyDpadLeft, joyDpadRight            = 13, 14
	axisLeftX, axisLeftY, axisRX, axisRY = 0, 1, 2, 3
	axisTriggerLeft, axisTriggerRight    = 4, 5
	stdButtons, stdAxes                  = 17, 4
	pressThreshold                       = 0.5
	axisEpsilon                          = 1.0 / 512
)

// buttonMap[i] is the Godot JoyButton for standard button i, or -1 when the
// standard button is an analog trigger sent as a Godot axis (triggerAxis).
var buttonMap = [stdButtons]int{
	joyA, joyB, joyX, joyY, joyLeftShoulder, joyRightShoulder, -1, -1,
	joyBack, joyStart, joyLeftStick, joyRightStick, joyDpadUp, joyDpadDown, joyDpadLeft, joyDpadRight, joyGuide,
}
var triggerAxis = map[int]int{6: axisTriggerLeft, 7: axisTriggerRight}
var axisMap = [stdAxes]int{axisLeftX, axisLeftY, axisRX, axisRY}

type pad struct {
	seq     int64
	have    bool
	pressed [stdButtons]bool
	trig    [2]float64
	axes    [stdAxes]float64
}

// Touch bounds (task 01a0fe45).
const (
	// TouchIDs is how many concurrent touches one slot may hold (ids 0..9),
	// and the stride of the Godot index slot*TouchIDs+id.
	TouchIDs = 10
	// TouchMinInterval is the shortest gap between two accepted moves of one
	// (slot, id); a move arriving sooner is dropped (DropTouchRate). The
	// browser already caps moves at the session's move_hz; this bounds a
	// browser that does not.
	TouchMinInterval = 4 * time.Millisecond
)

// Translator holds the diff state for one viewer connection.
type Translator struct {
	keys    map[int]map[string]bool // slot -> held codes
	pads    map[int]*pad
	touches map[int]map[int][2]float64 // slot -> id -> last position
	touchAt map[int]map[int]time.Time  // slot -> id -> when its last line was emitted
	// now is the clock for the touch rate bound (tests replace it).
	now func() time.Time
}

// NewTranslator starts with nothing held.
func NewTranslator() *Translator {
	return &Translator{keys: map[int]map[string]bool{}, pads: map[int]*pad{},
		touches: map[int]map[int][2]float64{}, touchAt: map[int]map[int]time.Time{}, now: time.Now}
}

// Result is what one browser message turned into.
type Result struct {
	Lines []Out
	// Activity is true when the message changed game input (key edge, pad
	// change, release, probe). A pad snapshot that changes nothing is NOT
	// activity, so a controller left plugged in cannot hold a session open
	// past its idle timeout.
	Activity bool
	// Slot is the slot the message was bound to, or -1 (release_all, a
	// dropped message).
	Slot int
	// Dropped names why the message was dropped ("" when it was not):
	// DropNoSlot for a src with no slot in the table.
	Dropped string
	// Type is MsgType of the message's "t" (task 01a0ff61), set by
	// Binding.Handle for every message that decoded; "" when it did not.
	Type string
}

// Drop reasons.
const (
	DropNoSlot       = "no slot"
	DropProbeNoSpace = "probe slot outside the 2-bit namespace"
	// DropBadTouch: a touch whose id is outside 0..9, whose ph is unknown,
	// or whose x or y is missing, non-finite or outside [0, 1].
	DropBadTouch = "bad touch"
	// DropTouchRate: a move for a (slot, id) under TouchMinInterval after
	// the previous line for it.
	DropTouchRate = "touch rate"
)

// Decode parses one data-channel message.
func Decode(label string, data []byte) (Browser, error) {
	var m Browser
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s: %w", label, err)
	}
	if m.Src < 0 {
		m.Src = 0
	}
	return m, nil
}

// Message types as the host counts and logs them (task 01a0ff61). The set
// is FIXED: any other "t" a browser sends is TypeOther, so a hostile page
// cannot grow what the host keeps per peer, and no browser string reaches
// the log through a type name.
const (
	TypeKey        = "key"
	TypePad        = "pad"
	TypeProbe      = "probe"
	TypeTouch      = "touch"
	TypeReleaseAll = "release_all"
	TypeOther      = "other"
)

// MsgTypes is every MsgType value, in the order the host counts them.
var MsgTypes = [...]string{TypeKey, TypePad, TypeProbe, TypeTouch, TypeReleaseAll, TypeOther}

// MsgType maps a decoded "t" onto MsgTypes.
func MsgType(t string) string {
	switch t {
	case TypeKey, TypePad, TypeProbe, TypeTouch, TypeReleaseAll:
		return t
	}
	return TypeOther
}

// MsgTypeIndex is MsgType's index in MsgTypes.
func MsgTypeIndex(t string) int {
	for i, n := range MsgTypes {
		if n == t {
			return i
		}
	}
	return len(MsgTypes) - 1
}

// Shape bounds (task 01a0ff61).
const (
	ShapeMaxKeys   = 16
	ShapeMaxKeyLen = 32
)

// Shape describes a message without any of its values: its size in bytes
// and its sorted top-level JSON key names, "not-json" when it is not JSON,
// or "not-an-object" when it is JSON but not an object. At most
// ShapeMaxKeys keys are listed (then "+N more"), each cut to ShapeMaxKeyLen
// bytes, and every byte outside printable ASCII, and every space, comma and
// square bracket, is "?", so a key cannot forge the list's syntax or
// another log line. NEVER a value: it is safe to log.
func Shape(data []byte) (size int, keys string) {
	size = len(data)
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return size, "not-json"
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return size, "not-an-object"
	}
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names)
	more := 0
	if len(names) > ShapeMaxKeys {
		more = len(names) - ShapeMaxKeys
		names = names[:ShapeMaxKeys]
	}
	for i, k := range names {
		names[i] = safeKey(k)
	}
	keys = "[" + strings.Join(names, ",") + "]"
	if more > 0 {
		keys += fmt.Sprintf(" +%d more", more)
	}
	return size, keys
}

func safeKey(k string) string {
	if len(k) > ShapeMaxKeyLen {
		k = k[:ShapeMaxKeyLen]
	}
	b := []byte(k)
	for i, c := range b {
		if c <= ' ' || c >= 0x7f || c == ',' || c == '[' || c == ']' {
			b[i] = '?'
		}
	}
	return string(b)
}

// ErrClass names why Decode failed without quoting the input (task
// 01a0ff61): json.SyntaxError's text quotes a byte of the message, so it is
// never logged. "syntax@<offset>" for a syntax error, "type:<Go field>"
// for a value of the wrong type in a Browser field (the field name comes
// from our struct, never from the browser), else "other".
func ErrClass(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("syntax@%d", se.Offset)
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return "type:" + browserField(te.Field)
	}
	return "other"
}

// browserField maps a JSON path from UnmarshalTypeError onto the Browser
// field it names, or "?" when it names none.
func browserField(path string) string {
	rt := reflect.TypeOf(Browser{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == path || f.Name == path {
			return f.Name
		}
	}
	return "?"
}

// Key translates a key edge for slot.
func (t *Translator) Key(m Browser, slot int) Result {
	k, ok := keyMap[m.Code]
	if !ok {
		return Result{Slot: slot}
	}
	held := t.keys[slot]
	if held == nil {
		held = map[string]bool{}
		t.keys[slot] = held
	}
	if m.Down {
		if held[m.Code] && !m.Repeat {
			return Result{Slot: slot} // duplicate down, not an edge
		}
		held[m.Code] = true
	} else {
		if !held[m.Code] {
			return Result{Slot: slot} // up without down (focus came in mid-press)
		}
		delete(held, m.Code)
	}
	return Result{Lines: []Out{{"t": "key", "d": slot, "k": k.name, "loc": k.loc, "p": m.Down, "e": m.Down && m.Repeat}}, Activity: true, Slot: slot}
}

func unit(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= 1
}

// Touch translates one touch message for slot. A down on an id the slot
// already holds is a move; a move, up or cancel on an id it does not hold
// is ignored (not an edge), like a key up without a down.
func (t *Translator) Touch(m Browser, slot int) Result {
	switch m.Ph {
	case "down", "move", "up", "cancel":
	default:
		return Result{Slot: slot, Dropped: DropBadTouch}
	}
	if m.ID < 0 || m.ID >= TouchIDs || !unit(m.X) || !unit(m.Y) {
		return Result{Slot: slot, Dropped: DropBadTouch}
	}
	x, y := *m.X, *m.Y
	i := slot*TouchIDs + m.ID
	held := t.touches[slot]
	_, isHeld := held[m.ID]
	now := t.now()
	switch {
	case m.Ph == "down" && !isHeld:
		if held == nil {
			held = map[int][2]float64{}
			t.touches[slot] = held
			t.touchAt[slot] = map[int]time.Time{}
		}
		held[m.ID] = [2]float64{x, y}
		t.touchAt[slot][m.ID] = now
		return Result{Lines: []Out{{"t": "st", "d": slot, "i": i, "p": true, "c": false, "x": x, "y": y}}, Activity: true, Slot: slot}
	case !isHeld:
		return Result{Slot: slot} // move/up/cancel on an id this slot does not hold
	case m.Ph == "down" || m.Ph == "move":
		if now.Sub(t.touchAt[slot][m.ID]) < TouchMinInterval {
			return Result{Slot: slot, Dropped: DropTouchRate}
		}
		held[m.ID] = [2]float64{x, y}
		t.touchAt[slot][m.ID] = now
		return Result{Lines: []Out{{"t": "sd", "d": slot, "i": i, "x": x, "y": y}}, Activity: true, Slot: slot}
	default: // up, cancel
		delete(held, m.ID)
		delete(t.touchAt[slot], m.ID)
		return Result{Lines: []Out{{"t": "st", "d": slot, "i": i, "p": false, "c": m.Ph == "cancel", "x": x, "y": y}}, Activity: true, Slot: slot}
	}
}

// releaseTouches lifts every touch slot holds as a canceled touch at its
// last position, in id order, and forgets them.
func (t *Translator) releaseTouches(slot int) []Out {
	held := t.touches[slot]
	ids := make([]int, 0, len(held))
	for id := range held {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var out []Out
	for _, id := range ids {
		p := held[id]
		out = append(out, Out{"t": "st", "d": slot, "i": slot*TouchIDs + id, "p": false, "c": true, "x": p[0], "y": p[1]})
	}
	delete(t.touches, slot)
	delete(t.touchAt, slot)
	return out
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(lo, math.Min(hi, v))
}

// Pad diffs a pad snapshot for slot.
func (t *Translator) Pad(m Browser, slot int) Result {
	p := t.pads[slot]
	if p == nil {
		p = &pad{}
		t.pads[slot] = p
	}
	if p.have && m.Seq <= p.seq {
		return Result{Slot: slot} // stale or duplicate snapshot on the unordered channel
	}
	p.have, p.seq = true, m.Seq
	if m.Connected != nil && !*m.Connected {
		return Result{Lines: t.releasePad(slot), Activity: true, Slot: slot}
	}
	var out []Out
	for i := 0; i < stdButtons; i++ {
		v := 0.0
		if i < len(m.Buttons) {
			v = clamp(m.Buttons[i], 0, 1)
		}
		if a, isTrig := triggerAxis[i]; isTrig {
			ti := a - axisTriggerLeft
			if math.Abs(v-p.trig[ti]) >= axisEpsilon || (v == 0 && p.trig[ti] != 0) {
				p.trig[ti] = v
				out = append(out, Out{"t": "ja", "d": slot, "a": a, "v": v})
			}
			continue
		}
		pressed := v >= pressThreshold
		if pressed != p.pressed[i] {
			p.pressed[i] = pressed
			out = append(out, Out{"t": "jb", "d": slot, "b": buttonMap[i], "p": pressed, "v": v})
		}
	}
	for i := 0; i < stdAxes; i++ {
		v := 0.0
		if i < len(m.Axes) {
			v = clamp(m.Axes[i], -1, 1)
		}
		if math.Abs(v-p.axes[i]) >= axisEpsilon || (v == 0 && p.axes[i] != 0) {
			p.axes[i] = v
			out = append(out, Out{"t": "ja", "d": slot, "a": axisMap[i], "v": v})
		}
	}
	return Result{Lines: out, Activity: len(out) > 0, Slot: slot}
}

func (t *Translator) releasePad(slot int) []Out {
	p := t.pads[slot]
	if p == nil {
		return nil
	}
	var out []Out
	for i, on := range p.pressed {
		if on {
			out = append(out, Out{"t": "jb", "d": slot, "b": buttonMap[i], "p": false, "v": 0.0})
			p.pressed[i] = false
		}
	}
	for i, v := range p.trig {
		if v != 0 {
			out = append(out, Out{"t": "ja", "d": slot, "a": axisTriggerLeft + i, "v": 0.0})
			p.trig[i] = 0
		}
	}
	for i, v := range p.axes {
		if v != 0 {
			out = append(out, Out{"t": "ja", "d": slot, "a": axisMap[i], "v": 0.0})
			p.axes[i] = 0
		}
	}
	return out
}

func (t *Translator) releaseKeys(slot int) []Out {
	var out []Out
	for code := range t.keys[slot] {
		k := keyMap[code]
		out = append(out, Out{"t": "key", "d": slot, "k": k.name, "loc": k.loc, "p": false, "e": false})
	}
	delete(t.keys, slot)
	return out
}

// ReleaseAll releases every held key, pad input and touch on every slot, then asks
// the addon to release whatever it believes is held (belt and braces: the
// addon also tracks what it pressed). This is the v1 (single-peer) release:
// the addon's release_all drops EVERY device, so a multi-peer session uses
// ReleaseSlot per slot instead.
func (t *Translator) ReleaseAll() []Out {
	var out []Out
	for slot := range t.keys {
		out = append(out, t.releaseKeys(slot)...)
	}
	for slot := range t.pads {
		out = append(out, t.releasePad(slot)...)
	}
	slots := make([]int, 0, len(t.touches))
	for slot := range t.touches {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	for _, slot := range slots {
		out = append(out, t.releaseTouches(slot)...)
	}
	return append(out, Out{"t": "release_all"})
}

// ReleaseSlot releases what this translator holds on one slot (keys, pad,
// touches), then asks the
// addon to release whatever it believes device `slot` holds. The pad's diff
// state is forgotten, so the next snapshot for the slot starts clean.
func (t *Translator) ReleaseSlot(slot int) []Out {
	out := t.releaseKeys(slot)
	out = append(out, t.releasePad(slot)...)
	delete(t.pads, slot)
	out = append(out, t.releaseTouches(slot)...)
	return append(out, Out{"t": "release", "d": slot})
}
