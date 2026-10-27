// Package host is the play host's session engine: it answers core's
// play_session_* frames (internal/protocol) by building the requested commit
// (internal/build), launching Godot in a Job Object (internal/launch),
// streaming it (internal/media, internal/rtc) and feeding the viewer's input
// to the burrow_play addon (internal/input, internal/link).
//
// One session at a time: a start while one is running answers `failed` with
// detail "host busy" for the NEW session id and leaves the running one alone.
// A session with no input for idle_timeout_s ends with reason "idle".
//
// N peers, one encode (task 01a0dbd6). A session holds `peers`, one per
// WebRTC connection, keyed by the core-minted peer_id (internal/protocol,
// "Widening"). ffmpeg/NVENC encodes video once and libopus encodes Master-bus PCM once;
// every peer binds both shared tracks, so VRAM is flat and only upstream grows
// with viewers. The encoder and the addon's export run while at least one
// peer is connected and stop at zero, so a joiner or a leaver never restarts
// the stream for the others. All peers share one UDP port (rtc.NewMuxAPI).
// Each peer's input is bound to the slots core's play_slots names for it
// (input.Binding); a peer with none (a spectator) has every message dropped
// and counted, because input never passes through core and this is the only
// place it can be stopped.
//
// An old core sends no play_peer_open and no play_slots. Then the host keeps
// v1 exactly: one implicit peer (peer_id ""), src 0 is slot 0, and a peer
// that ends is re-offered with a fresh `connecting` status. The first
// play_peer_open or play_slots switches the session to multi-peer for good
// (and closes the implicit peer if it was already offered).
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Startup-Suite/play-host/internal/build"
	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/engine"
	"github.com/Startup-Suite/play-host/internal/inject"
	"github.com/Startup-Suite/play-host/internal/input"
	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/link"
	"github.com/Startup-Suite/play-host/internal/media"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/rtc"
	"github.com/pion/webrtc/v4"
)

// newPeerGather is rtc.NewPeerGatherTracks; tests swap it to make offers fail.
var newPeerGather = rtc.NewPeerGatherTracks

// Config is everything the engine needs about the machine.
type Config struct {
	Godot           string
	RenderingDriver string
	FFmpeg          string
	Encoder         media.EncoderKind // concrete host default; auto is resolved in cmd/play-host
	EncoderFallback bool              // libx264 only after chosen encoder init fails
	Build           build.Config
	LogsDir         string
	HostIP          string
	UDPMin          uint16
	UDPMax          uint16
	GodotPort       int
	RTPPort         int
	AudioRTPPort    int
	ImportTimeout   time.Duration
	LinkTimeout     time.Duration
	ProgressEvery   time.Duration // building/progress status cadence (5 s)
	IdleCheck       time.Duration // how often the idle timer is evaluated (1 s)
	SkipImport      bool          // tests
	Loopback        bool          // tests: offer 127.0.0.1 candidates
	// OfferAttempts bounds how many times one offer is tried before the
	// session fails (0 = 3); OfferBackoff is the wait before the first retry,
	// doubled each time (0 = 500 ms). Stage 6: one failed re-offer after a
	// viewer change used to end a live session.
	OfferAttempts int
	OfferBackoff  time.Duration
	// PionLog receives pion's ICE warnings and errors (play-host.log on host-a).
	PionLog io.Writer
	// ResumeGrace is how long a running session survives a dropped suite
	// socket (0 = DefaultResumeGrace, negative = end at once, the pre-stage-6
	// behaviour). The viewers' media never passes through core, so they keep
	// watching while the socket reconnects; see protocol "Resume".
	ResumeGrace time.Duration
	// LogTouch logs one line per touch line (st/sd) sent to the addon,
	// releases included (task 01a0fe45 stage 5). DEV RIGS ONLY: a held
	// drag is up to 60 lines/s per finger. Config key "log_touch".
	LogTouch bool
	// CaptureBackend (ddagrab | wgc; "" = ddagrab) and WindowTimeout (0 = 120 s)
	// apply to sessions whose checkout is not a Godot project. Config keys
	// "capture_backend" and "window_timeout_s".
	CaptureBackend capture.Backend
	WindowTimeout  time.Duration
	// FrameStallTimeout is how long a capture session may go without a
	// frame before it fails (0 = 10 s). Config key "frame_stall_timeout_s".
	FrameStallTimeout time.Duration
	// KeepAwake, if set, holds the display awake while a capture session runs.
	KeepAwake capture.KeepAwake
}

// DefaultResumeGrace bounds a suite reconnect that keeps the session. Core's
// own grace (config :platform, :game_stream_host_grace_ms) is shorter, so a
// host whose core already failed the session is not kept waiting past it for
// nothing more than a few seconds.
const DefaultResumeGrace = 30 * time.Second

// Sender pushes host -> core frames.
type Sender interface {
	Push(event string, payload any) error
}

// Stages are swappable so the engine is testable without git or Godot.
type Stages interface {
	// Prepare returns a project dir whose HEAD is exactly start.SHA.
	Prepare(ctx context.Context, s protocol.SessionStart, progress func(detail string)) (string, error)
	// AssertHead re-checks HEAD == sha immediately before launch.
	AssertHead(ctx context.Context, dir, sha string) error
	// Launch starts Godot for dir.
	Launch(dir string, s protocol.SessionStart, logPath string) (launch.Proc, error)
	// Remove deletes the session's checkout once Godot is gone (the bare
	// mirror stays, so the next build of the same repo is still a local
	// worktree add). Stage 6: the review clause is that an ended session's
	// checkout is cleaned up.
	Remove(ctx context.Context, s protocol.SessionStart, dir string) error
}

// EngineStages is optionally implemented by Stages. A Stages without it is
// all-Godot (the addon path), which is what every pre-existing test uses.
type EngineStages interface {
	// Engine names the engine for a prepared checkout.
	Engine(dir string) (engine.Kind, error)
	// Capture returns the video source for a launched non-Godot game. It runs
	// the session-0 guard and waits for the game window.
	Capture(ctx context.Context, p launch.Proc, s protocol.SessionStart) (capture.Source, error)
}

// InputStages is optionally implemented alongside EngineStages: it builds the
// injector that delivers browser input to a captured (addon-less) game.
type InputStages interface {
	Input(p launch.Proc, src capture.Source) (*inject.Injector, error)
}

// Host is the engine. It implements suite.Handler.
type Host struct {
	cfg    Config
	send   Sender
	stages Stages
	logf   func(string, ...any)

	mu  sync.Mutex
	cur *Session
	// away is the session kept through a dropped suite socket, and awayGen
	// names the grace timer that may still end it (a newer drop or a rejoin
	// bumps it, so a stale timer does nothing).
	away    *Session
	awayGen int
}

