package host

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/rtc"
)

// Task 01a1a358 stage 2: with per_slot on, each peer's connection carries
// the encode of its lowest slot (the shared encode for a spectator), swapped
// with ReplaceTrack and announced with play_peer_view.

func (c *core) viewList() []protocol.PeerView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.PeerView(nil), c.views...)
}

func (c *core) traceList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.trace...)
}

func (c *core) viewsOf(id string) []string {
	var out []string
	for _, t := range c.traceList() {
		if strings.HasPrefix(t, "view "+id+" ") {
			out = append(out, t)
		}
	}
	return out
}

func perSlotHost(t *testing.T, port int, sid string) (*Host, *core, *logRec) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	lr := &logRec{t: t}
	st := &fakeStages{t: t, port: port, prepared: make(chan struct{})}
	h, c := newTestHostLog(t, port, st, lr.logf)
	h.cfg.PerSlot = true
	t.Cleanup(func() { stopAndWait(t, h, sid) })
	return h, c, lr
}

func TestPerSlotTwoSlotsAndSpectatorThreePipelines(t *testing.T) {
	h, c, lr := perSlotHost(t, 40420, "ps1")
	h.OnEvent(protocol.EventSessionStart, startPayload("ps1", 600))
	for _, id := range []string{"p1", "p2", "p3"} {
		peerOpen(h, "ps1", id)
	}
	slots(h, "ps1", 4, map[string]map[string]int{"p1": {"0": 0}, "p2": {"0": 1}, "p3": {}})
	v := map[string]*attached{}
	for _, id := range []string{"p1", "p2", "p3"} {
		v[id] = attach(t, h, c, id, 0)
	}
	for id, a := range v {
		growing(t, "RTP at "+id, a.pkts)
	}
	eventually(t, "three encoders", func() bool { return lr.count("encoder started") == 3 })
	for _, want := range []string{"view=0", "view=1", "view=2"} {
		if n := len(lr.find("encoder started")); n != 3 {
			t.Fatalf("encoders %d", n)
		}
		found := false
		for _, l := range lr.find("encoder started") {
			found = found || strings.Contains(l, want) || (want == "view=0" && !strings.Contains(l, "view="))
		}
		if !found {
			t.Errorf("no encoder for %s", want)
		}
	}
	eventually(t, "three views", func() bool { return len(c.viewList()) == 3 })
	c.mu.Lock()
	raw := map[string]string{}
	for i, pv := range c.views {
		raw[pv.PeerID] = c.viewRaw[i]
	}
	c.mu.Unlock()
	want := map[string]string{
		"p1": `{"session_id":"ps1","peer_id":"p1","view":"slot","slot":0}`,
		"p2": `{"session_id":"ps1","peer_id":"p2","view":"slot","slot":1}`,
		"p3": `{"session_id":"ps1","peer_id":"p3","view":"shared"}`,
	}
	if !reflect.DeepEqual(raw, want) {
		t.Fatalf("views %v, want %v", raw, want)
	}
	if strings.Contains(raw["p3"], "slot") && strings.Contains(raw["p3"], `"slot"`) {
		t.Fatalf("spectator frame carries a slot key: %s", raw["p3"])
	}
	for _, id := range []string{"p1", "p2", "p3"} {
		if n := len(c.offersFor(id)); n != 1 {
			t.Errorf("peer %s: %d offers", id, n)
		}
	}
}

