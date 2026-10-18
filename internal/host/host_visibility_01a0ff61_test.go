package host

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/input"
	"github.com/Startup-Suite/play-host/internal/protocol"
)

// Task 01a0ff61: per-peer input visibility. Each line answers one question
// about a session from the host log alone: did the browser's channels open,
// which slot is the peer on (or is it a spectator), did a message of each
// type arrive, did one arrive that did not parse, and what did the peer send
// in total. None of them carries a value from a message.

// waveIdle is the host-a idle-check regex (docs/touch-01a0fe45-results/rig/
// host-a/prodcheck-01a0fe45.ps1): a new line matching it would make the idle
// check read the box as busy.
var waveIdle = regexp.MustCompile(`\] (status |session ended|offer|viewer)`)

// visCount reads one of peer id's visibility counters on the loop; -1 when
// the peer is gone.
func visCount(s *Session, id string, f func(*peerState) int64) int64 {
	ch := make(chan int64, 1)
	if s == nil || !s.post(func() {
		if ps := s.peers[id]; ps != nil {
			ch <- f(ps)
		} else {
			ch <- -1
		}
	}) {
		return -1
	}
	select {
	case n := <-ch:
		return n
	case <-time.After(2 * time.Second):
		return -1
	}
}

func touchCount(ps *peerState) int64 { return ps.msgs[input.MsgTypeIndex(input.TypeTouch)].Load() }

// newVisibilityLines is every captured line of a kind task 01a0ff61 added.
func newVisibilityLines(lr *logRec) []string {
	var out []string
	for _, sub := range []string{": data channel ", ": slot ", ": first ", ": unparseable input", ": input summary"} {
		out = append(out, lr.find(sub)...)
	}
	return out
}

func checkNotIdleRegex(t *testing.T, lr *logRec) {
	t.Helper()
	lines := newVisibilityLines(lr)
	if len(lines) == 0 {
		t.Fatal("no visibility lines captured: the regex check below would pass vacuously")
	}
	for _, l := range lines {
		if waveIdle.MatchString(l) {
			t.Errorf("a new line matches the host-a idle-check regex: %s", l)
		}
	}
	// Positive control: the regex does fire on a line it exists to catch.
	if !waveIdle.MatchString("[t1] viewer connected (peer p): x") {
		t.Fatal("the idle regex copy no longer matches a viewer line")
	}
}

// A player and a spectator: both channels open with the peer's slot table
// ("[]" for the spectator), only the player logs a bind, both log their first
// touch (the spectator's with no slot), and each logs one summary on close.
func TestInputVisibilityPlayerAndSpectator(t *testing.T) {
	const port = 40420
	lr := &logRec{t: t}
	st := &fakeStages{t: t, port: port, prepared: make(chan struct{})}
	h, c := newTestHostLog(t, port, st, lr.logf)
	t.Cleanup(func() { stopAndWait(t, h, "t1") })
	h.OnEvent(protocol.EventSessionStart, startPayload("t1", 600))
	peerOpen(h, "t1", "p")
	peerOpen(h, "t1", "s")
	slots(h, "t1", 2, map[string]map[string]int{"p": {"0": 1}, "s": {}})
	p := attach(t, h, c, "p", 0)
	sp := attach(t, h, c, "s", 0)
	eventually(t, "export on", func() bool { return st.game.sawLine(`"on":true`) })

	for _, id := range []string{"p", "s"} {
		for _, label := range []string{"input-events", "input-state"} {
			sub := fmt.Sprintf("peer %s: data channel %s open", id, label)
			eventually(t, sub, func() bool { return lr.count(sub) == 1 })
		}
	}
	if got := lr.find("peer s: data channel input-events open"); !strings.Contains(got[0], "slots []") {
		t.Fatalf("spectator's open line does not show an empty table: %s", got[0])
	}
	if got := lr.find("peer p: data channel input-events open"); !strings.Contains(got[0], "slots [src0:1]") {
		t.Fatalf("player's open line does not show its slot: %s", got[0])
	}
	// One bind, although reconcile ran SetSlots for p more than once.
	if n := lr.count("peer p: slot 1 bound (src 0)"); n != 1 {
		t.Fatalf("p bind lines = %d, want 1", n)
	}
	if n := lr.count("peer s: slot "); n != 0 {
		t.Fatalf("spectator logged a slot change: %v", lr.find("peer s: slot "))
	}

	p.events.SendText(`{"t":"touch","src":0,"id":0,"ph":"down","x":0.5,"y":0.5}`)
	sp.events.SendText(`{"t":"touch","src":0,"id":0,"ph":"down","x":0.5,"y":0.5}`)
	eventually(t, "p first touch", func() bool { return lr.count("peer p: first touch message on input-events") == 1 })
	eventually(t, "s first touch", func() bool { return lr.count("peer s: first touch message on input-events") == 1 })
	if got := lr.find("peer p: first touch message")[0]; !strings.Contains(got, "slot 1") || strings.Contains(got, "no slot") {
		t.Fatalf("p's first touch line: %s", got)
	}
	if got := lr.find("peer s: first touch message")[0]; !strings.Contains(got, "no slot (its src has no entry") {
		t.Fatalf("s's first touch line: %s", got)
	}

	peerClose(h, "t1", "p")
	peerClose(h, "t1", "s")
	eventually(t, "both summaries", func() bool {
		return lr.count("peer p: input summary") == 1 && lr.count("peer s: input summary") == 1
	})
	ps := lr.find("peer p: input summary")[0]
	ss := lr.find("peer s: input summary")[0]
	for _, want := range []string{" touch=1 ", " key=0 ", " unparseable=0 ", " no_slot=0 "} {
		if !strings.Contains(ps, want) {
			t.Errorf("p summary lacks %q: %s", want, ps)
		}
	}
	for _, want := range []string{" touch=1 ", " no_slot=1 ", " unparseable=0 "} {
		if !strings.Contains(ss, want) {
			t.Errorf("s summary lacks %q: %s", want, ss)
		}
	}
	if strings.Contains(ps, "first_unparseable") {
		t.Errorf("first_unparseable with unparseable=0: %s", ps)
	}
	stopAndWait(t, h, "t1")
	// cleanup ran too: still one summary per peer.
	if a, b := lr.count("peer p: input summary"), lr.count("peer s: input summary"); a != 1 || b != 1 {
		t.Fatalf("summaries after session end: p=%d s=%d, want 1 each", a, b)
	}
	checkNotIdleRegex(t, lr)
}

