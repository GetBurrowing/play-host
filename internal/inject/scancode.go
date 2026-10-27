package inject

import (
	"fmt"

	"github.com/Startup-Suite/play-host/internal/input"
)

// Scan is a Set-1 scancode with the extended (E0) flag.
type Scan struct {
	Code     uint16
	Extended bool
}

const (
	locNone  = input.LocNone
	locLeft  = input.LocLeft
	locRight = input.LocRight
)

type nameLoc struct {
	name string
	loc  int
}

// scancodes is Godot key name (as input.KeyNames, which the translator emits
// in "key" lines) plus location -> Set-1 scancode. A US layout's physical
// positions, as the translator's keyMap assumes.
var scancodes = func() map[nameLoc]Scan {
	m := map[nameLoc]Scan{
		{"Escape", 0}: {0x01, false}, {"Minus", 0}: {0x0C, false}, {"Equal", 0}: {0x0D, false},
		{"Backspace", 0}: {0x0E, false}, {"Tab", 0}: {0x0F, false},
		{"BracketLeft", 0}: {0x1A, false}, {"BracketRight", 0}: {0x1B, false}, {"Enter", 0}: {0x1C, false},
		{"Semicolon", 0}: {0x27, false}, {"Apostrophe", 0}: {0x28, false}, {"QuoteLeft", 0}: {0x29, false},
		{"BackSlash", 0}: {0x2B, false}, {"Comma", 0}: {0x33, false}, {"Period", 0}: {0x34, false}, {"Slash", 0}: {0x35, false},
		{"Space", 0}: {0x39, false}, {"CapsLock", 0}: {0x3A, false}, {"NumLock", 0}: {0x45, false}, {"ScrollLock", 0}: {0x46, false},
		{"Shift", locLeft}: {0x2A, false}, {"Shift", locRight}: {0x36, false},
		{"Ctrl", locLeft}: {0x1D, false}, {"Ctrl", locRight}: {0x1D, true},
		{"Alt", locLeft}: {0x38, false}, {"Alt", locRight}: {0x38, true},
		{"Meta", locLeft}: {0x5B, true}, {"Meta", locRight}: {0x5C, true},
		{"Menu", 0}: {0x5D, true},
		// Print is E0 37 (make code of the 2-byte sequence). Pause's real
		// sequence (E1 1D 45) cannot be sent as one scancode; E0 45 is the
		// distinct extended code Windows maps to Pause/Break.
		{"Print", 0}: {0x37, true}, {"Pause", 0}: {0x45, true},
		{"Insert", 0}: {0x52, true}, {"Delete", 0}: {0x53, true}, {"Home", 0}: {0x47, true}, {"End", 0}: {0x4F, true},
		{"PageUp", 0}: {0x49, true}, {"PageDown", 0}: {0x51, true},
		{"Left", 0}: {0x4B, true}, {"Up", 0}: {0x48, true}, {"Right", 0}: {0x4D, true}, {"Down", 0}: {0x50, true},
		{"Kp Multiply", 0}: {0x37, false}, {"Kp Subtract", 0}: {0x4A, false}, {"Kp Add", 0}: {0x4E, false},
		{"Kp Period", 0}: {0x53, false}, {"Kp Divide", 0}: {0x35, true}, {"Kp Enter", 0}: {0x1C, true},
	}
	// Digit row 1..9,0 = 02..0B; Keypad 7 8 9 / 4 5 6 / 1 2 3 / 0.
	for i, d := range "1234567890" {
		m[nameLoc{string(d), 0}] = Scan{uint16(0x02 + i), false}
	}
	for d, code := range map[string]uint16{"7": 0x47, "8": 0x48, "9": 0x49, "4": 0x4B, "5": 0x4C, "6": 0x4D, "1": 0x4F, "2": 0x50, "3": 0x51, "0": 0x52} {
		m[nameLoc{"Kp " + d, 0}] = Scan{code, false}
	}
	// Letter rows: QWERTYUIOP = 10.., ASDFGHJKL = 1E.., ZXCVBNM = 2C...
	for i, c := range "QWERTYUIOP" {
		m[nameLoc{string(c), 0}] = Scan{uint16(0x10 + i), false}
	}
	for i, c := range "ASDFGHJKL" {
		m[nameLoc{string(c), 0}] = Scan{uint16(0x1E + i), false}
	}
	for i, c := range "ZXCVBNM" {
		m[nameLoc{string(c), 0}] = Scan{uint16(0x2C + i), false}
	}
	for f := 1; f <= 10; f++ {
		m[nameLoc{fmt.Sprintf("F%d", f), 0}] = Scan{uint16(0x3A + f), false}
	}
	m[nameLoc{"F11", 0}] = Scan{0x57, false}
	m[nameLoc{"F12", 0}] = Scan{0x58, false}
	return m
}()

// Scancode maps a translator key name and location to a Set-1 scancode.
func Scancode(name string, loc int) (Scan, bool) {
	if s, ok := scancodes[nameLoc{name, loc}]; ok {
		return s, true
	}
	// A key with no sided variant ignores the location.
	s, ok := scancodes[nameLoc{name, 0}]
	return s, ok
}
