package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/rtc"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestMain doubles as a fake ffmpeg (see internal/media's pipeline test).
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_FFMPEG") == "1" {
		fakeFFmpeg()
		return
	}
	os.Exit(m.Run())
}

func fakeFFmpeg() {
	var size, url string
	for i, a := range os.Args {
		if a == "-video_size" {
			size = os.Args[i+1]
		}
		if strings.HasPrefix(a, "rtp://") {
			url = strings.TrimPrefix(strings.SplitN(a, "?", 2)[0], "rtp://")
		}
	}
	c, err := net.Dial("udp4", url)
	if err != nil {
		os.Exit(3)
	}
	payloadType, step := uint8(96), uint32(1500)
	var buf []byte
	if size == "" {
		payloadType, step, buf = 111, 480, make([]byte, 8)
	} else {
		var w, h int
		fmt.Sscanf(size, "%dx%d", &w, &h)
		buf = make([]byte, w*h*4)
	}
	seq, timestamp := uint16(1), uint32(1500)
	for {
		if _, err := io.ReadFull(os.Stdin, buf); err != nil {
			return
		}
		pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: payloadType, SequenceNumber: seq, Timestamp: timestamp, Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}}
		b, _ := pkt.Marshal()
		c.Write(b)
		seq++
		timestamp += step
	}
}

// fakeGame is the addon: it listens on the link port, records every line
// the host sends, and exports small RGBA frames while export is on.
type fakeGame struct {
	ln      net.Listener
	mu      sync.Mutex
	lines   []string
	export  atomic.Bool
	done    chan struct{}
	exitNow chan int
	killed  atomic.Bool
}

func startFakeGame(t *testing.T, port int) *fakeGame {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGame{ln: ln, done: make(chan struct{}), exitNow: make(chan int, 1)}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close() // after EOF: the host closes the link before it kills the job
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				l := sc.Text()
				g.mu.Lock()
				g.lines = append(g.lines, l)
				g.mu.Unlock()
				var m struct {
					T  string `json:"t"`
					On bool   `json:"on"`
				}
				if json.Unmarshal([]byte(l), &m) == nil && m.T == "export" {
					g.export.Store(m.On)
				}
			}
		}()
		frame := make([]byte, 64*48*4)
		hdr := make([]byte, 20)
		copy(hdr, "SPF1")
		binary.LittleEndian.PutUint32(hdr[4:], 64)
		binary.LittleEndian.PutUint32(hdr[8:], 48)
		binary.LittleEndian.PutUint32(hdr[12:], 1)
		binary.LittleEndian.PutUint32(hdr[16:], uint32(len(frame)))
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-g.done:
				return
			case <-t.C:
				if g.export.Load() {
					if _, err := c.Write(append(append([]byte(nil), hdr...), frame...)); err != nil {
						return
					}
				}
			}
		}
	}()
	return g
}

func (g *fakeGame) sawLine(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, l := range g.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// fakeProc stands in for the Godot job.
type fakeProc struct{ g *fakeGame }

func (p *fakeProc) Pid() int { return 4242 }
func (p *fakeProc) Wait() (int, error) {
	select {
	case code := <-p.g.exitNow:
		return code, nil
	case <-p.g.done:
		return 1, nil
	}
}
func (p *fakeProc) Kill() error {
	if p.g.killed.CompareAndSwap(false, true) {
		close(p.g.done)
		p.g.ln.Close()
	}
	return nil
}
func (p *fakeProc) Pids() ([]int, error) { return []int{4242}, nil }

type fakeStages struct {
	t        *testing.T
	port     int
	game     *fakeGame
	prepared chan struct{}
	block    chan struct{} // if non-nil, Prepare waits on it
	headErr  error
	mu       sync.Mutex
	dir      string
	removed  []string
}

func (f *fakeStages) Prepare(ctx context.Context, s protocol.SessionStart, progress func(string)) (string, error) {
	progress("Importing assets (0s)")
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	close(f.prepared)
	dir := filepath.Join(f.t.TempDir(), "checkout")
	os.MkdirAll(dir, 0o755)
	f.mu.Lock()
	f.dir = dir
	f.mu.Unlock()
	return dir, nil
}
func (f *fakeStages) AssertHead(ctx context.Context, dir, sha string) error { return f.headErr }
func (f *fakeStages) Remove(ctx context.Context, s protocol.SessionStart, dir string) error {
	f.mu.Lock()
	f.removed = append(f.removed, dir)
	f.mu.Unlock()
	return os.RemoveAll(dir)
}
func (f *fakeStages) removedDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}
func (f *fakeStages) Launch(dir string, s protocol.SessionStart, logPath string) (launch.Proc, error) {
	f.game = startFakeGame(f.t, f.port)
	return &fakeProc{g: f.game}, nil
}