// Counters: two touch downs log one first-touch line; a bad touch and a
// burst of moves under TouchMinInterval are counted by reason; an
// unparseable message is logged once by size, keys and error class, never by
// value; and a slot taken away logs an unbind.
func TestInputVisibilityCountsAndUnparseable(t *testing.T) {
	const port = 40422
	lr := &logRec{t: t}
	st := &fakeStages{t: t, port: port, prepared: make(chan struct{})}
	h, c := newTestHostLog(t, port, st, lr.logf)
	t.Cleanup(func() { stopAndWait(t, h, "t1") })
	h.OnEvent(protocol.EventSessionStart, startPayload("t1", 600))
	peerOpen(h, "t1", "p")
	slots(h, "t1", 2, map[string]map[string]int{"p": {"0": 1}})
	p := attach(t, h, c, "p", 0)
	eventually(t, "export on", func() bool { return st.game.sawLine(`"on":true`) })

	p.events.SendText(`{"t":"touch","src":0,"id":0,"ph":"down","x":0.5,"y":0.5}`)
	p.events.SendText(`{"t":"touch","src":0,"id":1,"ph":"down","x":0.25,"y":0.5}`)
	p.events.SendText(`{"t":"touch","src":0,"id":42,"ph":"down","x":0.5,"y":0.5}`) // bad_touch
	// A burst of moves on held id 0: pion delivers a queued burst back to
	// back, so moves land under TouchMinInterval (4ms) and are touch_rate.
	const moves = 30
	for i := 0; i < moves; i++ {
		p.events.SendText(fmt.Sprintf(`{"t":"touch","src":0,"id":0,"ph":"move","x":0.%02d,"y":0.5}`, 10+i))
	}
	sent := int64(3 + moves)
	eventually(t, "all touches counted", func() bool { return visCount(h.Current(), "p", touchCount) == sent })
	if n := lr.count("peer p: first touch message"); n != 1 {
		t.Fatalf("first-touch lines = %d after %d touches, want exactly 1: %v", n, sent, lr.find("first touch"))
	}

	badA := `not json SENTINEL-01a0ff61-A`
	p.events.SendText(badA)
	p.events.SendText(`{"t":"touch","x":"SENTINEL-01a0ff61-B"}`) // type mismatch: Decode fails
	eventually(t, "2 unparseable", func() bool {
		return visCount(h.Current(), "p", func(ps *peerState) int64 { return ps.badParse.Load() }) == 2
	})
	bad := lr.find("peer p: unparseable input message on input-events")
	if len(bad) != 1 {
		t.Fatalf("unparseable lines = %d, want 1 (later ones are counted): %v", len(bad), bad)
	}
	if want := fmt.Sprintf("(%d bytes, keys not-json, syntax@", len(badA)); !strings.Contains(bad[0], want) {
		t.Fatalf("unparseable line lacks %q: %s", want, bad[0])
	}

	slots(h, "t1", 2, map[string]map[string]int{"p": {}})
	eventually(t, "unbind", func() bool { return lr.count("peer p: slot 1 unbound (src 0)") == 1 })

	peerClose(h, "t1", "p")
	eventually(t, "summary", func() bool { return lr.count("peer p: input summary") == 1 })
	sum := lr.find("peer p: input summary")[0]
	for _, want := range []string{
		fmt.Sprintf(" touch=%d ", sent), " unparseable=2 ", " bad_touch=1 ", " no_slot=0 ",
		fmt.Sprintf(" first_unparseable=%dB keys not-json", len(badA)),
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q: %s", want, sum)
		}
	}
	var rate int
	if _, err := fmt.Sscanf(sum[strings.Index(sum, "touch_rate="):], "touch_rate=%d", &rate); err != nil || rate < 1 || rate > moves {
		t.Errorf("touch_rate = %d (%v), want 1..%d: %s", rate, err, moves, sum)
	}
	lr.mu.Lock()
	for _, l := range lr.lines {
		if strings.Contains(l, "SENTINEL") {
			t.Errorf("a message value reached the log: %s", l)
		}
	}
	lr.mu.Unlock()
	checkNotIdleRegex(t, lr)
}
