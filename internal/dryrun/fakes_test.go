package dryrun

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/engine"
	"github.com/Startup-Suite/play-host/internal/host"
	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/rtc"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestMain doubles as the fake ffmpeg: it reads raw frames and sends one RTP
// packet per frame to the url the host gave it.
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
	seq, ts := uint16(1), uint32(1500)
	for {
		if _, err := io.ReadFull(os.Stdin, buf); err != nil {
			return
		}
		pkt := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: payloadType, SequenceNumber: seq, Timestamp: ts, Marker: true}, Payload: []byte{0x65, 0x88, 0x84, 0x00}}
		b, _ := pkt.Marshal()
		c.Write(b)
		seq++
		ts += step
	}
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func testHostConfig(t *testing.T, port int) host.Config {
	exe, _ := os.Executable()
	t.Setenv("FAKE_FFMPEG", "1")
	rtc.DisconnectGrace = 200 * time.Millisecond
	return host.Config{
		FFmpeg: exe, LogsDir: t.TempDir(), HostIP: "127.0.0.1", UDPMin: uint16(port - 9000), UDPMax: uint16(port - 9000),
		GodotPort: port, RTPPort: port + 1, ProgressEvery: 20 * time.Millisecond, IdleCheck: 20 * time.Millisecond,
		LinkTimeout: 5 * time.Second, Loopback: true, ResumeGrace: 50 * time.Millisecond,
	}
}

// scriptedBuild is the fake UAT: the banners a RunUAT would print, as
// building details that start with the step word, spaced by step.
func scriptedBuild(progress func(string), steps map[string]time.Duration, order []string) {
	for _, s := range order {
		progress(s + ": working")
		time.Sleep(steps[s])
	}
}

var unrealOrder = []string{"Preparing", "Compiling", "Cooking", "Staging", "Packaging", "Archiving"}

// unrealStages is an Unreal checkout whose capture is built by capture.Start
// over fakes and whose frames are pushed by a fake source.
type unrealStages struct {
	t       *testing.T
	session uint32
	builds  atomic.Int64
	steps   map[string]time.Duration
}

type fakeProc struct {
	done chan struct{}
	once sync.Once
}

func (p *fakeProc) Pid() int             { return 4343 }
func (p *fakeProc) Wait() (int, error)   { <-p.done; return 1, nil }
func (p *fakeProc) Kill() error          { p.once.Do(func() { close(p.done) }); return nil }
func (p *fakeProc) Pids() ([]int, error) { return []int{4343}, nil }

type fakeEnv struct{ id uint32 }

func (e fakeEnv) SessionID() (uint32, error)     { return e.id, nil }
func (e fakeEnv) ActiveConsoleSessionID() uint32 { return 1 }

type fakeFinder struct{}

func (fakeFinder) Windows([]int) ([]capture.Window, error) {
	return []capture.Window{{Handle: 1, Visible: true, Client: capture.Rect{X: 0, Y: 0, W: 64, H: 48}}}, nil
}

type fakeDesktop struct{}

func (fakeDesktop) Monitors() ([]capture.Rect, error) {
	return []capture.Rect{{X: 0, Y: 0, W: 1920, H: 1080}}, nil
}

