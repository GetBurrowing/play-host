// Package nativehost is Play's non-streaming provider. Only immutable,
// operator-allowlisted images run, in labeled rootless podman containers.
package nativehost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Startup-Suite/play-host/internal/protocol"
)

type Sender interface{ Push(string, any) error }
type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
}
type Podman struct{ Binary string }

func (p Podman) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, p.Binary, args...).Output()
}

type Config struct {
	Enabled      bool     `json:"enabled"`
	Podman       string   `json:"podman"`
	RuntimeID    string   `json:"-"`
	StateDir     string   `json:"state_dir"`
	PublicHost   string   `json:"public_host"`
	AdmissionURL string   `json:"admission_url"`
	Images       []string `json:"images"`
	Ports        []int    `json:"ports"`
}

func (c Config) Validate() error {
	u, e := url.Parse(c.AdmissionURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("native admission_url must be HTTPS without credentials or query")
	}
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) || c.PublicHost == "" || strings.ContainsAny(c.PublicHost, " /:\n\r") || len(c.Images) == 0 || len(c.Ports) == 0 {
		return errors.New("native state_dir, public_host, images and ports required")
	}
	seen := map[int]bool{}
	for _, p := range c.Ports {
		if p < 1024 || p > 65535 || seen[p] {
			return errors.New("invalid native UDP pool")
		}
		seen[p] = true
	}
	for _, im := range c.Images {
		if !imageRE.MatchString(im) {
			return errors.New("native images must be pinned digests")
		}
	}
	return nil
}

var idRE = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var imageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)

type session struct {
	Start          protocol.NativeStart `json:"start"`
	Port           int                  `json:"port"`
	CredentialHash string               `json:"credential_hash"`
	Started        time.Time            `json:"started"`
	LastRoster     time.Time            `json:"-"`
	Admissions     []string             `json:"-"`
	FailureReason  string               `json:"-"`
}
type Host struct {
	mu           sync.Mutex
	cfg          Config
	runner       Runner
	sender       Sender
	sessions     map[string]*session
	connected    bool
	disconnected time.Time
	lastControl  time.Time
}

