package dryrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Startup-Suite/play-host/internal/host"
	"github.com/Startup-Suite/play-host/internal/media"
	"github.com/Startup-Suite/play-host/internal/probe"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"github.com/Startup-Suite/play-host/internal/unreal"
)

// Preset is one cell of the encoder axis.
type Preset struct{ Preset, Tune string }

func (p Preset) String() string { return p.Preset + "/" + p.Tune }

// DefaultMatrix is the encoder axis: p1/ll and p4/ll. Only ddagrab exists, so
// the backend axis has one value; the runbook records that.
var DefaultMatrix = []Preset{{"p1", "ll"}, {"p4", "ll"}}

// ProbeResult is what a Prober measured: one RTT per answered probe.
type ProbeResult struct {
	RTTMs []float64
	Lost  int
	FPS   float64
}

// Prober sends n probes at 10 Hz through the viewer reachable at signalURL
// (the loopback core's /offer and /answer) and returns the round trips.
type Prober interface {
	Probe(ctx context.Context, signalURL, label string, n int) (ProbeResult, error)
}

// Options configure one dry run.
type Options struct {
	Repo   string // repo_url sent in play_session_start
	SHA    string // 40 hex
	Host   host.Config
	Stages host.Stages // nil = the real git, UAT, launch, capture and input stages
	Prober Prober
	Matrix []Preset // nil = DefaultMatrix; the first cell is the cold and warm build
	Probes int      // per cell, 0 = 150

	// WipeCaches empties ddc_dir and removes this commit's archive before the
	// cold pass. Without it a populated cache fails the run: a cold number
	// measured on a warm cache would be a lie.
	WipeCaches bool

	UEVersion string
	GPU       string
	Backend   string
	Encoder   string
	// ArchiveDir is where this commit's packaged build lives, for the cold
	// pre-flight ("" = none to check, as with fakes).
	ArchiveDir string

	BuildTimeout time.Duration // wait for connecting; 0 = 2h
	Logf         func(string, ...any)
}

// Run executes cold, the other matrix cells, then warm, and returns the report.
func Run(ctx context.Context, o Options) (*Report, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Probes == 0 {
		o.Probes = 150
	}
	if len(o.Matrix) == 0 {
		o.Matrix = DefaultMatrix
	}
	if o.BuildTimeout == 0 {
		o.BuildTimeout = 2 * time.Hour
	}
	if o.Prober == nil {
		return nil, errors.New("dryrun: no prober")
	}
	if err := coldPreflight(&o); err != nil {
		return nil, err
	}
	rep := &Report{SHA: o.SHA, UEVersion: o.UEVersion, GPU: o.GPU, CaptureBackend: o.Backend, Encoder: o.Encoder, Probe: []ProbeRow{}}
	backend := o.Backend
	for i, cell := range o.Matrix {
		label := fmt.Sprintf("%s-%s", cell.Preset, cell.Tune)
		o.Logf("dryrun: %s session %d/%d (%s)", map[bool]string{true: "cold", false: "probe"}[i == 0], i+1, len(o.Matrix), cell)
		// The first cell is the cold build; the others reuse its archive.
		res, err := runSession(ctx, o, label, cell, i == 0, o.Probes)
		if err != nil {
			return nil, fmt.Errorf("%s session: %w", label, err)
		}
		if i == 0 {
			rep.BuildMS.Cold = res.build
		}
		sum := probe.Summarize(res.probe.RTTMs)
		rep.Probe = append(rep.Probe, ProbeRow{Backend: backend, Preset: cell.String(), Kind: KindHostToPhoton,
			N: sum.N, Lost: res.probe.Lost, P50: sum.P50, P95: sum.P95, FPS: res.probe.FPS})
	}
	o.Logf("dryrun: warm session (same ddc, fresh checkout, archive cache off)")
	warm, err := runSession(ctx, o, "warm", o.Matrix[0], true, 0)
	if err != nil {
		return nil, fmt.Errorf("warm session: %w", err)
	}
	rep.BuildMS.Warm = warm.build
	return rep, nil
}

