package nativehost

import (
	"context"
	"encoding/json"
	"github.com/Startup-Suite/play-host/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	calls  [][]string
	labels map[string]string
	names  string
	fail   bool
}

func (f *fakeRunner) Run(_ context.Context, a ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{}, a...))
	if a[0] == "ps" {
		return []byte(f.names), nil
	}
	if a[0] == "inspect" {
		if strings.Contains(a[2], "Labels") {
			return json.Marshal(f.labels)
		}
		return []byte("true"), nil
	}
	if a[0] == "run" {
		f.names = a[3]
		for i, v := range a {
			if v == "--label" {
				p := strings.SplitN(a[i+1], "=", 2)
				f.labels[p[0]] = p[1]
			}
		}
	}
	return []byte("container"), nil
}

type sender struct {
	mu       sync.Mutex
	statuses []protocol.NativeStatus
}

func (s *sender) Push(_ string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, v.(protocol.NativeStatus))
	return nil
}

const sid = "019d1357-cb08-7023-b165-fd19e9462316"

var image = "example.com/voltron@sha256:" + strings.Repeat("a", 64)

func config(t *testing.T) Config {
	return Config{RuntimeID: "native-a", StateDir: t.TempDir(), PublicHost: "native.example.com", AdmissionURL: "https://burrow.example/api/ext/play/admit", Images: []string{image}, Ports: []int{24973}}
}
func startPayload() protocol.NativeStart {
	return protocol.NativeStart{SessionID: sid, Mode: "native", Generation: 1, BuildID: "build-1", ProtocolVersion: 1, Capacity: 4, IdleTimeoutS: 600, Build: map[string]any{"id": "build-1", "server_image": image}}
}
func TestStartIsIdempotentPinnedAndSecretFileOnly(t *testing.T) {
	c := config(t)
	r := &fakeRunner{labels: map[string]string{}}
	h, e := New(c, r)
	if e != nil {
		t.Fatal(e)
	}
	s := &sender{}
	h.SetSender(s)
	p := startPayload()
	h.start(p)
	h.start(p)
	runs := 0
	var argv string
	for _, a := range r.calls {
		if a[0] == "run" {
			runs++
			argv = strings.Join(a, " ")
		}
	}
	if runs != 1 {
		t.Fatalf("runs=%d", runs)
	}
	for _, v := range []string{"--pull=never", "--read-only", "--memory=2g", "--cap-drop=ALL", "24973:24972/udp", image} {
		if !strings.Contains(argv, v) {
			t.Error(v)
		}
	}
	var cfg map[string]any
	b, e := os.ReadFile(filepath.Join(c.StateDir, sid, "config.json"))
	if e != nil {
		t.Fatal(e)
	}
	if json.Unmarshal(b, &cfg) != nil {
		t.Fatal("config")
	}
	if strings.Contains(argv, cfg["credential"].(string)) {
		t.Fatal("credential in argv")
	}
	st, _ := os.Stat(filepath.Join(c.StateDir, sid, "config.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	p.BuildID = "changed"
	h.start(p)
	if s.statuses[len(s.statuses)-1].State != "failed" {
		t.Fatal("changed identity accepted")
	}
}
func TestFullHostAndUnallowlistedImageFailWithoutStarting(t *testing.T) {
	r := &fakeRunner{labels: map[string]string{}}
	h, _ := New(config(t), r)
	s := &sender{}
	h.SetSender(s)
	p := startPayload()
	p.Build["server_image"] = "evil/image:latest"
	h.start(p)
	if len(h.sessions) != 0 || s.statuses[0].State != "failed" {
		t.Fatal("unallowlisted image accepted")
	}
	p = startPayload()
	h.start(p)
	p.SessionID = "019d1357-cb08-7023-b165-fd19e9462317"
	h.start(p)
	if len(h.sessions) != 1 || s.statuses[len(s.statuses)-1].State != "failed" || s.statuses[len(s.statuses)-1].Reason != "host_full" {
		t.Fatal("full host accepted")
	}
}
func TestStopNeverRemovesForeignContainer(t *testing.T) {
	r := &fakeRunner{labels: map[string]string{}}
	h, _ := New(config(t), r)
	h.start(startPayload())
	r.labels["burrow.native.owner"] = "foreign"
	before := len(r.calls)
	h.stop(h.sessions[sid])
	for _, a := range r.calls[before:] {
		if a[0] == "rm" {
			t.Fatal("removed foreign container")
		}
	}
}
func TestRestartReconcilesLabelledSession(t *testing.T) {
	c := config(t)
	r := &fakeRunner{labels: map[string]string{}}
	h, _ := New(c, r)
	h.start(startPayload())
	h2, e := New(c, r)
	if e != nil {
		t.Fatal(e)
	}
	if len(h2.sessions) != 1 {
		t.Fatal("lost durable handle")
	}
	h2.sessions[sid].LastRoster = time.Now().Add(-61 * time.Second)
	h2.refresh(h2.sessions[sid])
	if len(h2.sessions) != 0 {
		t.Fatal("unreported roster kept alive")
	}
}
func TestConfigRejectsHTTPAndMutableImages(t *testing.T) {
	c := config(t)
	c.AdmissionURL = "http://example.com"
	if c.Validate() == nil {
		t.Fatal("HTTP accepted")
	}
	c = config(t)
	c.Images = []string{"game:latest"}
	if c.Validate() == nil {
		t.Fatal("mutable image accepted")
	}
}
