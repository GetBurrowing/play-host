// Package rtc is the play host's WebRTC side: one sendonly H.264 video track, one sendonly Opus audio track
// and two data channels, with the host creating the offer.
//
// The RTCP loop that turns PLI/FIR into OnPLI is ported from cloudplay
// (pkg/network/webrtc/webrtc.go:51-75, Apache-2.0, see NOTICE). The rest is
// written for the play host: fixed UDP port range so a single Windows firewall
// rule covers it, mDNS query mode (Chrome hides LAN host candidates behind
// .local names), and an explicit interface IP filter.
package rtc

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Config is the network shape of the host side.
type Config struct {
	PortMin, PortMax uint16   // inclusive UDP range (40300-40309 on host-a)
	AllowIPs         []net.IP // host candidate addresses; empty = all
	ICEServers       []webrtc.ICEServer
	IncludeLoopback  bool // tests only
	// LogWriter receives pion's ICE warnings (why a candidate type failed
	// to gather) and every scope's errors; nil keeps pion's default (errors
	// to stdout, which the scheduled task does not capture).
	LogWriter io.Writer
}

// Channel labels shared with the browser.
const (
	LabelInputState  = "input-state"  // ordered:false, maxRetransmits:0; idempotent pad snapshots
	LabelInputEvents = "input-events" // reliable, ordered; key edges and probes
)

// Callbacks are fixed at NewPeer, before any pion goroutine can call them.
type Callbacks struct {
	OnPLI  func()
	OnData func(label string, data []byte)
	// OnOpen fires once per data channel when it opens (task 01a0ff61), so
	// the host can log that a browser's channel is up before any message.
	OnOpen      func(label string)
	OnConnected func()
	// OnDone fires once: Failed, Closed, or Disconnected for longer than
	// DisconnectGrace (ICE can recover from a short Disconnected).
	OnDone func(reason string)
}

// DisconnectGrace is how long Disconnected may last before the peer is done.
var DisconnectGrace = 5 * time.Second

// GatherTimeout bounds ICE gathering for one offer. When it expires the
// offer still goes out if it already carries a host candidate: a LAN viewer
// needs nothing else, and a slow TURN allocation (the relay server is
// reached through the router's hairpin) must not fail the session. Only an
// offer with no host candidate at all is an error (stage 6: a re-offer that
// timed out here used to end a live session).
var GatherTimeout = 5 * time.Second

// Gather describes how one offer's ICE gathering went, for the host log.
type Gather struct {
	Took    time.Duration
	Partial bool           // GatherTimeout expired before gathering completed
	Counts  map[string]int // candidate type -> count in the offer SDP
}

func (g Gather) String() string {
	return fmt.Sprintf("gathered in %s partial=%v host=%d srflx=%d relay=%d",
		g.Took.Round(time.Millisecond), g.Partial, g.Counts["host"], g.Counts["srflx"], g.Counts["relay"])
}

// ErrNoHostCandidate is a gathering timeout with nothing a viewer could reach.
var ErrNoHostCandidate = errors.New("ice gathering timed out with no host candidate")

// CountCandidates counts `a=candidate` lines by type in an SDP.
func CountCandidates(sdp string) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "typ" {
				out[f[i+1]]++
				break
			}
		}
	}
	return out
}

// Peer is one viewer connection.
type Peer struct {
	pc    *webrtc.PeerConnection
	Track webrtc.TrackLocal
	cb    Callbacks

	mu        sync.Mutex
	closed    bool
	pli       int
	remoteSet bool
	queued    []webrtc.ICECandidateInit
	dcTimer   *time.Timer
}

// NewAPI builds a pion API from cfg. Every peer binds its own socket in the
// PortMin-PortMax range (the spike harness uses this).
func NewAPI(cfg Config) (*webrtc.API, error) {
	se, err := settingEngine(cfg)
	if err != nil {
		return nil, err
	}
	return newAPI(se)
}

