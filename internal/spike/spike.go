// Package spike is the stage-1 harness for task 01a0db5f: it launches a
// throwaway Godot scene inside a Job Object, takes frames by path A (ddagrab),
// B (gdigrab) or C (in-engine readback over 127.0.0.1 TCP), encodes them with
// an ffmpeg h264_nvenc subprocess and serves them to one browser over pion
// WebRTC. Signalling is plain HTTP on 127.0.0.1 (reach it through an ssh
// tunnel); media is direct UDP on the LAN in a fixed port range.
package spike

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/media"
	"github.com/Startup-Suite/play-host/internal/rtc"
	"github.com/pion/webrtc/v4"
)

//go:embed web
var webFS embed.FS

// Options configure one harness run.
type Options struct {
	Source          string // A | B | C
	Export          string // C only: image | async
	Framing         media.Framing
	Preset          media.Preset
	Encoder         media.EncoderKind
	Godot           string
	RenderingDriver string
	Project         string
	FFmpeg          string
	Session         string
	GodotPort       int
	RTPPort         int
	HTTPAddr        string
	HostIP          string
	UDPMin          uint16
	UDPMax          uint16
	LogDir          string
	Duration        time.Duration
	NoGodot         bool // path A/B smoke without a scene
}

// Run blocks until Duration elapses, /quit is posted, or a child dies.
func Run(o Options) error {
	if err := o.Preset.Validate(); err != nil {
		return err
	}
	if o.Encoder == "" {
		// Direct callers predate encoder selection; keep their NVENC path.
		o.Encoder = media.EncoderNVENC
	}
	if err := o.Encoder.Validate(); err != nil {
		return err
	}
	if o.Encoder == media.EncoderAuto {
		return fmt.Errorf("encoder auto must be resolved by cmd/play-host")
	}
	if err := os.MkdirAll(o.LogDir, 0o755); err != nil {
		return err
	}
	h := &harness{o: o, pending: map[int]time.Time{}, done: make(chan string, 4)}
	defer h.cleanup()
	return h.run()
}

type harness struct {
	o       Options
	godot   launch.Proc
	ff      *exec.Cmd
	ffIn    io.WriteCloser
	link    net.Conn
	linkMu  sync.Mutex
	track   webrtc.TrackLocal
	api     *webrtc.API
	peersMu sync.Mutex
	peers   map[string]*rtc.Peer
	cnt     media.Counters
	fromGd  atomic.Int64
	done    chan string

	pmu     sync.Mutex
	pending map[int]time.Time
	hostLat []float64 // probe received at host -> frame carrying it read back (path C image only)
}

func (h *harness) logf(f string, a ...any) {
	log.Printf("[%s] "+f, append([]any{h.o.Session}, a...)...)
}

func (h *harness) run() error {
	o := h.o
	var err error
	cfg := rtc.Config{PortMin: o.UDPMin, PortMax: o.UDPMax}
	if o.HostIP != "" {
		cfg.AllowIPs = []net.IP{net.ParseIP(o.HostIP)}
	}
	if h.api, err = rtc.NewAPI(cfg); err != nil {
		return err
	}
	h.peers = map[string]*rtc.Peer{}

	if !o.NoGodot {
		if err := h.startGodot(); err != nil {
			return err
		}
		if err := h.connectLink(); err != nil {
			return err
		}
	}
	if err := h.startEncoder(); err != nil {
		return err
	}
	srv := &http.Server{Addr: o.HTTPAddr, Handler: h.mux(cfg)}
	ln, err := net.Listen("tcp", o.HTTPAddr)
	if err != nil {
		return err
	}
	go srv.Serve(ln)
	defer srv.Close()
	h.logf("signalling on http://%s  source=%s export=%s framing=%s preset=%s", o.HTTPAddr, o.Source, o.Export, o.Framing, o.Preset.Label())

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var deadline <-chan time.Time
	if o.Duration > 0 {
		deadline = time.After(o.Duration)
	}
	for {
		select {
		case why := <-h.done:
			h.logf("stopping: %s", why)
			return nil
		case <-deadline:
			h.logf("stopping: duration reached")
			return nil
		case <-tick.C:
			h.logf("stats godot_frames=%d enc_frames=%d enc_packets=%d enc_bytes=%d peers=%d", h.fromGd.Load(), h.cnt.Frames.Load(), h.cnt.Packets.Load(), h.cnt.Bytes.Load(), h.peerCount())
		}
	}
}