// New builds an engine. stages nil means the real git + Godot stages.
func New(cfg Config, send Sender, stages Stages, logf func(string, ...any)) *Host {
	if logf == nil {
		logf = log.Printf
	}
	if cfg.ProgressEvery == 0 {
		cfg.ProgressEvery = 5 * time.Second
	}
	if cfg.IdleCheck == 0 {
		cfg.IdleCheck = time.Second
	}
	if cfg.AudioRTPPort == 0 {
		cfg.AudioRTPPort = cfg.RTPPort + 1
	}
	if cfg.LinkTimeout == 0 {
		cfg.LinkTimeout = 90 * time.Second
	}
	if cfg.ImportTimeout == 0 {
		cfg.ImportTimeout = 20 * time.Minute
	}
	if cfg.OfferAttempts <= 0 {
		cfg.OfferAttempts = 3
	}
	if cfg.OfferBackoff <= 0 {
		cfg.OfferBackoff = 500 * time.Millisecond
	}
	if cfg.Encoder == "" || cfg.Encoder == media.EncoderAuto {
		// Direct library callers predate the machine setting; keep their
		// established Windows/NVENC behavior. serve resolves auto per GOOS.
		cfg.Encoder = media.EncoderNVENC
	}
	if cfg.ResumeGrace == 0 {
		cfg.ResumeGrace = DefaultResumeGrace
	}
	h := &Host{cfg: cfg, send: send, logf: logf}
	if stages == nil {
		stages = &realStages{h: h}
	}
	h.stages = stages
	return h
}

// SetSender wires the sender after construction (the suite client needs the
// host as its handler, and the host needs the client to push).
func (h *Host) SetSender(s Sender) { h.send = s }

// Current is the running session, or nil.
func (h *Host) Current() *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur
}

// SweepCheckouts removes checkouts left by a session that never ended
// cleanly (the host was killed or crashed). Call it once at start, before
// any session can run.
func (h *Host) SweepCheckouts(ctx context.Context) {
	removed, err := (&build.Builder{Cfg: h.cfg.Build, Logf: h.logf}).SweepCheckouts(ctx)
	if len(removed) > 0 || err != nil {
		h.logf("host: removed %d checkout(s) left by an earlier run: %v err=%v", len(removed), removed, err)
	}
}

// OnConnected implements suite.Handler. A session kept through a dropped
// socket (OnDisconnected) is resumed: it tells core it is still running and
// which peers it holds, and re-offers every peer that is not connected (its
// offer or answer may have been lost with the socket).
func (h *Host) OnConnected() {
	h.logf("host: joined; ready for play_session_start")
	h.mu.Lock()
	s := h.away
	h.away = nil
	h.awayGen++
	h.mu.Unlock()
	if s != nil && s == h.Current() {
		s.resume()
	}
}

// Shutdown ends the running session at once (the process is stopping). No
// status is sent: the socket is going away with the process.
func (h *Host) Shutdown(reason string) {
	h.mu.Lock()
	h.away = nil
	h.awayGen++
	h.mu.Unlock()
	if s := h.Current(); s != nil {
		s.stop(reason, false)
	}
}

// OnDisconnected implements suite.Handler. Stage 6 (01a0dbd6): the session
// is NOT ended at once. It is kept for ResumeGrace while the client
// reconnects (it does so by itself, with backoff); only if the socket is not
// back by then does the host tear its side down. Before this, one link blip
// ("websocket: close 1002", or a missed heartbeat) ended every viewer's
// session.
func (h *Host) OnDisconnected(err error) {
	s := h.Current()
	if s == nil {
		return
	}
	grace := h.cfg.ResumeGrace
	if grace < 0 {
		s.stop(fmt.Sprintf("suite connection lost: %v", err), false)
		return
	}
	h.mu.Lock()
	h.away = s
	h.awayGen++
	gen := h.awayGen
	h.mu.Unlock()
	s.logf("suite connection lost (%v): keeping the session %s for the reconnect", err, grace)
	time.AfterFunc(grace, func() {
		h.mu.Lock()
		expired := h.awayGen == gen && h.away == s
		if expired {
			h.away = nil
		}
		h.mu.Unlock()
		if expired {
			s.stop(fmt.Sprintf("suite connection lost: %v (not back within %s)", err, grace), false)
		}
	})
}

// OnEvent implements suite.Handler.
func (h *Host) OnEvent(event string, payload json.RawMessage) {
	switch event {
	case protocol.EventSessionStart:
		h.start(payload)
	case protocol.EventSessionStop:
		var st protocol.SessionStop
		if json.Unmarshal(payload, &st) != nil {
			return
		}
		if s := h.Current(); s != nil && s.ID() == st.SessionID {
			reason := st.Reason
			if reason == "" {
				reason = "Stopped"
			}
			s.stop(reason, true)
		}
	case protocol.EventSignal:
		var sig protocol.Signal
		if json.Unmarshal(payload, &sig) != nil {
			return
		}
		if s := h.Current(); s != nil && s.ID() == sig.SessionID {
			s.signal(sig)
		}
	case protocol.EventPeerOpen, protocol.EventPeerClose:
		var r protocol.PeerRef
		if json.Unmarshal(payload, &r) != nil || r.PeerID == "" {
			return
		}
		if s := h.Current(); s != nil && s.ID() == r.SessionID {
			if event == protocol.EventPeerOpen {
				s.peerOpen(r.PeerID)
			} else {
				s.peerClose(r.PeerID)
			}
		}
	case protocol.EventSlots:
		var sl protocol.Slots
		if json.Unmarshal(payload, &sl) != nil {
			return
		}
		if s := h.Current(); s != nil && s.ID() == sl.SessionID {
			s.setSlots(sl)
		}
	default:
		// capabilities, spaces manifest, etc.: not for the play host.
	}
}

func (h *Host) push(event string, payload any) {
	if h.send == nil {
		return
	}
	if err := h.send.Push(event, payload); err != nil {
		h.logf("host: push %s: %v", event, err)
	}
}

func (h *Host) start(payload json.RawMessage) {
	st, err := protocol.DecodeSessionStart(payload)
	if err != nil {
		h.logf("host: %v", err)
		var id struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(payload, &id) == nil && id.SessionID != "" {
			h.push(protocol.EventSessionStatus, protocol.NewStatus(id.SessionID, protocol.StateFailed, err.Error(), 0))
		}
		return
	}
	h.mu.Lock()
	if h.cur != nil {
		curID := h.cur.ID()
		h.mu.Unlock()
		if curID != st.SessionID {
			h.logf("host: refusing session %s: host busy with %s", st.SessionID, curID)
			h.push(protocol.EventSessionStatus, protocol.NewStatus(st.SessionID, protocol.StateFailed, "host busy", 0))
		}
		return
	}
	s := newSession(h, st)
	h.cur = s
	h.mu.Unlock()
	go func() {
		defer func() {
			// The session goroutine already fails itself on a panic (run's
			// recoverPanic); this is for a panic in its cleanup. Either way
			// the host stays up and free for the next session.
			if r := recover(); r != nil {
				h.logf("host: PANIC in session %s cleanup: %v\n%s", st.SessionID, r, debug.Stack())
			}
			h.mu.Lock()
			if h.cur == s {
				h.cur = nil
			}
			h.mu.Unlock()
		}()
		s.run()
	}()
}

// guard runs f, turning a panic into a logged session failure instead of a
// process exit. Every goroutine the session starts, and every callback pion
// runs on its own goroutines, goes through it (stage 6: an unrecovered
// panic in a pion callback killed play-host.exe with exit code 2 and
// nothing in the log).
func (s *Session) guard(where string, f func()) {
	defer s.recoverPanic(where)
	f()
}