func coldPreflight(o *Options) error {
	ddc := o.Host.Unreal.DDCDir
	if ddc == "" && o.ArchiveDir == "" {
		return nil
	}
	dirty := func(p string) bool {
		if p == "" {
			return false
		}
		es, err := os.ReadDir(p)
		return err == nil && len(es) > 0
	}
	if o.ArchiveDir != "" {
		if _, err := os.Stat(o.ArchiveDir); err == nil {
			if !o.WipeCaches {
				return fmt.Errorf("cold run needs no archive for this commit, but %s exists; pass --wipe-caches", o.ArchiveDir)
			}
			if err := os.RemoveAll(o.ArchiveDir); err != nil {
				return err
			}
		}
	}
	if dirty(ddc) {
		if !o.WipeCaches {
			return fmt.Errorf("cold run needs an empty ddc_dir, but %s has files; pass --wipe-caches", ddc)
		}
		if err := os.RemoveAll(ddc); err != nil {
			return err
		}
		if err := os.MkdirAll(ddc, 0o755); err != nil {
			return err
		}
	}
	return nil
}

type sessionResult struct {
	build BuildPhase
	probe ProbeResult
}

func runSession(ctx context.Context, o Options, label string, cell Preset, noArchive bool, probes int) (sessionResult, error) {
	var res sessionResult
	cfg := o.Host
	cfg.Unreal.NoArchiveCache = noArchive
	core := newLoopbackCore()
	h := host.New(cfg, core, o.Stages, o.Logf)
	core.h = h
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return res, err
	}
	srv := &http.Server{Handler: core.mux()}
	go srv.Serve(ln)
	defer srv.Close()

	id := "dryrun-" + label
	enc := media.DefaultPreset()
	enc.Encoder = ""
	enc.Preset, enc.Tune = cell.Preset, cell.Tune
	payload, _ := json.Marshal(map[string]any{"session_id": id, "repo_url": o.Repo, "branch": "dryrun", "sha": o.SHA,
		"ice_servers": []any{}, "idle_timeout_s": 3600, "encoder": enc})
	core.id = id
	t0 := time.Now()
	h.OnEvent(protocol.EventSessionStart, payload)
	stop := func() {
		b, _ := json.Marshal(protocol.SessionStop{SessionID: id, Reason: "dryrun done"})
		h.OnEvent(protocol.EventSessionStop, b)
		end := time.Now().Add(60 * time.Second)
		for h.Current() != nil && time.Now().Before(end) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	defer stop()

	bctx, cancel := context.WithTimeout(ctx, o.BuildTimeout)
	defer cancel()
	st, err := core.waitState(bctx, protocol.StateConnecting, protocol.StateLive)
	if err != nil {
		return res, err
	}
	res.build = core.buildPhase(t0, st.at)
	if probes > 0 {
		pr, err := o.Prober.Probe(ctx, "http://"+ln.Addr().String(), label, probes)
		if err != nil {
			return res, fmt.Errorf("probe: %w", err)
		}
		res.probe = pr
	}
	return res, nil
}

type stamp struct {
	at     time.Time
	state  string
	detail string
}

// loopbackCore stands in for Burrow: it is the host's Sender, and serves the
// /offer, /answer and /stats the spike's CDP driver (and any Prober) talks to.
type loopbackCore struct {
	h  *host.Host
	id string

	mu     sync.Mutex
	stamps []stamp
	offers chan protocol.Signal
	wake   chan struct{}
	peer   string
}

func newLoopbackCore() *loopbackCore {
	return &loopbackCore{offers: make(chan protocol.Signal, 16), wake: make(chan struct{}, 1)}
}