// NewMuxAPI builds a pion API whose peers ALL share one UDP port, PortMin,
// through a single-port ICE mux (task 01a0dbd6): pion demultiplexes by ICE
// ufrag, so N viewers hold one socket per allowed interface address instead
// of one socket each. Without it every peer bound its own port in the
// 10-port range and 4 players + 4 spectators would exhaust it. The range
// stays set for any non-mux socket pion opens. (Relay candidates are
// allocated from an ephemeral local socket to the TURN server either way.)
//
// The returned closer releases the port; close it after every peer.
func NewMuxAPI(cfg Config) (*webrtc.API, io.Closer, error) {
	if cfg.PortMin == 0 {
		return nil, nil, errors.New("udp mux needs a port (udp_min)")
	}
	se, err := settingEngine(cfg)
	if err != nil {
		return nil, nil, err
	}
	opts := []ice.UDPMuxFromPortOption{ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4)}
	if f := ipFilter(cfg); f != nil {
		opts = append(opts, ice.UDPMuxFromPortWithIPFilter(f))
	}
	if cfg.IncludeLoopback {
		opts = append(opts, ice.UDPMuxFromPortWithLoopback())
	}
	if se.LoggerFactory != nil {
		opts = append(opts, ice.UDPMuxFromPortWithLogger(se.LoggerFactory.NewLogger("udpmux")))
	}
	mux, err := ice.NewMultiUDPMuxFromPort(int(cfg.PortMin), opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("udp mux on %d: %w", cfg.PortMin, err)
	}
	se.SetICEUDPMux(mux)
	api, err := newAPI(se)
	if err != nil {
		mux.Close()
		return nil, nil, err
	}
	return api, mux, nil
}

// MuxAddrs lists the addresses a NewMuxAPI closer is listening on.
func MuxAddrs(c io.Closer) []net.Addr {
	if m, ok := c.(*ice.MultiUDPMuxDefault); ok {
		return m.GetListenAddresses()
	}
	return nil
}

func ipFilter(cfg Config) func(net.IP) bool {
	if len(cfg.AllowIPs) == 0 {
		return nil
	}
	allow := append([]net.IP(nil), cfg.AllowIPs...)
	return func(ip net.IP) bool {
		for _, a := range allow {
			if a.Equal(ip) {
				return true
			}
		}
		return false
	}
}

func settingEngine(cfg Config) (webrtc.SettingEngine, error) {
	se := webrtc.SettingEngine{}
	if cfg.PortMin != 0 {
		if err := se.SetEphemeralUDPPortRange(cfg.PortMin, cfg.PortMax); err != nil {
			return se, fmt.Errorf("udp port range: %w", err)
		}
	}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	se.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)
	if cfg.LogWriter != nil {
		se.LoggerFactory = &logging.DefaultLoggerFactory{
			Writer:          cfg.LogWriter,
			DefaultLogLevel: logging.LogLevelError,
			ScopeLevels:     map[string]logging.LogLevel{"ice": logging.LogLevelWarn},
		}
	}
	if f := ipFilter(cfg); f != nil {
		se.SetIPFilter(f)
	}
	return se, nil
}

func newAPI(se webrtc.SettingEngine) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(m)), nil
}

// NewVideoTrackRTP is a track fed with already-packetized RTP (ffmpeg -f rtp).
func NewVideoTrackRTP() (*webrtc.TrackLocalStaticRTP, error) {
	return webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "video", "play")
}

// NewAudioTrackRTP is a send-only Opus track fed with ffmpeg RTP packets.
func NewAudioTrackRTP() (*webrtc.TrackLocalStaticRTP, error) {
	return webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}, "audio", "play")
}

// NewVideoTrackSample is a track fed with Annex-B access units.
func NewVideoTrackSample() (*webrtc.TrackLocalStaticSample, error) {
	return webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "play")
}

// NewPeer creates the connection, adds track and data channels, and returns
// the (non-trickle) offer once ICE gathering finishes.
func NewPeer(api *webrtc.API, cfg Config, track webrtc.TrackLocal, cb Callbacks) (*Peer, string, error) {
	p, sdp, _, err := NewPeerGather(api, cfg, track, cb)
	return p, sdp, err
}

// NewPeerGather is the one-track compatibility entry point.
func NewPeerGather(api *webrtc.API, cfg Config, track webrtc.TrackLocal, cb Callbacks) (*Peer, string, Gather, error) {
	return NewPeerGatherTracks(api, cfg, []webrtc.TrackLocal{track}, cb)
}

