// Package suite is a Phoenix channel (serializer v2, JSON) client for core's
// runtime websocket: `wss://<host>/runtime/ws` authenticated with
// runtime_id + token (PlatformWeb.RuntimeSocket.connect/3), joining
// `runtime:<runtime_id>` (PlatformWeb.RuntimeChannel.join/3) with
// client_info.features = ["game_stream_host", "game_stream_multi"]: the first
// is the declaration Platform.GameStream discovers hosts by, the second tells
// core this host speaks the multi-peer frames (task 01a0dbd6: play_peer_open,
// play_peer_close, play_slots, play_slot_activity, peer_id on play_signal).
//
// Frames are v2 arrays: [join_ref, ref, topic, event, payload]. The client
// heartbeats on the "phoenix" topic, treats a heartbeat not answered within
// one interval of being written as a dead socket, and reconnects with
// capped exponential backoff and jitter.
//
// Every write goes through Client.write, under one mutex: gorilla allows one
// concurrent writer, and nothing else writes to the conn (no WriteControl,
// no ping handler that writes). The Dialer's write buffer is 64 KiB, so a
// signalling frame is one websocket frame (legal either way).
//
// The token is read from a FILE on every connect (so a rotated token is
// picked up without a restart) and is never logged: it travels only in the
// websocket URL's query string, which is how Phoenix socket params work.
package suite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Feature is the client_info.features entry that marks a play host.
const Feature = "game_stream_host"

// FeatureMulti declares the multi-peer frames (core Protocol.multi_feature/0).
// Core sends play_peer_open, play_peer_close and play_slots only to a host
// that declares it, and otherwise keeps v1 single-peer wiring.
const FeatureMulti = "game_stream_multi"

// FeatureTouch declares that the host forwards touch input (core
// Protocol.touch_feature/0, task 01a0fe45). Core enables the browser's
// touch surface only for a host that declares it.
const FeatureTouch = "game_stream_touch"

// Handler receives broadcasts pushed on the runtime topic.
type Handler interface {
	// OnEvent is called for every server push on runtime:<id> other than
	// phx_reply / phx_error / phx_close. It must not block for long.
	OnEvent(event string, payload json.RawMessage)
	// OnConnected is called after a successful join; OnDisconnected when the
	// socket drops. Core waits a grace window for the host to rejoin and
	// resume a running session (protocol "Resume", task 01a0dbd6 stage 6).
	OnConnected()
	OnDisconnected(err error)
}

// Config is the connection shape.
type Config struct {
	URL        string // ws(s)://host[:port]/runtime/ws
	RuntimeID  string
	TokenFile  string
	Product    string // client_info.product; core stores unknown products as "unknown"
	Version    string
	Build      string
	Heartbeat  time.Duration // default 10s
	BackoffMin time.Duration // default 1s
	BackoffMax time.Duration // default 30s
	Logf       func(format string, args ...any)
	// Features replaces client_info.features (nil = the play host's
	// [Feature, FeatureMulti, FeatureTouch]). The stress tool sets it, and a
	// test or rig sets it to run a host WITHOUT FeatureTouch: the negative
	// control for core's touch gate.
	Features []string
	// WriteBufferSize is the websocket Dialer's write buffer (0 =
	// DefaultWriteBufferSize). gorilla sends a message larger than it as a
	// FRAGMENTED message; at its own default of 4096 bytes, most offers (about
	// 4-7 KB of SDP) went out in two frames.
	WriteBufferSize int
}

// DefaultWriteBufferSize makes every signalling frame the host sends (core
// caps play_signal data at 64 KiB) go out as a single websocket frame. It
// is tidiness, NOT the fix for task 01a0dbd6 stage 6's broken sockets:
// fragmented messages are legal and Bandit reassembled all 150 captured on
// one socket. Those breaks were bytes corrupted between host-b's NIC and the
// dev core container, by podman's rootlessport; the 64 KiB buffer does not
// prevent that (docs/multiplayer-01a0dbd6.md, "Stage 6").
const DefaultWriteBufferSize = 64 << 10

// Client is one logical connection that reconnects forever until ctx ends.
type Client struct {
	cfg Config
	h   Handler

	mu      sync.Mutex
	conn    *websocket.Conn
	joinRef string
	ref     int
	started time.Time
	wr      writeStats // this socket's writes, logged when it drops
}

// writeStats describe one socket's writes, so a drop's log line says what
// the host had just sent (task 01a0dbd6 stage 6: two drops were diagnosed
// from core's side only).
type writeStats struct {
	n, bytes  int64
	lastEvent string
	lastBytes int
	lastAt    time.Time
	lastTook  time.Duration
}

