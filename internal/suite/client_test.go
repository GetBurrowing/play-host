package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeCore speaks enough of Phoenix v2 to exercise the client: it checks the
// socket params, answers phx_join and heartbeats, and can push a broadcast
// or drop the connection.
type fakeCore struct {
	t        *testing.T
	token    string
	mu       sync.Mutex
	joins    []map[string]any
	pushes   []string
	conns    []*websocket.Conn
	beats    int
	answerHB bool
	// wmu serialises the fake server's writes: its read loop answers
	// frames while broadcast writes from the test goroutine, and gorilla
	// allows one concurrent writer (the -race failure stage 3 recorded as
	// pre-existing; fixed in 01a0dbd6 stage 6).
	wmu sync.Mutex
}

func (f *fakeCore) write(c *websocket.Conn, b []byte) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_ = c.WriteMessage(websocket.TextMessage, b)
}

func (f *fakeCore) handler() http.Handler {
	up := websocket.Upgrader{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/runtime/ws/websocket" || q.Get("vsn") != "2.0.0" || q.Get("runtime_id") != "play-host-test" || q.Get("token") != f.token {
			http.Error(w, "unauthorized", 403)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			fr, err := DecodeFrame(b)
			if err != nil {
				f.t.Errorf("bad frame from client: %s", b)
				return
			}
			switch {
			case fr.Event == "phx_join":
				if fr.JoinRef == nil || fr.Ref == nil || *fr.JoinRef != *fr.Ref {
					f.t.Errorf("phx_join join_ref/ref: %s", b)
				}
				var p map[string]any
				_ = json.Unmarshal(fr.Payload, &p)
				f.mu.Lock()
				f.joins = append(f.joins, p)
				f.mu.Unlock()
				out, _ := EncodeFrame(fr.JoinRef, fr.Ref, fr.Topic, "phx_reply", map[string]any{"status": "ok", "response": map[string]any{}})
				f.write(c, out)
			case fr.Topic == "phoenix" && fr.Event == "heartbeat":
				f.mu.Lock()
				f.beats++
				ans := f.answerHB
				f.mu.Unlock()
				if ans {
					out, _ := EncodeFrame(nil, fr.Ref, "phoenix", "phx_reply", map[string]any{"status": "ok", "response": map[string]any{}})
					f.write(c, out)
				}
			default:
				f.mu.Lock()
				f.pushes = append(f.pushes, fr.Event+" "+string(fr.Payload))
				f.mu.Unlock()
			}
		}
	})
}

func (f *fakeCore) broadcast(event string, payload any) {
	f.mu.Lock()
	c := f.conns[len(f.conns)-1]
	f.mu.Unlock()
	out, _ := EncodeFrame(nil, nil, "runtime:play-host-test", event, payload)
	f.write(c, out)
}

func (f *fakeCore) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
}

type recorder struct {
	mu        sync.Mutex
	events    []string
	connected chan struct{}
	dropped   chan error
}

func (r *recorder) OnEvent(e string, p json.RawMessage) {
	r.mu.Lock()
	r.events = append(r.events, e+" "+string(p))
	r.mu.Unlock()
}
func (r *recorder) OnConnected()             { r.connected <- struct{}{} }
func (r *recorder) OnDisconnected(err error) { r.dropped <- err }