func (s *Session) recoverPanic(where string) {
	if r := recover(); r != nil {
		s.logf("PANIC in %s: %v\n%s", where, r, debug.Stack())
		s.fail(fmt.Sprintf("The play host hit an internal error (%s); see play-host.log", where))
	}
}

// Session is one play session.
type Session struct {
	h     *Host
	start protocol.SessionStart
	t0    time.Time
	ctx   context.Context
	stopF context.CancelFunc

	mu       sync.Mutex
	state    string
	detail   string // the last non-terminal status detail (resume re-sends it)
	endState string
	endWhy   string
	report   bool // send the terminal status (false when the socket is gone)

	lastInput  atomic.Int64 // unix nanos
	dir        string       // the checkout, removed at the end
	godot      launch.Proc
	link       *link.Link
	api        *webrtc.API
	mux        io.Closer // the one shared UDP port
	rtcCfg     rtc.Config
	track      *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP
	enc        *media.Pipeline
	audio      *media.AudioPipeline
	events     chan func()
	liveSent   atomic.Bool
	mediaWG    sync.WaitGroup   // the goroutine that feeds the encoder
	inj        *inject.Injector // non-Godot engines: input goes here, not over a link
	captured   atomic.Int64     // frames seen from a capture Source
	exporting  atomic.Bool

	// peers is touched ONLY on the session loop.
	peers map[string]*peerState

	// pmu guards what core tells the host about peers. It is written from
	// the socket's goroutine (OnEvent) at any time, including while the
	// session is still building, and read by the loop in reconcile.
	pmu        sync.Mutex
	multi      bool                   // core has sent a multi-peer frame
	mediaReady bool                   // setupMedia done; reconcile may create peers
	openSeq    int                    // play_peer_open counter
	want       map[string]int         // peer_id -> the play_peer_open seq it must answer
	table      map[string]map[int]int // play_slots: peer_id -> src -> slot
	maxPlayers int

	actMu    sync.Mutex
	activity map[int]bool // slots with input since the last play_slot_activity
}

// peerState is one WebRTC connection and its input binding.
type peerState struct {
	id        string // "" is the v1 implicit peer
	seq       int    // the play_peer_open this connection answers
	peer      *rtc.Peer
	in        *input.Binding
	connected bool // loop only
	closed    atomic.Bool

	drops      atomic.Int64 // since the last drop log line
	dropsTotal atomic.Int64
	lastDrop   atomic.Int64 // unix nanos of the last drop log line
	dropWhy    atomic.Value // string

	// Input visibility (task 01a0ff61). All of it is fixed-size and atomic:
	// pion delivers the two data channels on their own goroutines, and
	// nothing a browser sends can grow it.
	msgs      [len(input.MsgTypes)]atomic.Int64 // decoded messages, by input.MsgTypes
	firstSeen [len(input.MsgTypes)]atomic.Bool  // the "first <type> message" line was logged
	dropBy    [len(dropReasons)]atomic.Int64    // drops, by dropReasons
	badParse  atomic.Int64                      // messages input.Decode refused
	// firstBad is the first unparseable message's shape, stored once by the
	// goroutine that took badParse from 0 to 1.
	firstBad   atomic.Pointer[badShape]
	summarized atomic.Bool // the close summary was logged
}

// badShape is an unparseable message described without its values.
type badShape struct {
	size  int
	keys  string
	class string
}

// dropReasons are the input drop reasons, in summary order, with the name
// each has in the summary line.
var dropReasons = [...]struct{ why, name string }{
	{input.DropNoSlot, "no_slot"},
	{input.DropProbeNoSpace, "probe_no_space"},
	{input.DropBadTouch, "bad_touch"},
	{input.DropTouchRate, "touch_rate"},
}

func (p *peerState) name() string {
	if p.id == "" {
		return "v1"
	}
	return p.id
}

// DropLogEvery bounds the per-peer "dropped input" log line.
var DropLogEvery = 10 * time.Second

// ActivityEvery is the play_slot_activity throttle (at most 4 per second).
var ActivityEvery = 250 * time.Millisecond

func newSession(h *Host, st protocol.SessionStart) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{h: h, start: st, t0: time.Now(), ctx: ctx, stopF: cancel, report: true, events: make(chan func(), 64),
		peers: map[string]*peerState{}, want: map[string]int{}, table: map[string]map[int]int{}, activity: map[int]bool{}}
	s.lastInput.Store(time.Now().UnixNano())
	return s
}

// ID is the session id.
func (s *Session) ID() string { return s.start.SessionID }

// State is the last state reported.
func (s *Session) State() string { s.mu.Lock(); defer s.mu.Unlock(); return s.state }

func (s *Session) logf(f string, a ...any) { s.h.logf("[%s] "+f, append([]any{s.ID()}, a...)...) }

func (s *Session) elapsed() int64 { return time.Since(s.t0).Milliseconds() }

// status reports a non-terminal state (terminal states go through finish).
func (s *Session) status(state, detail string) {
	s.mu.Lock()
	if protocol.Terminal(s.state) || s.endState != "" {
		s.mu.Unlock()
		return
	}
	s.state, s.detail = state, detail
	s.mu.Unlock()
	s.logf("status %s: %s", state, detail)
	s.h.push(protocol.EventSessionStatus, protocol.NewStatus(s.ID(), state, detail, s.elapsed()))
}

// stop ends the session from outside (core's stop, a disconnect).
func (s *Session) stop(reason string, report bool) {
	s.mu.Lock()
	if s.endState == "" {
		s.endState, s.endWhy, s.report = protocol.StateEnded, reason, report
	}
	s.mu.Unlock()
	s.stopF()
}

// resume runs after the suite socket rejoined while this session was kept
// (Host.OnConnected). On the loop: one resume status with the current state
// and the peers held, then a fresh offer for every peer that is not
// connected. Connected peers are left alone, so their video never stopped.
func (s *Session) resume() {
	s.post(func() {
		s.mu.Lock()
		state, detail, over := s.state, s.detail, s.endState != "" || protocol.Terminal(s.state)
		s.mu.Unlock()
		if over || state == "" {
			return
		}
		ids := make([]string, 0, len(s.peers))
		var reoffer []*peerState
		for id, ps := range s.peers {
			if id != "" {
				ids = append(ids, id)
			}
			if !ps.connected {
				reoffer = append(reoffer, ps)
			}
		}
		sort.Strings(ids)
		s.logf("suite reconnected: resuming %s with %d peer(s), re-offering %d", state, len(s.peers), len(reoffer))
		s.h.push(protocol.EventSessionStatus, protocol.NewResumeStatus(protocol.NewStatus(s.ID(), state, detail, s.elapsed()), ids))
		for _, ps := range reoffer {
			if s.peers[ps.id] == ps {
				s.openPeer(ps.id, ps.seq)
			}
		}
	})
}

func (s *Session) fail(detail string) {
	s.mu.Lock()
	if s.endState == "" {
		s.endState, s.endWhy = protocol.StateFailed, detail
	}
	s.mu.Unlock()
	s.stopF()
}

func (s *Session) end(detail string) {
	s.mu.Lock()
	if s.endState == "" {
		s.endState, s.endWhy = protocol.StateEnded, detail
	}
	s.mu.Unlock()
	s.stopF()
}