func (u *unrealStages) Prepare(ctx context.Context, s protocol.SessionStart, progress func(string)) (string, error) {
	dir := filepath.Join(u.t.TempDir(), "checkout")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "Sample.uproject"), []byte("{}"), 0o644)
	u.builds.Add(1)
	scriptedBuild(progress, u.steps, unrealOrder)
	return dir, nil
}
func (u *unrealStages) AssertHead(context.Context, string, string) error { return nil }
func (u *unrealStages) Remove(_ context.Context, _ protocol.SessionStart, dir string) error {
	return os.RemoveAll(dir)
}
func (u *unrealStages) Launch(string, protocol.SessionStart, string) (launch.Proc, error) {
	return &fakeProc{done: make(chan struct{})}, nil
}
func (u *unrealStages) Engine(dir string) (engine.Kind, error) { return engine.Detect(dir) }
func (u *unrealStages) Capture(ctx context.Context, p launch.Proc, s protocol.SessionStart) (capture.Source, error) {
	pushed := &capture.Pushed{Label: "fake", Run: func(ctx context.Context, deliver capture.FrameFunc) error {
		frame := make([]byte, 64*48*4)
		for ctx.Err() == nil {
			if err := deliver(frame, 64, 48); err != nil {
				return err
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}}
	return capture.Start(ctx, capture.Config{
		Backend: capture.BackendWGC, FPS: s.Encoder.FPS, Timeout: 150 * time.Millisecond, Poll: 5 * time.Millisecond,
		Pids: p.Pids, Env: fakeEnv{u.session}, Finder: fakeFinder{}, Desktop: fakeDesktop{},
		NewWGC: func(capture.Window, capture.Rect) (capture.Source, error) { return pushed, nil },
	})
}

// godotStages is the addon path: a fake game on the link port, no EngineStages.
type godotStages struct {
	t    *testing.T
	port int
}

type fakeGame struct {
	ln     net.Listener
	export atomic.Bool
	done   chan struct{}
	once   sync.Once
}

func (g *godotStages) Prepare(ctx context.Context, s protocol.SessionStart, progress func(string)) (string, error) {
	progress("Importing assets (0s)")
	time.Sleep(30 * time.Millisecond)
	progress("Importing assets (1s)")
	time.Sleep(30 * time.Millisecond)
	dir := filepath.Join(g.t.TempDir(), "checkout")
	os.MkdirAll(dir, 0o755)
	return dir, nil
}
func (g *godotStages) AssertHead(context.Context, string, string) error { return nil }
func (g *godotStages) Remove(_ context.Context, _ protocol.SessionStart, dir string) error {
	return os.RemoveAll(dir)
}
func (g *godotStages) Launch(string, protocol.SessionStart, string) (launch.Proc, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", g.port))
	if err != nil {
		return nil, err
	}
	fg := &fakeGame{ln: ln, done: make(chan struct{})}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				if bytes.Contains(buf[:n], []byte(`"export"`)) {
					fg.export.Store(bytes.Contains(buf[:n], []byte(`"on":true`)))
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
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-fg.done:
				return
			case <-tk.C:
				if fg.export.Load() {
					if _, err := c.Write(append(append([]byte(nil), hdr...), frame...)); err != nil {
						return
					}
				}
			}
		}
	}()
	return &godotProc{fg}, nil
}

type godotProc struct{ g *fakeGame }

func (p *godotProc) Pid() int             { return 4242 }
func (p *godotProc) Wait() (int, error)   { <-p.g.done; return 1, nil }
func (p *godotProc) Pids() ([]int, error) { return []int{4242}, nil }
func (p *godotProc) Kill() error {
	p.g.once.Do(func() { close(p.g.done); p.g.ln.Close() })
	return nil
}

// fakeProber is the browser: it completes the real offer/answer exchange with
// a pion peer, waits for video, then answers n probes with a fixed spread.
type fakeProber struct {
	t    *testing.T
	runs atomic.Int64
}

func (f *fakeProber) Probe(ctx context.Context, url, label string, n int) (ProbeResult, error) {
	f.runs.Add(1)
	resp, err := http.Post(url+"/offer", "application/json", nil)
	if err != nil {
		return ProbeResult{}, err
	}
	var offer struct{ ID, SDP string }
	err = json.NewDecoder(resp.Body).Decode(&offer)
	resp.Body.Close()
	if err != nil || offer.SDP == "" {
		return ProbeResult{}, fmt.Errorf("offer: %v", err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return ProbeResult{}, err
	}
	defer pc.Close()
	var pkts atomic.Int64
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			pkts.Add(1)
		}
	})
	pc.OnDataChannel(func(*webrtc.DataChannel) {})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return ProbeResult{}, err
	}
	ans, _ := pc.CreateAnswer(nil)
	g := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(ans)
	<-g
	body, _ := json.Marshal(map[string]string{"ID": offer.ID, "SDP": pc.LocalDescription().SDP})
	r2, err := http.Post(url+"/answer", "application/json", bytes.NewReader(body))
	if err != nil {
		return ProbeResult{}, err
	}
	r2.Body.Close()
	end := time.Now().Add(10 * time.Second)
	for pkts.Load() < 4 {
		if time.Now().After(end) {
			return ProbeResult{}, fmt.Errorf("no video from the host")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 25..44 ms in a fixed pattern: p50 and p95 differ.
	res := ProbeResult{FPS: 59.5}
	for i := 0; i < n; i++ {
		res.RTTMs = append(res.RTTMs, float64(25+(i*7)%20))
	}
	return res, nil
}