// New builds a client. Run starts it.
func New(cfg Config, h Handler) *Client {
	if cfg.Heartbeat == 0 {
		// Stage 6 (01a0dbd6): 10 s, not 30 s. A socket that stops carrying
		// the host's frames is found in 10-20 s instead of 30-60 s (the review
		// measured a 51 s stall), and the session resumes on a new socket.
		cfg.Heartbeat = 10 * time.Second
	}
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	if cfg.Product == "" {
		cfg.Product = "play-host"
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = DefaultWriteBufferSize
	}
	return &Client{cfg: cfg, h: h, started: time.Now().UTC()}
}

// Topic is the channel the host joins.
func (c *Client) Topic() string { return "runtime:" + c.cfg.RuntimeID }

// SocketURL builds the websocket URL with vsn=2.0.0 and the auth params.
// Exported for tests; never log its result.
func SocketURL(base, runtimeID, token string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("suite url scheme %q is not ws/wss", u.Scheme)
	}
	if !strings.HasSuffix(u.Path, "/websocket") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/websocket"
	}
	q := u.Query()
	q.Set("vsn", "2.0.0")
	q.Set("runtime_id", runtimeID)
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ReadToken reads the runtime token from path, trimming whitespace.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("runtime token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("runtime token file is empty")
	}
	return t, nil
}

// JoinPayload is what the host sends with phx_join.
func (c *Client) JoinPayload() map[string]any {
	ci := map[string]any{
		"product":    c.cfg.Product,
		"features":   c.features(),
		"started_at": c.started.Format(time.RFC3339),
	}
	if c.cfg.Version != "" {
		ci["version"] = c.cfg.Version
	}
	if c.cfg.Build != "" {
		ci["build"] = c.cfg.Build
	}
	return map[string]any{"client_info": ci}
}

func (c *Client) features() []string {
	if c.cfg.Features != nil {
		return c.cfg.Features
	}
	return []string{Feature, FeatureMulti, FeatureTouch}
}

// Run connects, joins and pumps until ctx is done, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) {
	backoff := c.cfg.BackoffMin
	for ctx.Err() == nil {
		t0 := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		// A connection that stayed up a while resets the backoff.
		if time.Since(t0) > 2*c.cfg.BackoffMax {
			backoff = c.cfg.BackoffMin
		}
		wait := Jitter(backoff)
		c.cfg.Logf("suite: disconnected (%v); reconnecting in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, c.cfg.BackoffMax)
	}
}

// Jitter spreads d over [d/2, d).
func Jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)))
}

type frame struct {
	JoinRef *string
	Ref     *string
	Topic   string
	Event   string
	Payload json.RawMessage
}

// EncodeFrame writes a v2 array frame.
func EncodeFrame(joinRef, ref *string, topic, event string, payload any) ([]byte, error) {
	p, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal([]any{joinRef, ref, topic, event, json.RawMessage(p)})
}

// DecodeFrame parses a v2 array frame.
func DecodeFrame(b []byte) (frame, error) {
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil {
		return frame{}, err
	}
	if len(parts) != 5 {
		return frame{}, fmt.Errorf("frame has %d parts, want 5", len(parts))
	}
	var f frame
	_ = json.Unmarshal(parts[0], &f.JoinRef)
	_ = json.Unmarshal(parts[1], &f.Ref)
	if err := json.Unmarshal(parts[2], &f.Topic); err != nil {
		return f, err
	}
	if err := json.Unmarshal(parts[3], &f.Event); err != nil {
		return f, err
	}
	f.Payload = parts[4]
	return f, nil
}

func (c *Client) nextRef() string {
	c.ref++
	return strconv.Itoa(c.ref)
}