func wait[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

func setup(t *testing.T, hb time.Duration, answerHB bool) (*fakeCore, *recorder, *Client, context.CancelFunc) {
	t.Helper()
	fc := &fakeCore{t: t, token: "tok-secret-123", answerHB: answerHB}
	srv := httptest.NewServer(fc.handler())
	t.Cleanup(srv.Close)
	tf := filepath.Join(t.TempDir(), "runtime-token")
	if err := os.WriteFile(tf, []byte(fc.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{connected: make(chan struct{}, 4), dropped: make(chan error, 4)}
	var logs []string
	var lmu sync.Mutex
	c := New(Config{
		URL: "http" + strings.TrimPrefix(srv.URL, "http") + "/runtime/ws", RuntimeID: "play-host-test", TokenFile: tf,
		Version: "test", Heartbeat: hb, BackoffMin: 20 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
		Logf: func(f string, a ...any) {
			lmu.Lock()
			logs = append(logs, f)
			lmu.Unlock()
		},
	}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() {
		cancel()
		lmu.Lock()
		defer lmu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, fc.token) {
				t.Errorf("token leaked into log format: %q", l)
			}
		}
	})
	return fc, rec, c, cancel
}

func TestJoinDeclaresFeatureAndRoutesBroadcasts(t *testing.T) {
	fc, rec, c, _ := setup(t, time.Second, true)
	wait(t, rec.connected, "join")
	fc.mu.Lock()
	ci := fc.joins[0]["client_info"].(map[string]any)
	fc.mu.Unlock()
	feats := ci["features"].([]any)
	// game_stream_multi is what makes core send play_peer_open (task
	// 01a0dbd6): without it core keeps v1 wiring and never asks for a fresh
	// offer on a same-page re-attach.
	// game_stream_touch (task 01a0fe45) turns on core's touch surface.
	if len(feats) != 3 || feats[0] != "game_stream_host" || feats[1] != "game_stream_multi" || feats[2] != "game_stream_touch" || ci["product"] != "play-host" {
		t.Fatalf("client_info %v", ci)
	}

	fc.broadcast("play_session_start", map[string]any{"session_id": "s1"})
	if err := c.Push("play_session_status", map[string]any{"session_id": "s1", "state": "building"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.events)
		rec.mu.Unlock()
		fc.mu.Lock()
		m := len(fc.pushes)
		fc.mu.Unlock()
		if n == 1 && m == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 1 || !strings.HasPrefix(rec.events[0], `play_session_start {"session_id":"s1"}`) {
		t.Fatalf("events %v", rec.events)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.pushes) != 1 || !strings.Contains(fc.pushes[0], `"state":"building"`) {
		t.Fatalf("pushes %v", fc.pushes)
	}
}

func TestReconnectsAfterDrop(t *testing.T) {
	fc, rec, c, _ := setup(t, time.Second, true)
	wait(t, rec.connected, "first join")
	fc.dropAll()
	wait(t, rec.dropped, "disconnect")
	wait(t, rec.connected, "rejoin")
	if !c.Connected() {
		t.Fatal("not connected after rejoin")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.joins) != 2 {
		t.Fatalf("joins %d", len(fc.joins))
	}
}

func TestMissedHeartbeatReplyDropsSocket(t *testing.T) {
	fc, rec, _, _ := setup(t, 60*time.Millisecond, false)
	wait(t, rec.connected, "join")
	err := wait(t, rec.dropped, "heartbeat timeout")
	if err == nil {
		t.Fatal("no error on heartbeat drop")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.beats < 1 {
		t.Fatal("never heartbeated")
	}
}

func TestBadTokenNeverJoinsAndIsNotLogged(t *testing.T) {
	fc := &fakeCore{t: t, token: "right-token", answerHB: true}
	srv := httptest.NewServer(fc.handler())
	defer srv.Close()
	tf := filepath.Join(t.TempDir(), "runtime-token")
	_ = os.WriteFile(tf, []byte("wrong-token-xyz"), 0o600)
	rec := &recorder{connected: make(chan struct{}, 4), dropped: make(chan error, 4)}
	var mu sync.Mutex
	var logs []string
	c := New(Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/runtime/ws", RuntimeID: "play-host-test", TokenFile: tf,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		Logf: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			mu.Unlock()
		}}, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	select {
	case <-rec.connected:
		t.Fatal("joined with a wrong token")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("no reconnect attempts logged")
	}
	for _, l := range logs {
		if strings.Contains(l, "wrong-token-xyz") {
			t.Fatalf("token in log: %s", l)
		}
	}
}

func TestSocketURL(t *testing.T) {
	u, err := SocketURL("https://suite.example/runtime/ws", "rt", "a b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "wss://suite.example/runtime/ws/websocket?") || !strings.Contains(u, "token=a+b") || !strings.Contains(u, "vsn=2.0.0") {
		t.Fatal(u)
	}
	if _, err := SocketURL("ftp://x", "rt", "t"); err == nil {
		t.Fatal("ftp accepted")
	}
	if r := redact("wss://h/runtime/ws/websocket?token=zzz"); strings.Contains(r, "zzz") {
		t.Fatal(r)
	}
}

func TestJitterBounds(t *testing.T) {
	for range 200 {
		j := Jitter(time.Second)
		if j < 500*time.Millisecond || j >= time.Second {
			t.Fatalf("jitter %s", j)
		}
	}
}

// Stage 6 (01a0dbd6): a heartbeat whose WRITE waited behind other writes for
// longer than an interval used to be declared missed on the ticker's
// buffered tick, microseconds after it went out, closing a healthy socket.
// The write lock is held here for 1.6 intervals (standing in for a long
// write; under the 3-interval read deadline), and core answers every beat:
// the socket must stay up.
func TestALateHeartbeatWriteIsNotAMissedReply(t *testing.T) {
	fc, rec, c, _ := setup(t, 50*time.Millisecond, true)
	wait(t, rec.connected, "join")
	time.Sleep(120 * time.Millisecond) // a couple of answered beats first
	c.mu.Lock()
	time.Sleep(80 * time.Millisecond)
	c.mu.Unlock()
	select {
	case err := <-rec.dropped:
		t.Fatalf("a healthy socket was dropped after a late heartbeat write: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.beats < 3 {
		t.Fatalf("only %d beats", fc.beats)
	}
}

// The Features override is the negative control for core's touch gate
// (task 01a0fe45): a host configured without FeatureTouch does not declare
// it, and the default does.
func TestFeaturesOverrideDropsTouch(t *testing.T) {
	def := New(Config{}, nil).features()
	if fmt.Sprint(def) != "[game_stream_host game_stream_multi game_stream_touch]" {
		t.Fatalf("default features %v", def)
	}
	got := New(Config{Features: []string{Feature, FeatureMulti}}, nil).features()
	for _, f := range got {
		if f == FeatureTouch {
			t.Fatalf("override kept %s: %v", FeatureTouch, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("override %v", got)
	}
}

func TestFeaturesPerSlotTable(t *testing.T) {
	cases := []struct {
		name     string
		perSlot  bool
		features []string
		want     []string
	}{
		{"off", false, nil, []string{Feature, FeatureMulti, FeatureTouch}},
		{"on", true, nil, []string{Feature, FeatureMulti, FeatureTouch, FeaturePerSlot}},
		{"on, dev list lacking multi", true, []string{Feature}, []string{Feature}},
		{"on, dev list with multi", true, []string{Feature, FeatureMulti}, []string{Feature, FeatureMulti, FeaturePerSlot}},
		{"off, dev list", false, []string{Feature}, []string{Feature}},
	}
	for _, tc := range cases {
		var logs []string
		c := New(Config{URL: "ws://x", RuntimeID: "r", Features: tc.features, PerSlot: tc.perSlot,
			Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}, nil)
		got := c.features()
		c.features()
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: features = %v, want %v", tc.name, got, tc.want)
		}
		if tc.name == "on, dev list lacking multi" && len(logs) != 1 {
			t.Fatalf("%s: warning logged %d times, want once", tc.name, len(logs))
		}
	}
}