func (s *Session) signal(sig protocol.Signal) {
	select {
	case s.events <- func() { s.applySignal(sig) }:
	case <-s.ctx.Done():
	}
}

func (s *Session) applySignal(sig protocol.Signal) {
	ps := s.peers[sig.PeerID]
	if ps == nil || ps.peer == nil {
		s.logf("signal %s for peer %q before the peer exists; dropped", sig.Kind, sig.PeerID)
		return
	}
	switch sig.Kind {
	case protocol.KindAnswer:
		d, err := sig.Description()
		if err != nil {
			s.logf("answer: %v", err)
			return
		}
		if err := ps.peer.SetAnswer(d.SDP); err != nil {
			s.logf("answer (peer %s): %v", ps.name(), err)
		}
	case protocol.KindICE:
		c, ok, err := sig.Candidate()
		if err != nil || !ok {
			return
		}
		init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex, UsernameFragment: c.UsernameFragment}
		if err := ps.peer.AddICECandidate(init); err != nil {
			s.logf("ice (peer %s): %v", ps.name(), err)
		}
	}
}

// peerOpen records core's play_peer_open: create, or REPLACE, that peer.
// Recorded at once (the session may still be building); the loop acts on it.
func (s *Session) peerOpen(id string) {
	s.pmu.Lock()
	s.multi = true
	s.openSeq++
	s.want[id] = s.openSeq
	ready := s.mediaReady
	s.pmu.Unlock()
	s.logf("play_peer_open %s", id)
	if ready {
		s.post(s.reconcile)
	}
}

// peerClose records core's play_peer_close.
func (s *Session) peerClose(id string) {
	s.pmu.Lock()
	s.multi = true
	delete(s.want, id)
	ready := s.mediaReady
	s.pmu.Unlock()
	s.logf("play_peer_close %s", id)
	if ready {
		s.post(s.reconcile)
	}
}

// setSlots records core's play_slots snapshot.
func (s *Session) setSlots(sl protocol.Slots) {
	s.pmu.Lock()
	s.multi = true
	s.table = sl.Table()
	s.maxPlayers = sl.MaxPlayers
	ready := s.mediaReady
	s.pmu.Unlock()
	if ready {
		s.post(s.reconcile)
	}
}

// reconcile makes the live peers match what core asked for. Loop only.
func (s *Session) reconcile() {
	if s.ctx.Err() != nil {
		return
	}
	s.pmu.Lock()
	multi := s.multi
	want := make(map[string]int, len(s.want))
	for id, seq := range s.want {
		want[id] = seq
	}
	table := s.table
	s.pmu.Unlock()
	if !multi {
		if s.peers[""] == nil {
			s.openPeer("", 0)
		}
		return
	}
	if ps := s.peers[""]; ps != nil {
		s.logf("core sent multi-peer frames: closing the v1 peer")
		s.closePeer(ps)
	}
	for id, ps := range s.peers {
		if _, ok := want[id]; !ok {
			s.closePeer(ps)
		}
	}
	for id, seq := range want {
		if ps := s.peers[id]; ps == nil || ps.seq < seq {
			s.openPeer(id, seq)
		}
	}
	for id, ps := range s.peers {
		lines, bound, unbound := ps.in.SetSlots(table[id])
		s.logSlotChanges(ps, bound, unbound)
		s.sendLines(lines)
	}
}

func (s *Session) run() {
	defer s.cleanup()
	defer s.recoverPanic("session")
	s.status(protocol.StateBuilding, fmt.Sprintf("Preparing %s at %s", s.start.Branch, s.start.SHA[:12]))

	// Progress reporter: building with elapsed_ms every ProgressEvery.
	var detail atomic.Value
	detail.Store("Preparing")
	progCtx, progStop := context.WithCancel(s.ctx)
	go s.guard("progress", func() {
		t := time.NewTicker(s.h.cfg.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-progCtx.Done():
				return
			case <-t.C:
				s.status(protocol.StateBuilding, detail.Load().(string))
			}
		}
	})
	dir, err := s.h.stages.Prepare(s.ctx, s.start, func(d string) { detail.Store(d) })
	if dir != "" {
		s.dir = dir
	}
	if err != nil {
		progStop()
		s.fail("build: " + err.Error())
		return
	}
	if err := s.h.stages.AssertHead(s.ctx, dir, s.start.SHA); err != nil {
		progStop()
		s.fail(err.Error())
		return
	}
	kind := engine.Godot
	es, hasEngines := s.h.stages.(EngineStages)
	if hasEngines {
		k, err := es.Engine(dir)
		if err != nil {
			progStop()
			s.fail("engine: " + err.Error())
			return
		}
		kind = k
	}
	detail.Store("Starting the game")
	godotLog := filepath.Join(s.h.cfg.LogsDir, "godot-"+s.ID()+".log")
	p, err := s.h.stages.Launch(dir, s.start, godotLog)
	if err != nil {
		progStop()
		s.fail("launch: " + err.Error())
		return
	}
	s.godot = p
	pids, _ := p.Pids()
	s.logf("godot pid %d job %v dir %s", p.Pid(), pids, dir)
	exited := make(chan int, 1)
	go s.guard("godot wait", func() {
		code, _ := p.Wait()
		exited <- code
	})
	var gone atomic.Bool
	go s.guard("godot watch", func() {
		select {
		case code := <-exited:
			gone.Store(true)
			s.fail(fmt.Sprintf("The game exited (code %d); see %s", code, filepath.Base(godotLog)))
		case <-s.ctx.Done():
		}
	})
	if kind == engine.Godot {
		l, err := link.Dial(s.h.cfg.GodotPort, s.h.cfg.LinkTimeout, func() bool { return !gone.Load() && s.ctx.Err() == nil })
		progStop()
		if err != nil {
			s.fail(err.Error())
			return
		}
		s.link = l
		if err := s.setupMedia(); err != nil {
			s.fail("webrtc: " + err.Error())
			return
		}
		s.goMedia("media", s.readMedia)
	} else {
		detail.Store("Waiting for the game window")
		src, err := es.Capture(s.ctx, p, s.start)
		progStop()
		if err != nil {
			if s.ctx.Err() == nil {
				s.fail(err.Error())
			}
			return
		}
		if err := s.setupMedia(); err != nil {
			s.fail("webrtc: " + err.Error())
			return
		}
		s.enc.Source = src
		if is, ok := s.h.stages.(InputStages); ok {
			inj, err := is.Input(p, src)
			if err != nil {
				s.fail(err.Error())
				return
			}
			s.inj = inj
		}
		s.logf("capture: %s (%s)", src.Name(), kind)
		s.goMedia("capture", func() { s.runCapture(src) })
	}

	s.pmu.Lock()
	s.mediaReady = true
	s.pmu.Unlock()
	s.reconcile()
	idle := time.Duration(s.start.IdleTimeoutS) * time.Second
	tick := time.NewTicker(s.h.cfg.IdleCheck)
	defer tick.Stop()
	act := time.NewTicker(ActivityEvery)
	defer act.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case f := <-s.events:
			f()
		case <-act.C:
			s.flushActivity()
		case <-tick.C:
			s.flushDrops(false)
			if time.Since(time.Unix(0, s.lastInput.Load())) >= idle {
				s.end("idle")
				return
			}
		}
	}
}

