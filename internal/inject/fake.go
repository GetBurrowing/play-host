package inject

import (
	"fmt"
	"sync"

	"github.com/Startup-Suite/play-host/internal/capture"
)

// FakeSink records calls. It is the !windows Sink and the test double. Given
// a geometry it also computes the absolute coordinates the windows sink would
// hand to SendInput.
type FakeSink struct {
	Client, Virtual capture.Rect
	PadsOff         bool // Pad returns ErrPadUnavailable

	mu      sync.Mutex
	Calls   []string
	Abs     [][2]int
	Px      [][2]int
	Reports map[int][]X360Report
	targets map[int]bool
	Created int // pad targets ever created
}

func (f *FakeSink) rec(format string, a ...any) {
	f.mu.Lock()
	f.Calls = append(f.Calls, fmt.Sprintf(format, a...))
	f.mu.Unlock()
}

func (f *FakeSink) Key(scan uint16, extended, down bool) {
	f.rec("key %#x ext=%v down=%v", scan, extended, down)
}

func (f *FakeSink) MouseAbs(cx, cy int) {
	ax, ay := AbsCoords(f.Client, f.Virtual, cx, cy)
	f.mu.Lock()
	f.Px = append(f.Px, [2]int{cx, cy})
	f.Abs = append(f.Abs, [2]int{ax, ay})
	f.mu.Unlock()
	f.rec("mouse %d,%d", ax, ay)
}

func (f *FakeSink) MouseButton(btn int, down bool) { f.rec("button %d down=%v", btn, down) }

func (f *FakeSink) Pad(slot int, r X360Report) error {
	if f.PadsOff {
		return ErrPadUnavailable
	}
	f.mu.Lock()
	if f.Reports == nil {
		f.Reports, f.targets = map[int][]X360Report{}, map[int]bool{}
	}
	if !f.targets[slot] {
		f.targets[slot] = true
		f.Created++
	}
	f.Reports[slot] = append(f.Reports[slot], r)
	f.mu.Unlock()
	f.rec("pad %d", slot)
	return nil
}

func (f *FakeSink) RemovePad(slot int) {
	f.mu.Lock()
	delete(f.targets, slot)
	f.mu.Unlock()
	f.rec("removepad %d", slot)
}

func (f *FakeSink) ReleaseAll() { f.rec("releaseall") }

// Targets is how many pad targets exist now.
func (f *FakeSink) Targets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.targets)
}

// Count is how many calls were recorded.
func (f *FakeSink) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// Has reports whether a recorded call equals s.
func (f *FakeSink) Has(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if c == s {
			return true
		}
	}
	return false
}