// core records pushes and lets the test answer the offer like a browser.
type core struct {
	mu       sync.Mutex
	resumes  []protocol.ResumeStatus
	statuses []protocol.Status
	signals  []protocol.Signal
	offer    chan protocol.Signal
	events   []string // every event name, in order
	activity []protocol.SlotActivity
}

func (c *core) Push(event string, payload any) error {
	b, _ := json.Marshal(payload)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
	switch event {
	case protocol.EventSlotActivity:
		var a protocol.SlotActivity
		json.Unmarshal(b, &a)
		c.activity = append(c.activity, a)
	case protocol.EventSessionStatus:
		var s protocol.Status
		json.Unmarshal(b, &s)
		c.statuses = append(c.statuses, s)
		var r protocol.ResumeStatus
		if json.Unmarshal(b, &r) == nil && r.Resume {
			c.resumes = append(c.resumes, r)
		}
	case protocol.EventSignal:
		var s protocol.Signal
		json.Unmarshal(b, &s)
		c.signals = append(c.signals, s)
		if s.Kind == protocol.KindOffer {
			// Never block the session loop that pushes (a multi-peer test
			// reads offers by peer through offersFor instead).
			select {
			case c.offer <- s:
			default:
			}
		}
	}
	return nil
}

func (c *core) states(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.statuses {
		if s.SessionID == id && (len(out) == 0 || out[len(out)-1] != s.State) {
			out = append(out, s.State)
		}
	}
	return out
}

func (c *core) last(id string) protocol.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.statuses) - 1; i >= 0; i-- {
		if c.statuses[i].SessionID == id {
			return c.statuses[i]
		}
	}
	return protocol.Status{}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for !f() {
		if time.Now().After(end) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func startPayload(id string, idle int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"session_id": id, "task_id": "01a0db5f-4f1a-7001-87a3-e777ad1cab82", "repo_url": "https://github.com/example-owner/example-game",
		"branch": "task/x", "sha": sha, "ice_servers": []any{}, "idle_timeout_s": idle,
		"encoder": map[string]any{"preset": "p1", "tune": "ll", "bitrate_kbps": 8000, "fps": 60, "width": 1280, "height": 720, "gop_frames": 120},
	})
	return b
}

func newTestHost(t *testing.T, port int, stages *fakeStages) (*Host, *core) {
	return newTestHostLog(t, port, stages, t.Logf)
}

// muxPort is the one UDP port a test session's peers share (task 01a0dbd6:
// rtc.NewMuxAPI). It sits below 32768, outside Linux's ephemeral range, so
// no pion answerer or fake-ffmpeg socket can already hold it (measured on
// host-b: 40366 was taken by an ephemeral socket), and is unique per test port.
func muxPort(port int) int { return port - 9000 }

