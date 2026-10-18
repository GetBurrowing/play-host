package rtc

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// A pion answerer stands in for the browser: the host offer must connect on
// loopback inside the fixed port range and deliver input-events messages.
func TestOfferAnswerLoopbackDeliversInputEvents(t *testing.T) {
	cfg := Config{PortMin: 40380, PortMax: 40389, AllowIPs: []net.IP{net.ParseIP("127.0.0.1")}, IncludeLoopback: true}
	api, err := NewAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	track, err := NewVideoTrackRTP()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	connected := make(chan struct{}, 1)
	host, offer, err := NewPeer(api, cfg, track, Callbacks{
		OnData:      func(label string, data []byte) { got <- label + ":" + string(data) },
		OnConnected: func() { connected <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	viewer, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	labels := make(chan string, 2)
	viewer.OnDataChannel(func(dc *webrtc.DataChannel) {
		labels <- dc.Label()
		if dc.Label() == LabelInputEvents {
			dc.OnOpen(func() { _ = dc.SendText(`{"t":"probe","seq":7}`) })
		}
	})
	if err := viewer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		t.Fatal(err)
	}
	ans, err := viewer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	g := webrtc.GatheringCompletePromise(viewer)
	if err := viewer.SetLocalDescription(ans); err != nil {
		t.Fatal(err)
	}
	<-g
	// A candidate trickled before the answer is queued, not an error.
	if err := host.AddICECandidate(webrtc.ICECandidateInit{Candidate: ""}); err != nil {
		t.Fatal(err)
	}
	if err := host.SetAnswer(viewer.LocalDescription().SDP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("OnConnected never fired")
	}
	select {
	case m := <-got:
		if m != `input-events:{"t":"probe","seq":7}` {
			t.Fatalf("got %q", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no input-events message; host state %s", host.State())
	}
	// "host 127.0.0.1:403xx <-> ..."
	f := strings.Fields(host.SelectedPair())
	_, port, _ := net.SplitHostPort(f[1])
	if len(f) < 2 || port < "40380" || port > "40389" {
		t.Fatalf("selected pair %q outside port range", host.SelectedPair())
	}
}

// Stage 6: a TURN server that never answers (the hairpinned relay, at its
// worst) must not turn into "ice gathering timed out" and a failed session:
// after GatherTimeout the offer goes out with the host candidates it has.
func TestGatherTimeoutStillOffersHostCandidates(t *testing.T) {
	old := GatherTimeout
	GatherTimeout = 700 * time.Millisecond
	defer func() { GatherTimeout = old }()
	// 192.0.2.0/24 is TEST-NET-1: nothing answers there.
	cfg := Config{PortMin: 40380, PortMax: 40389, IncludeLoopback: true,
		ICEServers: []webrtc.ICEServer{{URLs: []string{"turn:192.0.2.1:3478"}, Username: "u", Credential: "c"}}}
	api, err := NewAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	track, _ := NewVideoTrackRTP()
	p, sdp, g, err := NewPeerGather(api, cfg, track, Callbacks{})
	if err != nil {
		t.Fatalf("offer failed: %v (%s)", err, g)
	}
	defer p.Close()
	if !g.Partial || g.Counts["host"] == 0 || g.Counts["relay"] != 0 {
		t.Fatalf("gather %s", g)
	}
	if !strings.Contains(sdp, "typ host") {
		t.Fatalf("offer has no host candidate:\n%s", sdp)
	}
}

func TestCountCandidates(t *testing.T) {
	sdp := "v=0\r\na=candidate:1 1 udp 2130706431 <host-a> 40300 typ host\r\n" +
		"a=candidate:2 1 udp 16777215 203.0.113.17 40001 typ relay raddr 0.0.0.0 rport 5\r\n" +
		"a=candidate:3 1 udp 2130706431 127.0.0.1 40301 typ host\r\n"
	got := CountCandidates(sdp)
	if got["host"] != 2 || got["relay"] != 1 || got["srflx"] != 0 {
		t.Fatalf("%v", got)
	}
}

func TestCloseIsNilSafeAndIdempotent(t *testing.T) {
	var nilPeer *Peer
	nilPeer.Close()
	cfg := Config{PortMin: 40390, PortMax: 40399, IncludeLoopback: true}
	api, _ := NewAPI(cfg)
	track, _ := NewVideoTrackRTP()
	done := 0
	p, _, err := NewPeer(api, cfg, track, Callbacks{OnDone: func(string) { done++ }})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	p.Close()
	if done != 1 {
		t.Fatalf("OnDone fired %d times", done)
	}
}

// Task 01a0dbd6: four peers on one NewMuxAPI hold ONE UDP port between
// them. Each connects and delivers data, and every selected pair's local
// port is PortMin; the mux listens on exactly one address.
func TestFourPeersShareOneMuxedPort(t *testing.T) {
	const port = 31370 // below 32768: outside Linux's ephemeral range
	cfg := Config{PortMin: port, PortMax: port + 9, AllowIPs: []net.IP{net.ParseIP("127.0.0.1")}, IncludeLoopback: true}
	api, mux, err := NewMuxAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	if addrs := MuxAddrs(mux); len(addrs) != 1 || addrs[0].String() != "127.0.0.1:31370" {
		t.Fatalf("mux listens on %v", addrs)
	}
	track, _ := NewVideoTrackRTP()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	vapi := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	type pair struct {
		host *Peer
		got  chan string
	}
	var peers []pair
	for i := 0; i < 4; i++ {
		got := make(chan string, 1)
		connected := make(chan struct{}, 1)
		host, offer, err := NewPeer(api, cfg, track, Callbacks{
			OnData:      func(label string, data []byte) { got <- string(data) },
			OnConnected: func() { connected <- struct{}{} },
		})
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		defer host.Close()
		viewer, err := vapi.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		defer viewer.Close()
		msg := fmt.Sprintf(`{"t":"probe","seq":%d}`, i+1)
		viewer.OnDataChannel(func(dc *webrtc.DataChannel) {
			if dc.Label() == LabelInputEvents {
				dc.OnOpen(func() { _ = dc.SendText(msg) })
			}
		})
		if err := viewer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
			t.Fatal(err)
		}
		ans, _ := viewer.CreateAnswer(nil)
		g := webrtc.GatheringCompletePromise(viewer)
		_ = viewer.SetLocalDescription(ans)
		<-g
		if err := host.SetAnswer(viewer.LocalDescription().SDP); err != nil {
			t.Fatal(err)
		}
		select {
		case <-connected:
		case <-time.After(10 * time.Second):
			t.Fatalf("peer %d never connected", i)
		}
		peers = append(peers, pair{host, got})
	}
	for i, p := range peers {
		select {
		case m := <-p.got:
			if m != fmt.Sprintf(`{"t":"probe","seq":%d}`, i+1) {
				t.Fatalf("peer %d got %q (crossed ufrags?)", i, m)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("peer %d: no data", i)
		}
		f := strings.Fields(p.host.SelectedPair())
		if len(f) < 2 || f[1] != "127.0.0.1:31370" {
			t.Fatalf("peer %d selected %q, want local 127.0.0.1:31370", i, p.host.SelectedPair())
		}
	}
}