// setupMedia builds the pion API, shared video/audio tracks and both encoder
// pipelines; they outlive individual peers.
func (s *Session) setupMedia() error {
	cfg := rtc.Config{PortMin: s.h.cfg.UDPMin, PortMax: s.h.cfg.UDPMax, ICEServers: ICEServers(s.start.ICEServers), IncludeLoopback: s.h.cfg.Loopback, LogWriter: s.h.cfg.PionLog}
	if s.h.cfg.HostIP != "" {
		cfg.AllowIPs = []net.IP{net.ParseIP(s.h.cfg.HostIP)}
	}
	api, mux, err := rtc.NewMuxAPI(cfg)
	if err != nil {
		return err
	}
	s.mux = mux
	s.logf("ice udp mux listening on %v", rtc.MuxAddrs(mux))
	track, err := rtc.NewVideoTrackRTP()
	if err != nil {
		return err
	}
	audioTrack, err := rtc.NewAudioTrackRTP()
	if err != nil {
		return err
	}
	s.api, s.rtcCfg, s.track, s.audioTrack = api, cfg, track, audioTrack
	encoder := s.start.Encoder.EncoderKind()
	if encoder == media.EncoderAuto {
		encoder = s.h.cfg.Encoder
	}
	s.enc = &media.Pipeline{FFmpeg: s.h.cfg.FFmpeg, Preset: s.start.Encoder, Encoder: encoder, Fallback: s.h.cfg.EncoderFallback, RTPPort: s.h.cfg.RTPPort, Track: track,
		LogPath: filepath.Join(s.h.cfg.LogsDir, "ffmpeg-"+s.ID()+".log"), Counts: &media.Counters{}}
	s.audio = &media.AudioPipeline{FFmpeg: s.h.cfg.FFmpeg, RTPPort: s.h.cfg.AudioRTPPort, Track: audioTrack,
		LogPath: filepath.Join(s.h.cfg.LogsDir, "ffmpeg-audio-"+s.ID()+".log"), Counts: &media.Counters{}}
	return nil
}