// write sends one frame. selfJoin makes join_ref equal to the frame's own
// ref, which is what phoenix.js does for phx_join.
func (c *Client) write(conn *websocket.Conn, joinRef *string, topic, event string, payload any, selfJoin ...bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref := c.nextRef()
	if len(selfJoin) > 0 && selfJoin[0] {
		joinRef = &ref
	}
	b, err := EncodeFrame(joinRef, &ref, topic, event, payload)
	if err != nil {
		return "", err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	t0 := time.Now()
	err = conn.WriteMessage(websocket.TextMessage, b)
	c.wr.n++
	c.wr.bytes += int64(len(b))
	c.wr.lastEvent, c.wr.lastBytes, c.wr.lastAt, c.wr.lastTook = event, len(b), t0, time.Since(t0)
	return ref, err
}

// Push sends event on the runtime topic. It fails when not joined; the
// caller decides whether that matters (a session kept through a drop
// re-sends its state when it resumes).
func (c *Client) Push(event string, payload any) error {
	c.mu.Lock()
	conn, jr := c.conn, c.joinRef
	c.mu.Unlock()
	if conn == nil {
		return errors.New("suite: not connected")
	}
	_, err := c.write(conn, &jr, c.Topic(), event, payload)
	return err
}

// Connected reports whether a joined socket is up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *Client) session(ctx context.Context) error {
	token, err := ReadToken(c.cfg.TokenFile)
	if err != nil {
		return err
	}
	u, err := SocketURL(c.cfg.URL, c.cfg.RuntimeID, token)
	if err != nil {
		return err
	}
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, WriteBufferSize: c.cfg.WriteBufferSize}
	conn, resp, err := d.DialContext(ctx, u, nil)
	if err != nil {
		// Never wrap err with u: the URL carries the token.
		if resp != nil {
			return fmt.Errorf("dial: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("dial %s: %w", redact(c.cfg.URL), scrub(err, token))
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	// Join.
	c.mu.Lock()
	c.ref = 0
	c.wr = writeStats{}
	c.mu.Unlock()
	joinRef, err := c.write(conn, nil, c.Topic(), "phx_join", c.JoinPayload(), true)
	if err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, b, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("join: %w", err)
		}
		f, err := DecodeFrame(b)
		if err != nil {
			continue
		}
		if f.Event == "phx_reply" && f.Ref != nil && *f.Ref == joinRef {
			var r struct {
				Status   string          `json:"status"`
				Response json.RawMessage `json:"response"`
			}
			_ = json.Unmarshal(f.Payload, &r)
			if r.Status != "ok" {
				return fmt.Errorf("join refused: %s %s", r.Status, r.Response)
			}
			break
		}
	}
	c.mu.Lock()
	c.conn, c.joinRef = conn, joinRef
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()
	c.cfg.Logf("suite: joined %s as %s", c.Topic(), c.cfg.Product)
	c.h.OnConnected()

	// Heartbeat: a reply must arrive by the next tick, unless the beat was
	// WRITTEN less than half an interval before that tick. Stage 6
	// (01a0dbd6): the check used to be "a reply before the next tick" alone,
	// and a beat whose write waited behind other writes for longer than an
	// interval was declared missed on the ticker's buffered tick,
	// microseconds after it went out, closing a healthy socket.
	// A reply can also beat the goroutine that wrote the beat back to hbMu
	// (loopback, or that goroutine descheduled after the write): `early`
	// holds the last reply ref that matched no pending beat, so the beat is
	// not then recorded as pending forever (the same stage-6 review).
	hbErr := make(chan error, 1)
	var hbMu sync.Mutex
	pending, early := "", ""
	var sentAt time.Time
	go func() {
		t := time.NewTicker(c.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				hbMu.Lock()
				waiting, age := pending != "", time.Since(sentAt)
				hbMu.Unlock()
				if waiting && age < c.cfg.Heartbeat/2 {
					continue // the beat in flight went out late, just now: give it a tick
				}
				if waiting {
					hbErr <- fmt.Errorf("heartbeat reply missed (%s after the beat)", age.Round(time.Millisecond))
					conn.Close()
					return
				}
				ref, err := c.write(conn, nil, "phoenix", "heartbeat", map[string]any{})
				if err != nil {
					hbErr <- err
					conn.Close()
					return
				}
				hbMu.Lock()
				if early != ref {
					pending, sentAt = ref, time.Now()
				}
				hbMu.Unlock()
			}
		}
	}()

	var readErr error
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * c.cfg.Heartbeat))
		_, b, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		f, err := DecodeFrame(b)
		if err != nil {
			c.cfg.Logf("suite: bad frame: %v", err)
			continue
		}
		switch {
		case f.Topic == "phoenix" && f.Event == "phx_reply":
			hbMu.Lock()
			switch {
			case f.Ref == nil:
			case *f.Ref == pending:
				pending = ""
			default:
				early = *f.Ref
			}
			hbMu.Unlock()
		case f.Topic != c.Topic():
		case f.Event == "phx_close" || f.Event == "phx_error":
			readErr = fmt.Errorf("channel %s", f.Event)
		case f.Event == "phx_reply":
			// Replies to our pushes; core's play handlers reply noreply.
		default:
			c.h.OnEvent(f.Event, f.Payload)
		}
		if readErr != nil {
			break
		}
	}
	select {
	case e := <-hbErr:
		readErr = e
	default:
	}
	c.mu.Lock()
	wr := c.wr
	c.mu.Unlock()
	if wr.n > 0 {
		c.cfg.Logf("suite: socket dropped after %d writes (%d bytes); the last was %s, %d bytes, written %s before the drop in %s",
			wr.n, wr.bytes, wr.lastEvent, wr.lastBytes, time.Since(wr.lastAt).Round(time.Millisecond), wr.lastTook.Round(time.Microsecond))
	}
	c.h.OnDisconnected(readErr)
	return readErr
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<bad url>"
	}
	u.RawQuery = ""
	return u.String()
}

type scrubbed struct{ msg string }

func (s scrubbed) Error() string { return s.msg }

func scrub(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return scrubbed{strings.ReplaceAll(err.Error(), token, "<token>")}
}