// NewPeerGatherTracks adds every track as a send-only transceiver. The play
// host supplies its shared video and audio tracks here, producing both m-lines
// in one offer with one stream id so browsers can synchronize them.
func NewPeerGatherTracks(api *webrtc.API, cfg Config, tracks []webrtc.TrackLocal, cb Callbacks) (*Peer, string, Gather, error) {
	var g Gather
	t0 := time.Now()
	if len(tracks) == 0 {
		return nil, "", g, errors.New("peer needs at least one media track")
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: cfg.ICEServers})
	if err != nil {
		return nil, "", g, err
	}
	p := &Peer{pc: pc, Track: tracks[0], cb: cb}
	for _, track := range tracks {
		tr, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			pc.Close()
			return nil, "", g, err
		}
		go p.readRTCP(tr.Sender())
	}

	f, zero := false, uint16(0)
	for _, dc := range []struct {
		label string
		init  *webrtc.DataChannelInit
	}{
		{LabelInputState, &webrtc.DataChannelInit{Ordered: &f, MaxRetransmits: &zero}},
		{LabelInputEvents, nil},
	} {
		ch, err := pc.CreateDataChannel(dc.label, dc.init)
		if err != nil {
			pc.Close()
			return nil, "", g, err
		}
		label := dc.label
		ch.OnOpen(func() {
			if p.cb.OnOpen != nil {
				p.cb.OnOpen(label)
			}
		})
		ch.OnMessage(func(m webrtc.DataChannelMessage) {
			if p.cb.OnData != nil {
				p.cb.OnData(label, m.Data)
			}
		})
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			p.mu.Lock()
			if p.dcTimer != nil {
				p.dcTimer.Stop()
				p.dcTimer = nil
			}
			p.mu.Unlock()
			if p.cb.OnConnected != nil {
				p.cb.OnConnected()
			}
		case webrtc.PeerConnectionStateDisconnected:
			p.mu.Lock()
			if p.dcTimer == nil && !p.closed {
				p.dcTimer = time.AfterFunc(DisconnectGrace, func() { p.finish("disconnected") })
			}
			p.mu.Unlock()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			p.finish(s.String())
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, "", g, err
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, "", g, err
	}
	select {
	case <-done:
	case <-time.After(GatherTimeout):
		g.Partial = true
	}
	sdp := pc.LocalDescription().SDP
	g.Took = time.Since(t0)
	g.Counts = CountCandidates(sdp)
	if g.Partial && g.Counts["host"] == 0 {
		p.closeQuiet()
		return nil, "", g, ErrNoHostCandidate
	}
	return p, sdp, g, nil
}

// readRTCP is ported from cloudplay webrtc.go:51-75.
func (p *Peer) readRTCP(s *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		n, _, err := s.Read(buf)
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				p.mu.Lock()
				p.pli++
				p.mu.Unlock()
				if p.cb.OnPLI != nil {
					p.cb.OnPLI()
				}
			}
		}
	}
}

// PLICount is how many PLI/FIR the viewer sent.
func (p *Peer) PLICount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.pli }

// SetAnswer applies the browser's answer, then any candidates that arrived
// before it.
func (p *Peer) SetAnswer(sdp string) error {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		return err
	}
	p.mu.Lock()
	p.remoteSet = true
	q := p.queued
	p.queued = nil
	p.mu.Unlock()
	for _, c := range q {
		if err := p.pc.AddICECandidate(c); err != nil {
			return err
		}
	}
	return nil
}

// AddICECandidate adds a trickled viewer candidate, queueing it until the
// answer is set.
func (p *Peer) AddICECandidate(c webrtc.ICECandidateInit) error {
	p.mu.Lock()
	if !p.remoteSet {
		p.queued = append(p.queued, c)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.pc.AddICECandidate(c)
}

// SelectedPair describes the nominated candidate pair ("host", "srflx", "relay").
func (p *Peer) SelectedPair() string {
	sctp := p.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil || sctp.Transport().ICETransport() == nil {
		return ""
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return ""
	}
	return fmt.Sprintf("%s %s:%d <-> %s %s:%d", pair.Local.Typ, pair.Local.Address, pair.Local.Port, pair.Remote.Typ, pair.Remote.Address, pair.Remote.Port)
}

// State is the peer connection state.
func (p *Peer) State() string { return p.pc.ConnectionState().String() }

// Close ends the connection and waits for pion to release its sockets (and
// its ufrag's slot in a shared mux). Safe on a nil Peer and safe to call
// twice.
func (p *Peer) Close() {
	if p == nil {
		return
	}
	p.finish("closed by host")
	if p.pc != nil {
		_ = p.pc.Close()
	}
}

// closeQuiet closes a peer that never reached its caller: no OnDone.
func (p *Peer) closeQuiet() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_ = p.pc.Close()
}

func (p *Peer) finish(reason string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.dcTimer != nil {
		p.dcTimer.Stop()
		p.dcTimer = nil
	}
	p.mu.Unlock()
	if p.cb.OnDone != nil {
		p.cb.OnDone(reason)
	}
}