// post runs f on the session loop, unless the session is over; it reports
// whether f was queued.
func (s *Session) post(f func()) bool {
	if s.ctx.Err() != nil {
		return false
	}
	select {
	case s.events <- f:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// openPeer (re)creates peer id and gathers its offer off the loop. Loop only.
func (s *Session) openPeer(id string, seq int) {
	if old := s.peers[id]; old != nil {
		s.closePeer(old)
	}
	ps := &peerState{id: id, seq: seq}
	if id == "" {
		ps.in = input.NewV1Binding()
	} else {
		ps.in = input.NewBinding()
		s.pmu.Lock()
		// A fresh binding holds nothing, so there are no release lines.
		_, bound, unbound := ps.in.SetSlots(s.table[id])
		s.pmu.Unlock()
		s.logSlotChanges(ps, bound, unbound)
	}
	s.peers[id] = ps
	go s.guard("offer", func() { s.gatherOffer(ps) })
}

// gatherOffer builds ps's connection, retrying a failed attempt (bounded,
// with backoff). Stage 6 (01a0db5f): a re-offer after a viewer change once
// timed out gathering and ended a live session; one failed attempt is now a
// log line. It runs off the loop so one peer's gathering (up to
// rtc.GatherTimeout over TURN) never stalls another peer's signals.
func (s *Session) gatherOffer(ps *peerState) {
	var err error
	wait := s.h.cfg.OfferBackoff
	for attempt := 1; attempt <= s.h.cfg.OfferAttempts; attempt++ {
		holder := new(*rtc.Peer)
		var p *rtc.Peer
		var offer string
		var g rtc.Gather
		p, offer, g, err = newPeerGather(s.api, s.rtcCfg, []webrtc.TrackLocal{s.track, s.audioTrack}, s.callbacks(ps, holder))
		if err == nil {
			if !s.post(func() { s.installPeer(ps, holder, p, offer, g) }) {
				p.Close() // the session ended while it gathered
			}
			return
		}
		if s.ctx.Err() != nil || ps.closed.Load() {
			return
		}
		s.logf("peer %s: offer attempt %d/%d failed: %v", ps.name(), attempt, s.h.cfg.OfferAttempts, err)
		if attempt == s.h.cfg.OfferAttempts {
			break
		}
		select {
		case <-time.After(wait):
		case <-s.ctx.Done():
			return
		}
		wait *= 2
	}
	s.post(func() { s.offerFailed(ps, err) })
}

// callbacks are one connection attempt's pion callbacks. *holder is set on
// the loop when the attempt becomes ps.peer; a callback from an attempt that
// never did (its NewPeer failed and closed it), or from a connection since
// replaced or closed, does nothing. Stage 6 (01a0db5f): the nil == nil case
// of an older check ran viewerGone on a nil peer and panicked the process.
// Callbacks post from their own goroutine: pion may run them inside Close,
// which the loop itself calls.
func (s *Session) callbacks(ps *peerState, holder **rtc.Peer) rtc.Callbacks {
	mine := func() bool {
		return *holder != nil && ps.peer == *holder && !ps.closed.Load() && s.peers[ps.id] == ps
	}
	return rtc.Callbacks{
		OnPLI: func() {
			s.logf("PLI/FIR from peer %s: no forced IDR in the subprocess encoder; next IDR within GOP %d", ps.name(), s.start.Encoder.GOPFrames)
		},
		OnData: func(label string, data []byte) {
			s.guard("input", func() { s.onInput(ps, label, data) })
		},
		OnOpen: func(label string) {
			if ps.closed.Load() {
				return
			}
			s.logf("peer %s: data channel %s open, slots %s", ps.name(), label, slotTable(ps.in.Slots()))
		},
		OnConnected: func() {
			go s.post(func() {
				if !mine() {
					return
				}
				s.logf("viewer connected (peer %s): %s", ps.name(), ps.peer.SelectedPair())
				ps.connected = true
				s.mediaCheck()
			})
		},
		OnDone: func(reason string) {
			go s.post(func() {
				if !mine() {
					return
				}
				s.logf("viewer gone (peer %s): %s", ps.name(), reason)
				s.peerEnded(ps)
			})
		},
	}
}

// installPeer publishes a gathered offer. Loop only.
func (s *Session) installPeer(ps *peerState, holder **rtc.Peer, p *rtc.Peer, offer string, g rtc.Gather) {
	if s.ctx.Err() != nil || ps.closed.Load() || s.peers[ps.id] != ps {
		p.Close() // superseded while it gathered; its callbacks see a nil holder
		return
	}
	*holder = p
	ps.peer = p
	s.logf("offer (peer %s): %s", ps.name(), g)
	sig, err := protocol.NewOfferSignal(s.ID(), ps.id, offer)
	if err != nil {
		s.offerFailed(ps, err)
		return
	}
	// connecting goes out BEFORE the offer, so whoever reads the offer can
	// rely on the state (the stage-6 retry test read the state straight after
	// the offer and raced the old push-then-status order).
	if ps.id == "" {
		// v1: every re-offer reports connecting and a fresh live.
		s.liveSent.Store(false)
		s.status(protocol.StateConnecting, "Waiting for the viewer")
	} else if !s.liveSent.Load() {
		// Multi-peer: live is sent once per session, and a later joiner's
		// offer does not send the others' canvases back to connecting.
		s.status(protocol.StateConnecting, "Waiting for the viewer")
	}
	s.h.push(protocol.EventSignal, sig)
}

// offerFailed: every attempt failed. The v1 peer is the only viewer, so the
// session fails (as before). A multi-peer session keeps streaming to the
// others; that peer stays down until core opens it again.
func (s *Session) offerFailed(ps *peerState, err error) {
	if ps.closed.Load() || s.peers[ps.id] != ps {
		return
	}
	if ps.id == "" {
		s.fail("webrtc: " + err.Error())
		return
	}
	s.logf("peer %s: no offer after %d attempts (%v); dropped until core opens it again", ps.id, s.h.cfg.OfferAttempts, err)
	s.closePeer(ps)
}

// peerEnded runs on the loop when a connection ends on its own (failed,
// closed by the browser, disconnected past rtc.DisconnectGrace). It is
// closed, its slots' input released, and it is re-offered under the same
// peer_id while core still wants it (v1: always).
func (s *Session) peerEnded(ps *peerState) {
	s.closePeer(ps)
	if s.ctx.Err() != nil {
		return
	}
	s.pmu.Lock()
	multi := s.multi
	seq, wanted := s.want[ps.id]
	s.pmu.Unlock()
	switch {
	case ps.id == "" && !multi:
		s.openPeer("", 0)
	case ps.id != "" && wanted:
		s.openPeer(ps.id, seq)
	}
}

// closePeer closes one connection and releases ONLY its slots. The old
// connection is closed before the next one is created, so its TURN
// allocation is released first. Loop only.
func (s *Session) closePeer(ps *peerState) {
	ps.closed.Store(true) // before Close: no input from it after this
	if s.peers[ps.id] == ps {
		delete(s.peers, ps.id)
	}
	ps.connected = false
	ps.peer.Close() // nil-safe
	s.sendLines(ps.in.Release())
	s.flushDropsFor(ps, true)
	s.inputSummary(ps)
	s.mediaCheck()
}

// mediaCheck runs both encoders and the addon's export while at least one
// peer is connected, and tears all producer state down at zero. Loop only.
func (s *Session) mediaCheck() {
	n := 0
	for _, ps := range s.peers {
		if ps.connected {
			n++
		}
	}
	switch {
	case n > 0 && !s.exporting.Load():
		if err := s.audio.Start(); err != nil {
			s.fail("audio encoder: " + err.Error())
			return
		}
		s.setExport(true)
	case n == 0 && s.exporting.Load():
		s.setExport(false)
		s.enc.Stop()
		s.audio.Stop()
		s.logf("no connected peer: encoder and export stopped (audio stopped)")
	}
}
func (s *Session) setExport(on bool) {
	s.exporting.Store(on)
	if s.link != nil {
		if err := s.link.Send(map[string]any{"t": "export", "on": on}); err != nil {
			s.logf("export %v: %v", on, err)
		}
	}
}

func (s *Session) sendLines(lines []input.Out) {
	if s.inj != nil {
		s.inj.Apply(lines)
		return
	}
	if s.link == nil {
		return
	}
	for _, l := range lines {
		if err := s.link.Send(l); err != nil {
			s.logf("addon link send: %v", err)
			return
		}
		if s.h.cfg.LogTouch && (l["t"] == "st" || l["t"] == "sd") {
			b, _ := json.Marshal(l)
			s.logf("touch line %s", b)
		}
	}
}

// onInput runs on pion's goroutines. The slot comes from ps's binding.
func (s *Session) onInput(ps *peerState, label string, data []byte) {
	if ps.closed.Load() {
		return
	}
	r, err := ps.in.Handle(label, data)
	if err != nil {
		s.noteUnparseable(ps, label, data, err)
		return
	}
	s.noteType(ps, r, label)
	if r.Dropped != "" {
		s.noteDrop(ps, r.Dropped, label)
		return
	}
	if r.Activity {
		s.lastInput.Store(time.Now().UnixNano())
		if r.Slot >= 0 {
			s.actMu.Lock()
			s.activity[r.Slot] = true
			s.actMu.Unlock()
		}
	}
	s.sendLines(r.Lines)
}

// noteDrop counts a dropped message; the first one, and then at most one
// line per DropLogEvery, is logged against the peer.
func (s *Session) noteDrop(ps *peerState, why, label string) {
	for i := range dropReasons {
		if dropReasons[i].why == why {
			ps.dropBy[i].Add(1)
		}
	}
	ps.dropsTotal.Add(1)
	ps.drops.Add(1)
	ps.dropWhy.Store(why + " (" + label + ")")
	s.flushDropsFor(ps, false)
}

// noteType counts a decoded message by type and logs the first of each type
// (task 01a0ff61). It runs before the drop branch, so a spectator's first
// touch is logged too, with the reason it has no slot. At most
// len(input.MsgTypes) lines per peer.
func (s *Session) noteType(ps *peerState, r input.Result, label string) {
	i := input.MsgTypeIndex(r.Type)
	ps.msgs[i].Add(1)
	if !ps.firstSeen[i].CompareAndSwap(false, true) {
		return
	}
	var where string
	switch {
	case r.Dropped == input.DropNoSlot:
		where = "no slot (its src has no entry in the slot table)"
	case r.Slot < 0 && r.Dropped != "":
		where = "no slot (" + r.Dropped + ")"
	case r.Slot < 0: // release_all, an unknown type: not bound to one slot
		where = "no slot (not slot-bound)"
	case r.Dropped != "":
		where = fmt.Sprintf("slot %d, dropped (%s)", r.Slot, r.Dropped)
	default:
		where = fmt.Sprintf("slot %d", r.Slot)
	}
	s.logf("peer %s: first %s message on %s, %s", ps.name(), input.MsgTypes[i], label, where)
}

// noteUnparseable counts a message input.Decode refused. The first is
// logged at once, by size, key names and error class only (task 01a0ff61):
// never a value, and never the decoder's text, which quotes the input.
func (s *Session) noteUnparseable(ps *peerState, label string, data []byte, err error) {
	n := ps.badParse.Add(1)
	if n != 1 {
		return
	}
	size, keys := input.Shape(data)
	b := &badShape{size: size, keys: keys, class: input.ErrClass(err)}
	ps.firstBad.Store(b)
	s.logf("peer %s: unparseable input message on %s (%d bytes, keys %s, %s); later ones are counted", ps.name(), label, b.size, b.keys, b.class)
}

// inputSummary logs ps's input counts once, when it closes (task 01a0ff61).
func (s *Session) inputSummary(ps *peerState) {
	if !ps.summarized.CompareAndSwap(false, true) {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "peer %s: input summary", ps.name())
	for i, t := range input.MsgTypes {
		fmt.Fprintf(&b, " %s=%d", t, ps.msgs[i].Load())
	}
	bad := ps.badParse.Load()
	fmt.Fprintf(&b, " unparseable=%d drops", bad)
	for i := range dropReasons {
		fmt.Fprintf(&b, " %s=%d", dropReasons[i].name, ps.dropBy[i].Load())
	}
	if f := ps.firstBad.Load(); bad > 0 && f != nil {
		fmt.Fprintf(&b, " first_unparseable=%dB keys %s", f.size, f.keys)
	}
	s.logf("%s", b.String())
}

// logSlotChanges logs what a SetSlots actually changed for ps (task
// 01a0ff61). reconcile calls SetSlots for every peer on every play_slots,
// so an unchanged table logs nothing.
func (s *Session) logSlotChanges(ps *peerState, bound, unbound []input.SlotChange) {
	for _, c := range unbound {
		s.logf("peer %s: slot %d unbound (src %d)", ps.name(), c.Slot, c.Src)
	}
	for _, c := range bound {
		s.logf("peer %s: slot %d bound (src %d)", ps.name(), c.Slot, c.Src)
	}
}

// slotTable formats a src -> slot table as "[src0:slot ...]", sorted by
// src; "[]" is a spectator.
func slotTable(t map[int]int) string {
	srcs := make([]int, 0, len(t))
	for src := range t {
		srcs = append(srcs, src)
	}
	sort.Ints(srcs)
	parts := make([]string, len(srcs))
	for i, src := range srcs {
		parts[i] = fmt.Sprintf("src%d:%d", src, t[src])
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (s *Session) flushDrops(force bool) {
	for _, ps := range s.peers {
		s.flushDropsFor(ps, force)
	}
}

func (s *Session) flushDropsFor(ps *peerState, force bool) {
	if ps.drops.Load() == 0 {
		return
	}
	now := time.Now().UnixNano()
	last := ps.lastDrop.Load()
	if !force && now-last < int64(DropLogEvery) {
		return
	}
	if !ps.lastDrop.CompareAndSwap(last, now) {
		return
	}
	n := ps.drops.Swap(0)
	if n == 0 {
		return
	}
	why, _ := ps.dropWhy.Load().(string)
	s.logf("peer %s: dropped %d input message(s), %d in total: %s", ps.name(), n, ps.dropsTotal.Load(), why)
}

// flushActivity sends play_slot_activity for the slots that saw input since
// the last one (multi-peer only: an old core has no handler for it).
func (s *Session) flushActivity() {
	s.actMu.Lock()
	if len(s.activity) == 0 {
		s.actMu.Unlock()
		return
	}
	slots := make([]int, 0, len(s.activity))
	for slot := range s.activity {
		slots = append(slots, slot)
	}
	s.activity = map[int]bool{}
	s.actMu.Unlock()
	s.pmu.Lock()
	multi := s.multi
	s.pmu.Unlock()
	if !multi {
		return
	}
	sort.Ints(slots)
	s.h.push(protocol.EventSlotActivity, protocol.SlotActivity{SessionID: s.ID(), Slots: slots})
}

// readMedia demultiplexes addon video and game-process PCM. Audio is copied
// into a bounded non-blocking queue; video starts on its first frame.
func (s *Session) readMedia() {
	var buf []byte
	for {
		rec, b, err := s.link.ReadRecord(buf)
		buf = b
		if err != nil {
			if s.ctx.Err() == nil {
				s.fail("addon link: " + err.Error())
			}
			return
		}
		if !s.exporting.Load() {
			continue
		}
		if rec.PCM != nil {
			s.audio.Submit(rec.Data)
			continue
		}
		if rec.Frame == nil || rec.Frame.Kind != link.KindImageRGBA8 {
			continue
		}
		h := rec.Frame
		if !s.enc.Running() {
			if err := s.enc.Start(int(h.Width), int(h.Height)); err != nil {
				s.fail("encoder: " + err.Error())
				return
			}
			s.logf("encoder started video=%dx%d %s audio=opus/48000/2: %s", h.Width, h.Height, s.start.Encoder.Label(), strings.Join(s.enc.Args, " "))
		}
		if err := s.enc.WriteFrame(rec.Data); err != nil {
			s.logf("encoder write: %v", err)
			continue
		}
		if s.enc.Counts.Packets.Load() > 0 && !s.liveSent.Swap(true) {
			p := s.start.Encoder
			s.status(protocol.StateLive, fmt.Sprintf("Streaming %dx%d at %d fps (%s/%s %d kbps, GOP %d) with Opus game audio; asked for %dx%d", h.Width, h.Height, p.FPS, p.Preset, p.Tune, p.BitrateKbps, p.GOPFrames, p.Width, p.Height))
		}
	}
}

// runCapture feeds the encoder from a capture Source (non-Godot engines).
// Frames are dropped, and a native encoder is not started, until a viewer is
// connected, exactly like the addon path.
func (s *Session) runCapture(src capture.Source) {
	if ka := s.h.cfg.KeepAwake; ka != nil {
		if err := ka.Start(); err != nil {
			s.fail("capture: " + err.Error())
			return
		}
		defer ka.Stop()
	}
	go s.guard("capture watchdog", s.stallWatchdog)
	if src.Native() {
		w, h := src.Size()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			if !s.exporting.Load() {
				continue
			}
			if !s.enc.Running() {
				if err := s.enc.Start(w, h); err != nil {
					s.fail("encoder: " + err.Error())
					return
				}
				s.logf("encoder started video=%dx%d %s source=%s: %s", w, h, s.start.Encoder.Label(), src.Name(), strings.Join(s.enc.Args, " "))
			}
			s.captured.Store(s.enc.Counts.Packets.Load())
			s.noteLive(w, h)
		}
	}
	err := src.Frames(s.ctx, func(rgba []byte, w, h int) error {
		if !s.exporting.Load() {
			return nil
		}
		s.captured.Add(1)
		if !s.enc.Running() {
			if err := s.enc.Start(w, h); err != nil {
				return fmt.Errorf("encoder: %w", err)
			}
			s.logf("encoder started video=%dx%d %s source=%s: %s", w, h, s.start.Encoder.Label(), src.Name(), strings.Join(s.enc.Args, " "))
		}
		if err := s.enc.WriteFrame(rgba); err != nil {
			s.logf("encoder write: %v", err)
			return nil
		}
		s.noteLive(w, h)
		return nil
	})
	if err != nil && s.ctx.Err() == nil {
		s.fail("capture: " + err.Error())
	}
}

// stallWatchdog fails the session when a capture session that has a viewer
// delivers no frame within FrameStallTimeout of the viewer connecting (or of
// the last frame). It never lets a dead capture hang.
func (s *Session) stallWatchdog() {
	limit := s.h.cfg.FrameStallTimeout
	if limit <= 0 {
		limit = 10 * time.Second
	}
	poll := min(limit/10, 100*time.Millisecond)
	if poll <= 0 {
		poll = time.Millisecond
	}
	var since time.Time
	var lastPackets int64 = -1
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(poll):
		}
		if !s.exporting.Load() {
			since, lastPackets = time.Time{}, -1
			continue
		}
		n := s.captured.Load()
		if since.IsZero() || n != lastPackets {
			since, lastPackets = time.Now(), n
			continue
		}
		if time.Since(since) >= limit {
			s.fail("capture stalled: no frames (display asleep?)")
			return
		}
	}
}

// noteLive sends the live status once the encoder has produced packets.
func (s *Session) noteLive(w, h int) {
	if s.enc.Counts.Packets.Load() > 0 && !s.liveSent.Swap(true) {
		p := s.start.Encoder
		s.status(protocol.StateLive, fmt.Sprintf("Streaming %dx%d at %d fps (%s/%s %d kbps, GOP %d) with Opus game audio; asked for %dx%d", w, h, p.FPS, p.Preset, p.Tune, p.BitrateKbps, p.GOPFrames, p.Width, p.Height))
	}
}

// goMedia runs the goroutine that feeds the encoder, tracked so cleanup can
// wait for it before the final encoder stop: otherwise a frame in flight at
// the end restarts ffmpeg after cleanup stopped it, and the orphan keeps its
// log file open (seen as a TempDir cleanup failure on Windows).
func (s *Session) goMedia(where string, f func()) {
	s.mediaWG.Add(1)
	go func() {
		defer s.mediaWG.Done()
		s.guard(where, f)
	}()
}

func (s *Session) cleanup() {
	// Run what was queued before the end: an offer that finished gathering
	// is closed by installPeer (the context is done) instead of leaking.
	for drained := false; !drained; {
		select {
		case f := <-s.events:
			s.guard("drain", f)
		default:
			drained = true
		}
	}
	for _, ps := range s.peers {
		ps.closed.Store(true)
		ps.peer.Close() // nil-safe
		s.sendLines(ps.in.Release())
		s.flushDropsFor(ps, true)
		s.inputSummary(ps)
	}
	s.peers = map[string]*peerState{}
	if s.mux != nil {
		s.mux.Close()
	}
	if s.enc != nil {
		s.enc.Stop()
	}
	if s.audio != nil {
		s.audio.Stop()
	}
	if s.link != nil {
		s.sendLines([]input.Out{{"t": "release_all"}})
		s.link.Close()
	}
	if s.godot != nil {
		pids, _ := s.godot.Pids()
		err := s.godot.Kill()
		s.logf("killed godot job pids=%v err=%v", pids, err)
	}
	if s.inj != nil {
		s.inj.Close()
		s.logf("input injection: %s", s.inj.Counts())
	}
	s.exporting.Store(false)
	waited := make(chan struct{})
	go func() { s.mediaWG.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		s.logf("media goroutine still running at cleanup")
	}
	if s.enc != nil {
		s.enc.Stop()
	}
	if s.dir != "" {
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := s.h.stages.Remove(ctx, s.start, s.dir)
		cancel()
		s.logf("removed checkout %s in %s err=%v", s.dir, time.Since(t0).Round(time.Millisecond), err)
	}
	s.mu.Lock()
	state, why, report := s.endState, s.endWhy, s.report
	if state == "" {
		state, why = protocol.StateEnded, "ended"
	}
	s.state = state
	s.mu.Unlock()
	s.logf("session %s: %s", state, why)
	if report {
		s.h.push(protocol.EventSessionStatus, protocol.NewStatus(s.ID(), state, why, s.elapsed()))
	}
}

// ICEServers converts the start payload's servers for pion.
func ICEServers(in []protocol.ICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	return out
}

// realStages uses git and Godot.
type realStages struct{ h *Host }

func (r *realStages) Prepare(ctx context.Context, st protocol.SessionStart, progress func(string)) (string, error) {
	cfg := r.h.cfg
	b := &build.Builder{Cfg: cfg.Build, Logf: r.h.logf}
	repo, err := cfg.Build.FindRepo(st.RepoURL)
	if err != nil {
		return "", err
	}
	progress("Fetching " + repo.Match)
	mirror, err := b.EnsureCommit(ctx, repo, st.SHA)
	if err != nil {
		return "", err
	}
	dir, err := build.CheckoutDir(cfg.Build.CheckoutsDir, build.Task8(st.TaskID, st.Branch, st.SessionID), st.SHA)
	if err != nil {
		return "", err
	}
	progress("Checking out " + st.SHA[:12])
	if err := b.Checkout(ctx, repo, mirror, dir, st.SHA); err != nil {
		return "", err
	}
	addon, removed, err := b.InstallAddon(dir)
	if err != nil {
		return "", err
	}
	if addon == build.LegacySuitePlay {
		r.h.logf("build: addon_dir holds the older suite_play.gd; installed it under addons/suite_play (deprecated: replace addon_dir with addons/burrow_play)")
	}
	r.h.logf("build: %s at %s; addon %s installed; removed from the throwaway project.godot: %v", dir, st.SHA, addon.Name, removed)
	b.Prune(ctx, mirror, dir)
	if cfg.SkipImport {
		return dir, nil
	}
	progress("Importing assets")
	logPath := filepath.Join(cfg.LogsDir, "import-"+st.SessionID+".log")
	err = build.Import(ctx, cfg.Godot, dir, st.SessionID, logPath, cfg.ImportTimeout, cfg.ProgressEvery, func(el time.Duration) {
		progress(fmt.Sprintf("Importing assets (%ds)", int(el.Seconds())))
	})
	return dir, err
}

func (r *realStages) Remove(ctx context.Context, st protocol.SessionStart, dir string) error {
	cfg := r.h.cfg
	repo, err := cfg.Build.FindRepo(st.RepoURL)
	if err != nil {
		return err
	}
	b := &build.Builder{Cfg: cfg.Build, Logf: r.h.logf}
	return b.RemoveCheckout(ctx, cfg.Build.MirrorDir(repo), dir)
}

func (r *realStages) AssertHead(ctx context.Context, dir, sha string) error {
	return (&build.Builder{Cfg: r.h.cfg.Build}).AssertHead(ctx, dir, sha)
}

// Engine implements EngineStages from the checkout's contents.
func (r *realStages) Engine(dir string) (engine.Kind, error) { return engine.Detect(dir) }

// Capture implements EngineStages: the session-0 guard, then the game window.
func (r *realStages) Capture(ctx context.Context, p launch.Proc, st protocol.SessionStart) (capture.Source, error) {
	cc := capture.Real(p.Pids)
	cc.Backend = r.h.cfg.CaptureBackend
	cc.Timeout = r.h.cfg.WindowTimeout
	cc.FPS = st.Encoder.FPS
	return capture.Start(ctx, cc)
}

// Input implements InputStages: SendInput and ViGEm aimed at the captured
// window. It refuses in session 0, like capture.
func (r *realStages) Input(p launch.Proc, src capture.Source) (*inject.Injector, error) {
	t, ok := src.(capture.Targeted)
	if !ok {
		return nil, fmt.Errorf("input: capture source %s has no window to aim at", src.Name())
	}
	fw, fh := src.Size()
	return inject.NewReal(capture.Real(p.Pids).Env, inject.RealOptions{
		Client: t.ClientRect, Frame: func() (int, int) { return fw, fh }, Window: t.WindowHandle, Pids: p.Pids, Logf: r.h.logf,
	})
}

func (r *realStages) Launch(dir string, st protocol.SessionStart, logPath string) (launch.Proc, error) {
	cfg := r.h.cfg
	if k, err := engine.Detect(dir); err == nil && k != engine.Godot {
		return nil, fmt.Errorf("launching a %s build is not wired into this host yet", k)
	}
	args := launch.GodotArgsForDriver(dir, cfg.RenderingDriver, st.Encoder.Width, st.Encoder.Height, st.SessionID, cfg.GodotPort, st.Encoder.FPS)
	r.h.logf("launch: %s %v", cfg.Godot, args)
	return launch.Start(launch.Spec{Path: cfg.Godot, Args: args, Dir: dir, LogPath: logPath})
}