// newTestHostLog is newTestHost with the host's log sent to logf.
func newTestHostLog(t *testing.T, port int, stages *fakeStages, logf func(string, ...any)) (*Host, *core) {
	exe, _ := os.Executable()
	t.Setenv("FAKE_FFMPEG", "1")
	c := &core{offer: make(chan protocol.Signal, 16)}
	h := New(Config{
		FFmpeg: exe, LogsDir: t.TempDir(), HostIP: "127.0.0.1", UDPMin: uint16(muxPort(port)), UDPMax: uint16(muxPort(port)),
		GodotPort: port, RTPPort: port + 1, ProgressEvery: 20 * time.Millisecond, IdleCheck: 20 * time.Millisecond, LinkTimeout: 5 * time.Second, Loopback: true,
		// A dropped suite socket keeps the session this long (stage 6); short,
		// so the cases that drop the socket to end a session stay fast.
		ResumeGrace: 50 * time.Millisecond,
	}, c, stages, logf)
	return h, c
}

// viewer answers an offer the way the browser will: data is an
// RTCSessionDescriptionInit, relayed by core as play_signal answer.
func viewer(t *testing.T, h *Host, offer protocol.Signal) (*webrtc.PeerConnection, chan *webrtc.DataChannel, *atomic.Int64) {
	t.Helper()
	d, err := offer.Description()
	if err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	m := &webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	var pkts atomic.Int64
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			pkts.Add(1)
		}
	})
	dcs := make(chan *webrtc.DataChannel, 2)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) { dc.OnOpen(func() { dcs <- dc }) })
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP}); err != nil {
		t.Fatal(err)
	}
	ans, _ := pc.CreateAnswer(nil)
	g := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(ans)
	<-g
	data, _ := json.Marshal(protocol.SessionDescription{Type: "answer", SDP: pc.LocalDescription().SDP})
	// Core tags the answer with the peer the offer was for (empty for v1).
	sig, _ := json.Marshal(protocol.Signal{SessionID: offer.SessionID, Kind: protocol.KindAnswer, Data: data, PeerID: offer.PeerID})
	h.OnEvent(protocol.EventSignal, sig)
	return pc, dcs, &pkts
}

func TestSessionReachesLiveForwardsInputAndStops(t *testing.T) {
	rtc.DisconnectGrace = 200 * time.Millisecond
	st := &fakeStages{t: t, port: 40350, prepared: make(chan struct{})}
	h, c := newTestHost(t, 40350, st)
	h.OnEvent(protocol.EventSessionStart, startPayload("s1", 600))
	offer := <-c.offer
	pc, dcs, pkts := viewer(t, h, offer)
	defer pc.Close()
	eventually(t, "live", func() bool { return c.last("s1").State == protocol.StateLive })
	eventually(t, "video packets at the viewer", func() bool { return pkts.Load() > 3 })
	if !strings.Contains(c.last("s1").Detail, "64x48") {
		t.Errorf("live detail should report the real frame size: %q", c.last("s1").Detail)
	}

	var events *webrtc.DataChannel
	for events == nil {
		dc := <-dcs
		if dc.Label() == rtc.LabelInputEvents {
			events = dc
		}
	}
	events.SendText(`{"t":"key","code":"KeyW","down":true}`)
	eventually(t, "key line at the addon", func() bool { return st.game.sawLine(`"k":"W"`) && st.game.sawLine(`"p":true`) })

	// A second start while busy answers failed/host busy for the NEW id only.
	h.OnEvent(protocol.EventSessionStart, startPayload("s2", 600))
	eventually(t, "host busy", func() bool { return c.last("s2").State == protocol.StateFailed })
	if c.last("s2").Detail != "host busy" || c.last("s1").State != protocol.StateLive {
		t.Fatalf("busy handling: s2 %+v s1 %+v", c.last("s2"), c.last("s1"))
	}

	stop, _ := json.Marshal(protocol.SessionStop{SessionID: "s1", Reason: "Stopped by reviewer"})
	h.OnEvent(protocol.EventSessionStop, stop)
	eventually(t, "ended", func() bool { return c.last("s1").State == protocol.StateEnded })
	if got := c.last("s1").Detail; got != "Stopped by reviewer" {
		t.Errorf("ended detail %q", got)
	}
	if !st.game.killed.Load() {
		t.Error("godot job not killed on stop")
	}
	eventually(t, "release_all before teardown", func() bool { return st.game.sawLine(`"release_all"`) })
	want := []string{"building", "connecting", "live", "ended"}
	if got := c.states("s1"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("state sequence %v, want %v", got, want)
	}
	eventually(t, "session slot freed", func() bool { return h.Current() == nil })
}