// Push implements host.Sender.
func (c *loopbackCore) Push(event string, payload any) error {
	b, _ := json.Marshal(payload)
	switch event {
	case protocol.EventSessionStatus:
		var s protocol.Status
		if json.Unmarshal(b, &s) == nil {
			c.mu.Lock()
			c.stamps = append(c.stamps, stamp{time.Now(), s.State, s.Detail})
			c.mu.Unlock()
			select {
			case c.wake <- struct{}{}:
			default:
			}
		}
	case protocol.EventSignal:
		var s protocol.Signal
		if json.Unmarshal(b, &s) == nil && s.Kind == protocol.KindOffer {
			select {
			case c.offers <- s:
			default:
			}
		}
	}
	return nil
}

func (c *loopbackCore) waitState(ctx context.Context, want ...string) (stamp, error) {
	seen := 0
	for {
		c.mu.Lock()
		for ; seen < len(c.stamps); seen++ {
			s := c.stamps[seen]
			for _, w := range want {
				if s.state == w {
					c.mu.Unlock()
					return s, nil
				}
			}
			if s.state == protocol.StateFailed || s.state == protocol.StateEnded {
				c.mu.Unlock()
				return s, fmt.Errorf("session %s: %s", s.state, s.detail)
			}
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-ctx.Done():
			return stamp{}, fmt.Errorf("waiting for %s: %w", strings.Join(want, "|"), ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Steps names the build phase each building status belongs to.
func stepOf(detail string) string {
	for _, s := range unreal.Steps {
		if strings.HasPrefix(detail, s) {
			return s
		}
	}
	if i := strings.IndexAny(detail, "(:"); i > 0 {
		detail = detail[:i]
	}
	detail = strings.TrimSpace(detail)
	if f := strings.Fields(detail); len(f) > 0 {
		return f[0]
	}
	return "building"
}

// buildPhase turns the building statuses into per-step milliseconds: a step
// runs from its first status to the next step's first status, the last until
// the session left building at end.
func (c *loopbackCore) buildPhase(t0, end time.Time) BuildPhase {
	c.mu.Lock()
	defer c.mu.Unlock()
	type seg struct {
		step string
		at   time.Time
	}
	var segs []seg
	for _, s := range c.stamps {
		if s.state != protocol.StateBuilding || s.at.After(end) {
			continue
		}
		st := stepOf(s.detail)
		if len(segs) == 0 || segs[len(segs)-1].step != st {
			segs = append(segs, seg{st, s.at})
		}
	}
	p := BuildPhase{Total: end.Sub(t0).Milliseconds(), PerStep: map[string]int64{}}
	for i, s := range segs {
		next := end
		if i+1 < len(segs) {
			next = segs[i+1].at
		}
		p.PerStep[s.step] += next.Sub(s.at).Milliseconds()
	}
	return p
}

func (c *loopbackCore) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /offer", func(w http.ResponseWriter, r *http.Request) {
		select {
		case off := <-c.offers:
			d, err := off.Description()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			c.mu.Lock()
			c.peer = off.PeerID
			c.mu.Unlock()
			writeJSON(w, map[string]string{"id": c.id, "sdp": d.SDP})
		case <-time.After(30 * time.Second):
			http.Error(w, "no offer from the host", 504)
		case <-r.Context().Done():
		}
	})
	m.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID, SDP string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := json.Marshal(protocol.SessionDescription{Type: "answer", SDP: in.SDP})
		c.mu.Lock()
		peer := c.peer
		c.mu.Unlock()
		sig, _ := json.Marshal(protocol.Signal{SessionID: c.id, Kind: protocol.KindAnswer, Data: data, PeerID: peer})
		c.h.OnEvent(protocol.EventSignal, sig)
		writeJSON(w, map[string]string{"ok": "1"})
	})
	m.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		// The CDP driver reads this for the addon's readback times; an
		// addon-less session has none.
		writeJSON(w, map[string]any{"session": c.id, "host_probe_to_readback_ms": []float64{}})
	})
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