func New(c Config, r Runner) (*Host, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if r == nil {
		if c.Podman == "" {
			c.Podman = "podman"
		}
		r = Podman{c.Podman}
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	h := &Host{cfg: c, runner: r, sessions: map[string]*session{}}
	if err := h.recover(); err != nil {
		return nil, err
	}
	return h, nil
}
func (h *Host) SetSender(s Sender) { h.mu.Lock(); defer h.mu.Unlock(); h.sender = s }
func (h *Host) label() string {
	v := sha256.Sum256([]byte(h.cfg.RuntimeID + "\n" + strings.TrimRight(h.cfg.AdmissionURL, "/")))
	return "burrow.native.owner=" + hex.EncodeToString(v[:8])
}
func (h *Host) name(id string) string { return "burrow-native-" + id }
func (h *Host) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.runner.Run(ctx, args...)
}
func (h *Host) send(s *session, state string) {
	if h.sender != nil {
		_ = h.sender.Push(protocol.EventNativeStatus, protocol.NativeStatus{SessionID: s.Start.SessionID, Generation: s.Start.Generation, State: state, Reason: s.FailureReason, Endpoint: net.JoinHostPort(h.cfg.PublicHost, strconv.Itoa(s.Port)), CredentialHash: s.CredentialHash, Admissions: append([]string{}, s.Admissions...)})
	}
}
func (h *Host) OnEvent(event string, payload json.RawMessage) {
	// Do not block the websocket reader on podman I/O.
	go func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if event == protocol.EventNativeStart || event == protocol.EventNativeStop || event == protocol.EventNativeReconcile {
			h.lastControl = time.Now()
		}
		switch event {
		case protocol.EventNativeStart:
			var s protocol.NativeStart
			if json.Unmarshal(payload, &s) == nil {
				h.start(s)
			}
		case protocol.EventNativeStop, protocol.EventNativeReconcile:
			var p struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(payload, &p) != nil || !idRE.MatchString(p.SessionID) {
				return
			}
			if s := h.sessions[p.SessionID]; s != nil {
				if event == protocol.EventNativeStop {
					h.stop(s)
				} else {
					h.refresh(s)
				}
			}
		}
	}()
}
func (h *Host) start(p protocol.NativeStart) {
	s := &session{Start: p, Started: time.Now(), LastRoster: time.Now()}
	if !idRE.MatchString(p.SessionID) || p.Mode != "native" || p.Generation < 1 || p.Capacity < 1 || p.Capacity > 8 || p.ProtocolVersion < 1 || (p.IdleTimeoutS != 300 && p.IdleTimeoutS != 600 && p.IdleTimeoutS != 900) {
		return
	}
	image, _ := p.Build["server_image"].(string)
	allowed := false
	for _, v := range h.cfg.Images {
		if v == image {
			allowed = true
		}
	}
	if !allowed || p.Build["id"] != p.BuildID {
		s.FailureReason = "image_not_allowed"
		h.send(s, "failed")
		return
	}
	if old := h.sessions[p.SessionID]; old != nil {
		if old.Start.Generation != p.Generation || old.Start.BuildID != p.BuildID {
			h.send(s, "failed")
			return
		}
		h.refresh(old)
		return
	}
	used := map[int]bool{}
	for _, a := range h.sessions {
		used[a.Port] = true
	}
	for _, port := range h.cfg.Ports {
		if !used[port] {
			s.Port = port
			break
		}
	}
	if s.Port == 0 {
		s.FailureReason = "host_full"
		h.send(s, "failed")
		return
	}
	dir := filepath.Join(h.cfg.StateDir, p.SessionID)
	if os.MkdirAll(filepath.Join(dir, "state"), 0700) != nil {
		h.send(s, "failed")
		return
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		h.send(s, "failed")
		return
	}
	credential := hex.EncodeToString(raw)
	digest := sha256.Sum256([]byte(credential))
	s.CredentialHash = hex.EncodeToString(digest[:])
	config := map[string]any{"session_id": p.SessionID, "build_id": p.BuildID, "generation": p.Generation, "protocol_version": p.ProtocolVersion, "credential": credential, "admission_url": strings.TrimRight(h.cfg.AdmissionURL, "/") + "/" + p.SessionID, "control_url": strings.TrimSuffix(strings.TrimRight(h.cfg.AdmissionURL, "/"), "/admit") + "/control/" + p.SessionID, "roster_file": "/run/native-state/roster.json"}
	b, _ := json.Marshal(config)
	if os.WriteFile(filepath.Join(dir, "config.json"), b, 0600) != nil {
		h.send(s, "failed")
		return
	}
	meta, _ := json.Marshal(s)
	if os.WriteFile(filepath.Join(dir, "session.json"), meta, 0600) != nil {
		h.send(s, "failed")
		return
	}
	_, err := h.run("run", "-d", "--name", h.name(p.SessionID), "--label", h.label(), "--label", "burrow.native.session="+p.SessionID,
		"--label", "burrow.native.generation="+strconv.Itoa(p.Generation), "--pull=never", "--userns=keep-id", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--memory=2g", "--cpus=2", "--pids-limit=128", "--tmpfs", "/tmp:rw,size=128m", "-p", strconv.Itoa(s.Port)+":24972/udp",
		"--mount", "type=bind,src="+filepath.Join(dir, "config.json")+",dst=/run/native/config.json,ro=true",
		"--mount", "type=bind,src="+filepath.Join(dir, "state")+",dst=/run/native-state",
		"--env", "GAME_NATIVE_CONFIG_FILE=/run/native/config.json", "--env", "HOME=/tmp", "--env", "XDG_DATA_HOME=/tmp", image)
	if err != nil {
		h.send(s, "failed")
		return
	}
	h.sessions[p.SessionID] = s
	h.send(s, "running")
}
func (h *Host) owns(s *session) bool {
	b, e := h.run("inspect", "--format", "{{json .Config.Labels}}", h.name(s.Start.SessionID))
	if e != nil {
		return false
	}
	var labels map[string]string
	if json.Unmarshal(b, &labels) != nil {
		return false
	}
	pair := strings.SplitN(h.label(), "=", 2)
	return labels[pair[0]] == pair[1] && labels["burrow.native.session"] == s.Start.SessionID && labels["burrow.native.generation"] == strconv.Itoa(s.Start.Generation)
}
func (h *Host) stop(s *session) {
	if !h.owns(s) {
		h.send(s, "failed")
		delete(h.sessions, s.Start.SessionID)
		return
	}
	if _, e := h.run("rm", "-f", h.name(s.Start.SessionID)); e != nil {
		return
	}
	h.send(s, "ended")
	delete(h.sessions, s.Start.SessionID)
	_ = os.RemoveAll(filepath.Join(h.cfg.StateDir, s.Start.SessionID))
}
func (h *Host) refresh(s *session) {
	if !h.owns(s) {
		h.send(s, "failed")
		delete(h.sessions, s.Start.SessionID)
		return
	}
	b, e := h.run("inspect", "--format", "{{.State.Running}}", h.name(s.Start.SessionID))
	if e != nil || strings.TrimSpace(string(b)) != "true" {
		h.stop(s)
		return
	}
	roster := filepath.Join(h.cfg.StateDir, s.Start.SessionID, "state", "roster.json")
	info, e := os.Stat(roster)
	if e == nil && info.Size() < 8192 && time.Since(info.ModTime()) < 20*time.Second {
		var ids []string
		b, e := os.ReadFile(roster)
		if e == nil && json.Unmarshal(b, &ids) == nil && len(ids) <= s.Start.Capacity {
			valid := true
			for _, id := range ids {
				if !idRE.MatchString(id) {
					valid = false
				}
			}
			if valid {
				s.Admissions = ids
				s.LastRoster = time.Now()
			}
		}
	}
	// A dead/unreporting game is not a forever-running phantom roster.
	if time.Since(s.LastRoster) > 60*time.Second {
		h.stop(s)
		return
	}
	h.send(s, "running")
}
func (h *Host) recover() error {
	files, e := os.ReadDir(h.cfg.StateDir)
	if e != nil {
		return e
	}
	for _, f := range files {
		if !f.IsDir() || !idRE.MatchString(f.Name()) {
			continue
		}
		b, e := os.ReadFile(filepath.Join(h.cfg.StateDir, f.Name(), "session.json"))
		if e != nil {
			continue
		}
		var s session
		if json.Unmarshal(b, &s) != nil || s.Start.SessionID != f.Name() {
			continue
		}
		s.LastRoster = time.Now()
		if h.owns(&s) {
			h.sessions[f.Name()] = &s
		}
	}
	// Remove only OUR labeled orphan containers, never stream or another runtime's.
	b, e := h.run("ps", "-a", "--filter", "label="+h.label(), "--format", "{{.Names}}")
	if e != nil {
		return fmt.Errorf("native podman inventory unavailable")
	}
	for _, name := range strings.Fields(string(b)) {
		id := strings.TrimPrefix(name, "burrow-native-")
		if name == h.name(id) && idRE.MatchString(id) && h.sessions[id] == nil {
			_, _ = h.run("rm", "-f", name)
		}
	}
	return nil
}
func (h *Host) OnConnected() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected = true
	h.lastControl = time.Now()
	for _, s := range h.sessions {
		h.refresh(s)
	}
}
func (h *Host) OnDisconnected(_ error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connected = false
	h.disconnected = time.Now()
}
func (h *Host) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.mu.Lock()
			for _, s := range h.sessions {
				if (!h.connected && !h.disconnected.IsZero() && time.Since(h.disconnected) > 120*time.Second) || (!h.lastControl.IsZero() && time.Since(h.lastControl) > 120*time.Second) {
					h.stop(s)
				} else {
					h.refresh(s)
				}
			}
			h.mu.Unlock()
		}
	}
}