func (h *harness) startGodot() error {
	o := h.o
	w, ht := o.Preset.Width, o.Preset.Height
	args := launch.GodotArgsForDriver(o.Project, o.RenderingDriver, w, ht, o.Session, o.GodotPort, o.Preset.FPS)
	args = append(args, launch.AddonArg("export", exportMode(o))...)
	p, err := launch.Start(launch.Spec{Path: launch.HostGodotExecutable(o.Godot), Args: args, Dir: o.Project, LogPath: filepath.Join(o.LogDir, "godot-"+o.Session+".log")})
	if err != nil {
		return err
	}
	h.godot = p
	h.logf("godot pid %d (job) args %v", p.Pid(), args)
	go func() {
		code, _ := p.Wait()
		h.done <- fmt.Sprintf("godot exited %d", code)
	}()
	return nil
}

func exportMode(o Options) string {
	if o.Source == "C" {
		if o.Export == "" {
			return "image"
		}
		return o.Export
	}
	return "none"
}

func (h *harness) connectLink() error {
	addr := fmt.Sprintf("127.0.0.1:%d", h.o.GodotPort)
	deadline := time.Now().Add(45 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.(*net.TCPConn).SetNoDelay(true)
			h.link = c
			h.logf("addon link connected %s", addr)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("addon link %s: %w", addr, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (h *harness) startEncoder() error {
	o := h.o
	var input []string
	switch o.Source {
	case "A":
		input = media.DDAGrabInputArgs(o.Preset.FPS)
	case "B":
		// The scene sets the window title in its _ready, after the addon link is up.
		time.Sleep(3 * time.Second)
		input = media.GDIGrabInputArgs("burrow-play-spike-01a0db5f", o.Preset.FPS)
	case "C":
		// Size comes from the first frame the addon exports.
		hdr, err := ReadFrameHeader(h.link)
		if err != nil {
			return fmt.Errorf("first frame: %w", err)
		}
		h.logf("first frame %dx%d kind=%d len=%d", hdr.Width, hdr.Height, hdr.Kind, hdr.Length)
		first := make([]byte, hdr.Length)
		if _, err := io.ReadFull(h.link, first); err != nil {
			return err
		}
		input = media.RawInputArgs("rgba", int(hdr.Width), int(hdr.Height), o.Preset.FPS)
		defer func() { go h.pumpFrames(hdr, first) }()
	default:
		return fmt.Errorf("unknown source %q", o.Source)
	}

	var err error
	switch o.Framing {
	case media.FramingAnnexB:
		h.track, err = rtc.NewVideoTrackSample()
	default:
		h.track, err = rtc.NewVideoTrackRTP()
	}
	if err != nil {
		return err
	}
	var rtpConn net.PacketConn
	if o.Framing != media.FramingAnnexB {
		if rtpConn, err = net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", o.RTPPort)); err != nil {
			return err
		}
		go func() {
			if err := media.ForwardRTP(rtpConn, h.track.(*webrtc.TrackLocalStaticRTP), &h.cnt); err != nil {
				h.logf("rtp forward: %v", err)
			}
		}()
	}

	args := media.CommandForEncoder(input, o.Preset, o.Encoder, o.Framing, o.RTPPort)
	h.ff = exec.Command(o.FFmpeg, args...)
	ffLog, err := os.Create(filepath.Join(o.LogDir, "ffmpeg-"+o.Session+".log"))
	if err != nil {
		return err
	}
	fmt.Fprintf(ffLog, "ffmpeg %q\n", args)
	h.ff.Stderr = ffLog
	if o.Source == "C" {
		if h.ffIn, err = h.ff.StdinPipe(); err != nil {
			return err
		}
	}
	var out io.ReadCloser
	if o.Framing == media.FramingAnnexB {
		if out, err = h.ff.StdoutPipe(); err != nil {
			return err
		}
	}
	if err := h.ff.Start(); err != nil {
		return err
	}
	h.logf("ffmpeg pid %d %v", h.ff.Process.Pid, args)
	if out != nil {
		go func() {
			if err := media.ForwardAnnexB(bufio.NewReaderSize(out, 1<<20), h.track.(*webrtc.TrackLocalStaticSample), o.Preset.FPS, &h.cnt); err != nil {
				h.logf("annexb forward: %v", err)
			}
		}()
	}
	go func() {
		err := h.ff.Wait()
		if rtpConn != nil {
			rtpConn.Close()
		}
		h.done <- fmt.Sprintf("ffmpeg exited: %v", err)
	}()
	return nil
}

// pumpFrames copies addon frames into ffmpeg's stdin and, for RGBA readback,
// decodes the probe corner to time the game-side part of the round trip.
func (h *harness) pumpFrames(hdr FrameHeader, first []byte) {
	buf := first
	for {
		if hdr.Kind == KindImageRGBA8 {
			h.noteCorner(buf, int(hdr.Width))
		}
		if _, err := h.ffIn.Write(buf); err != nil {
			h.logf("ffmpeg stdin: %v", err)
			return
		}
		h.fromGd.Add(1)
		var err error
		if hdr, err = ReadFrameHeader(h.link); err != nil {
			h.done <- fmt.Sprintf("addon link: %v", err)
			return
		}
		if cap(buf) < int(hdr.Length) {
			buf = make([]byte, hdr.Length)
		}
		buf = buf[:hdr.Length]
		if _, err := io.ReadFull(h.link, buf); err != nil {
			h.done <- fmt.Sprintf("addon link: %v", err)
			return
		}
	}
}

func (h *harness) noteCorner(pix []byte, w int) {
	seq, err := DecodeCornerRGBA(pix, w)
	if err != nil {
		return
	}
	h.pmu.Lock()
	defer h.pmu.Unlock()
	if t0, ok := h.pending[int(seq)]; ok {
		h.hostLat = append(h.hostLat, float64(time.Since(t0).Microseconds())/1000)
		delete(h.pending, int(seq))
	}
}

func (h *harness) sendProbe(seq int) {
	h.pmu.Lock()
	h.pending[seq] = time.Now()
	h.pmu.Unlock()
	h.linkMu.Lock()
	defer h.linkMu.Unlock()
	if h.link != nil {
		fmt.Fprintf(h.link, "{\"t\":\"probe\",\"seq\":%d}\n", seq)
	}
}

func (h *harness) peerCount() int { h.peersMu.Lock(); defer h.peersMu.Unlock(); return len(h.peers) }

func (h *harness) mux(cfg rtc.Config) http.Handler {
	m := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	m.Handle("GET /", http.FileServerFS(sub))
	m.HandleFunc("POST /offer", func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("v%d", time.Now().UnixNano())
		p, sdp, err := rtc.NewPeer(h.api, cfg, h.track, rtc.Callbacks{
			OnPLI: func() {
				h.logf("PLI/FIR from %s (subprocess encoder: bounded GOP %d, no forced IDR)", id, h.o.Preset.GOPFrames)
			},
			OnData: func(label string, data []byte) {
				var msg struct {
					T   string `json:"t"`
					Seq int    `json:"seq"`
				}
				if json.Unmarshal(data, &msg) == nil && msg.T == "probe" {
					h.sendProbe(msg.Seq)
				}
			},
			OnDone: func(reason string) {
				h.logf("peer %s done: %s", id, reason)
				h.peersMu.Lock()
				delete(h.peers, id)
				h.peersMu.Unlock()
			},
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		h.peersMu.Lock()
		h.peers[id] = p
		h.peersMu.Unlock()
		writeJSON(w, map[string]string{"id": id, "sdp": sdp})
	})
	m.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ ID, SDP string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		h.peersMu.Lock()
		p := h.peers[in.ID]
		h.peersMu.Unlock()
		if p == nil {
			http.Error(w, "unknown peer", 404)
			return
		}
		if err := p.SetAnswer(in.SDP); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, map[string]string{"ok": "1"})
	})
	m.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		h.pmu.Lock()
		hl := append([]float64(nil), h.hostLat...)
		h.pmu.Unlock()
		h.peersMu.Lock()
		pairs := map[string]any{}
		for id, p := range h.peers {
			pairs[id] = map[string]any{"state": p.State(), "pair": p.SelectedPair(), "pli": p.PLICount()}
		}
		h.peersMu.Unlock()
		writeJSON(w, map[string]any{
			"session": h.o.Session, "source": h.o.Source, "export": exportMode(h.o), "framing": h.o.Framing, "preset": h.o.Preset,
			"godot_frames": h.fromGd.Load(), "enc_frames": h.cnt.Frames.Load(), "enc_bytes": h.cnt.Bytes.Load(),
			"host_probe_to_readback_ms": hl, "peers": pairs,
		})
	})
	m.HandleFunc("POST /quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"ok": "1"})
		h.done <- "quit requested"
	})
	return m
}

func (h *harness) cleanup() {
	// Copy first: Close fires OnDone, which takes peersMu itself.
	h.peersMu.Lock()
	peers := make([]*rtc.Peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.peersMu.Unlock()
	for _, p := range peers {
		p.Close()
	}
	if h.ffIn != nil {
		h.ffIn.Close()
	}
	if h.ff != nil && h.ff.Process != nil {
		_ = h.ff.Process.Kill()
	}
	if h.godot != nil {
		pids, _ := h.godot.Pids()
		err := h.godot.Kill()
		h.logf("killed godot job pids=%v err=%v", pids, err)
	}
	if h.link != nil {
		h.link.Close()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