func TestPerSlotLowestSlotWins(t *testing.T) {
	h, c, _ := perSlotHost(t, 40422, "ps2")
	h.OnEvent(protocol.EventSessionStart, startPayload("ps2", 600))
	peerOpen(h, "ps2", "p1")
	slots(h, "ps2", 4, map[string]map[string]int{"p1": {"0": 2, "1": 0}})
	a := attach(t, h, c, "p1", 0)
	growing(t, "RTP at p1", a.pkts)
	eventually(t, "one view", func() bool { return len(c.viewList()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := fmt.Sprint(c.traceList()); got != "[view p1 slot 0]" {
		t.Fatalf("trace %s, want one view of slot 0", got)
	}
}

// per_slot off: one encoder, no play_peer_view, no event name beyond status
// and signal (the shape TestFanOutThreePeersOneEncoder records).
func TestPerSlotOffIsByteIdentical(t *testing.T) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	const port = 40424
	lr := &logRec{t: t}
	st := &fakeStages{t: t, port: port, prepared: make(chan struct{})}
	h, c := newTestHostLog(t, port, st, lr.logf)
	t.Cleanup(func() { stopAndWait(t, h, "ps3") })
	h.OnEvent(protocol.EventSessionStart, startPayload("ps3", 600))
	for _, id := range []string{"p1", "p2", "p3"} {
		peerOpen(h, "ps3", id)
	}
	slots(h, "ps3", 4, map[string]map[string]int{"p1": {"0": 0}, "p2": {"0": 1}, "p3": {}})
	for _, id := range []string{"p1", "p2", "p3"} {
		growing(t, "RTP at "+id, attach(t, h, c, id, 0).pkts)
	}
	slots(h, "ps3", 4, map[string]map[string]int{"p1": {"0": 0}, "p2": {}, "p3": {}})
	time.Sleep(200 * time.Millisecond)
	if n := lr.count("encoder started"); n != 1 {
		t.Fatalf("encoder started %d times", n)
	}
	if got := c.states("ps3"); fmt.Sprint(got) != "[building connecting live]" {
		t.Errorf("states %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e != protocol.EventSessionStatus && e != protocol.EventSignal {
			t.Errorf("unexpected pushed event %q", e)
		}
	}
	if len(c.views) != 0 {
		t.Fatalf("play_peer_view with per_slot off: %v", c.views)
	}
}

func TestPerSlotSlotLeaveAndRejoinReplacesTrackNoReoffer(t *testing.T) {
	h, c, lr := perSlotHost(t, 40426, "ps4")
	h.OnEvent(protocol.EventSessionStart, startPayload("ps4", 600))
	peerOpen(h, "ps4", "p1")
	peerOpen(h, "ps4", "p2")
	slots(h, "ps4", 4, map[string]map[string]int{"p1": {"0": 0}, "p2": {"0": 1}})
	a1 := attach(t, h, c, "p1", 0)
	a2 := attach(t, h, c, "p2", 0)
	growing(t, "RTP at p1", a1.pkts)
	growing(t, "RTP at p2", a2.pkts)
	eventually(t, "slot encoder 1 started", func() bool { return lr.count("view=2") == 1 })

	slots(h, "ps4", 4, map[string]map[string]int{"p1": {"0": 0}, "p2": {}})
	eventually(t, "p2 shared", func() bool { return len(c.viewsOf("p2")) == 2 })
	if got := c.viewsOf("p2")[1]; got != "view p2 shared -" {
		t.Fatalf("p2 leave: %s", got)
	}
	eventually(t, "slot 1 view dropped", func() bool { return lr.count("slot 1 view dropped") == 1 })
	growing(t, "RTP at p2 after leaving", a2.pkts)

	slots(h, "ps4", 4, map[string]map[string]int{"p1": {"0": 0}, "p2": {"0": 2}})
	eventually(t, "p2 slot 2", func() bool { return len(c.viewsOf("p2")) == 3 })
	if got := c.viewsOf("p2")[2]; got != "view p2 slot 2" {
		t.Fatalf("p2 rejoin: %s", got)
	}
	eventually(t, "new encoder for view 3", func() bool { return lr.count("view=3") == 1 })
	growing(t, "RTP at p2 after rejoining", a2.pkts)
	if n := len(c.offersFor("p2")); n != 1 {
		t.Fatalf("p2 was re-offered: %d offers", n)
	}
	if n := len(c.viewsOf("p1")); n != 1 {
		t.Fatalf("p1 got %d views", n)
	}
}

func TestPerSlotResumeResendsOneViewPerPeer(t *testing.T) {
	h, c, _ := perSlotHost(t, 40428, "ps5")
	h.cfg.ResumeGrace = 2 * time.Second
	h.OnEvent(protocol.EventSessionStart, startPayload("ps5", 600))
	peerOpen(h, "ps5", "p1")
	peerOpen(h, "ps5", "p3")
	slots(h, "ps5", 4, map[string]map[string]int{"p1": {"0": 0}, "p3": {}})
	a1 := attach(t, h, c, "p1", 0)
	a3 := attach(t, h, c, "p3", 0)
	growing(t, "RTP at p1", a1.pkts)
	growing(t, "RTP at p3", a3.pkts)
	eventually(t, "first views", func() bool { return len(c.viewList()) == 2 })

	h.OnDisconnected(errors.New("test drop"))
	time.Sleep(100 * time.Millisecond)
	h.OnConnected()
	eventually(t, "resume then two views", func() bool {
		tr := c.traceList()
		return len(tr) == 5
	})
	time.Sleep(200 * time.Millisecond)
	tr := c.traceList()
	if len(tr) != 5 || tr[2] != "resume" {
		t.Fatalf("trace %v", tr)
	}
	if got := tr[3:]; got[0] != "view p1 slot 0" || got[1] != "view p3 shared -" {
		t.Fatalf("after the resume: %v", got)
	}
	r := c.resumeList()
	if len(r) != 1 || fmt.Sprint(r[0].Peers) != "[p1 p3]" {
		t.Fatalf("resume %+v", r)
	}
	if n := len(c.offersFor("p1")) + len(c.offersFor("p3")); n != 2 {
		t.Fatalf("offers %d", n)
	}
}

func TestPerSlotV1PeerNeverEmits(t *testing.T) {
	h, c, lr := perSlotHost(t, 40430, "ps6")
	h.OnEvent(protocol.EventSessionStart, startPayload("ps6", 600))
	offer := <-c.offer
	pc, _, pkts := viewer(t, h, offer)
	defer pc.Close()
	growing(t, "RTP at the v1 peer", pkts)
	time.Sleep(200 * time.Millisecond)
	if v := c.viewList(); len(v) != 0 {
		t.Fatalf("v1 peer got views: %v", v)
	}
	if n := lr.count("encoder started"); n != 1 {
		t.Fatalf("encoder started %d times for the v1 peer", n)
	}
}

func TestPeerViewJSONShape(t *testing.T) {
	two := 2
	for _, tc := range []struct {
		pv   protocol.PeerView
		want string
	}{
		{protocol.PeerView{SessionID: "s", PeerID: "p", View: protocol.ViewSlot, Slot: &two}, `{"session_id":"s","peer_id":"p","view":"slot","slot":2}`},
		{protocol.PeerView{SessionID: "s", PeerID: "p", View: protocol.ViewShared}, `{"session_id":"s","peer_id":"p","view":"shared"}`},
	} {
		b, _ := json.Marshal(tc.pv)
		if string(b) != tc.want {
			t.Errorf("got %s, want %s", b, tc.want)
		}
	}
}

// An SPF2 record with an out-of-range view is skipped whole: logged and
// counted once, the link stays in sync, and the next SPF1 frame still
// reaches the shared pipeline.
func TestPerSlotMalformedSPF2ViewIsDroppedLinkAlive(t *testing.T) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	lr := &logRec{t: t}
	st := &fakeStages{t: t, port: 40440, prepared: make(chan struct{})}
	h, c := newTestHostLog(t, 40440, st, lr.logf)
	h.cfg.PerSlot = true
	t.Cleanup(func() { stopAndWait(t, h, "ps9") })
	h.OnEvent(protocol.EventSessionStart, startPayload("ps9", 600))
	peerOpen(h, "ps9", "p1")
	slots(h, "ps9", 4, map[string]map[string]int{"p1": {}})
	a := attach(t, h, c, "p1", 0)
	growing(t, "RTP before the bad record", a.pkts)

	hdr := make([]byte, 24)
	copy(hdr, "SPF2")
	binary.LittleEndian.PutUint32(hdr[4:], 64)
	binary.LittleEndian.PutUint32(hdr[8:], 48)
	binary.LittleEndian.PutUint32(hdr[12:], 1)
	binary.LittleEndian.PutUint32(hdr[16:], 64*48*4)
	binary.LittleEndian.PutUint32(hdr[20:], 40)
	bad := append(hdr, make([]byte, 64*48*4)...)
	for i := 0; i < 3; i++ {
		st.game.inject <- bad
	}
	eventually(t, "bad records logged", func() bool { return lr.count("dropped malformed addon record") == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := lr.count("dropped malformed addon record"); n != 1 {
		t.Fatalf("logged %d times, want once", n)
	}
	if n := h.Current().badRecords.Load(); n != 3 {
		t.Fatalf("counted %d, want 3", n)
	}
	before := a.pkts.Load()
	eventually(t, "SPF1 still reaches the pipeline", func() bool { return a.pkts.Load() > before+5 })
	if n := lr.count("addon link:"); n != 0 {
		t.Fatalf("link failed: %v", lr.find("addon link:"))
	}
}