func TestIdleEndsSession(t *testing.T) {
	st := &fakeStages{t: t, port: 40352, prepared: make(chan struct{})}
	h, c := newTestHost(t, 40352, st)
	// idle_timeout_s is whole seconds on the wire; 1 s is the smallest.
	h.OnEvent(protocol.EventSessionStart, startPayload("idle1", 1))
	<-c.offer
	eventually(t, "idle end", func() bool { return c.last("idle1").State == protocol.StateEnded })
	if c.last("idle1").Detail != "idle" || !st.game.killed.Load() {
		t.Fatalf("idle: %+v killed=%v", c.last("idle1"), st.game.killed.Load())
	}
}

func TestBuildingProgressAndFailures(t *testing.T) {
	st := &fakeStages{t: t, port: 40354, prepared: make(chan struct{}), block: make(chan struct{})}
	h, c := newTestHost(t, 40354, st)
	h.OnEvent(protocol.EventSessionStart, startPayload("b1", 600))
	// Progress ticks repeat `building` with a growing elapsed_ms.
	eventually(t, "progress ticks", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := 0
		var last int64 = -1
		for _, s := range c.statuses {
			if s.State == protocol.StateBuilding && s.ElapsedMs > last {
				n++
				last = s.ElapsedMs
			}
		}
		return n >= 3
	})
	if !strings.Contains(c.last("b1").Detail, "Importing") {
		t.Errorf("progress detail %q", c.last("b1").Detail)
	}
	// A disconnect from core that is not followed by a rejoin within the
	// resume grace tears the session down without a status.
	h.OnDisconnected(fmt.Errorf("test drop"))
	eventually(t, "session slot freed", func() bool { return h.Current() == nil })
	if c.last("b1").State != protocol.StateBuilding {
		t.Fatalf("a status was pushed after the disconnect: %+v", c.last("b1"))
	}

	// HEAD drift right before launch fails the session and launches nothing.
	st2 := &fakeStages{t: t, port: 40356, prepared: make(chan struct{}), headErr: fmt.Errorf("checkout HEAD is not the requested sha")}
	h2, c2 := newTestHost(t, 40356, st2)
	h2.OnEvent(protocol.EventSessionStart, startPayload("b2", 600))
	eventually(t, "failed on head", func() bool { return c2.last("b2").State == protocol.StateFailed })
	if st2.game != nil {
		t.Fatal("godot launched despite a HEAD mismatch")
	}

	// A bad start payload with an id answers failed.
	h2.OnEvent(protocol.EventSessionStart, json.RawMessage(`{"session_id":"b3","sha":"nope"}`))
	eventually(t, "failed on payload", func() bool { return c2.last("b3").State == protocol.StateFailed })
}

func TestGameExitFailsSession(t *testing.T) {
	st := &fakeStages{t: t, port: 40358, prepared: make(chan struct{})}
	h, c := newTestHost(t, 40358, st)
	h.OnEvent(protocol.EventSessionStart, startPayload("x1", 600))
	<-c.offer
	st.game.exitNow <- 3
	eventually(t, "failed", func() bool { return c.last("x1").State == protocol.StateFailed })
	if !strings.Contains(c.last("x1").Detail, "code 3") {
		t.Fatalf("detail %q", c.last("x1").Detail)
	}
}
