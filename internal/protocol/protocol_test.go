package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Startup-Suite/play-host/internal/media"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// startJSON is shaped like Protocol.session_start_payload/1 in core, plus an
// unknown key the host must ignore.
const startJSON = `{
  "session_id": "01a0db5f-0000-7000-8000-000000000001",
  "task_id": "01a0db5f-4f1a-7001-87a3-e777ad1cab82",
  "repo_url": "https://github.com/example-owner/example-game",
  "branch": "task/01a0db5f-4f1a-7001-87a3-e777ad1cab82",
  "sha": "` + sha + `",
  "encoder": {"preset":"p1","tune":"ll","bitrate_kbps":8000,"fps":60,"width":1280,"height":720,"gop_frames":120},
  "ice_servers": [{"urls":"stun:stun.example:3478"},{"urls":["turn:t.example:3479"],"username":"u","credential":"c"}],
  "idle_timeout_s": 600,
  "slot_hint": 3
}`

func TestDecodeSessionStart(t *testing.T) {
	s, err := DecodeSessionStart([]byte(startJSON))
	if err != nil {
		t.Fatal(err)
	}
	if s.SHA != sha || s.Branch == "" || s.RepoURL == "" || s.IdleTimeoutS != 600 {
		t.Fatalf("decoded %+v", s)
	}
	if s.Encoder.EncoderKind() != media.EncoderAuto || s.Encoder.Preset != "p1" || s.Encoder.GOPFrames != 120 || s.Encoder.Width != 1280 {
		t.Fatalf("encoder %+v", s.Encoder)
	}
	if len(s.ICEServers) != 2 || s.ICEServers[0].URLs[0] != "stun:stun.example:3478" || s.ICEServers[1].Username != "u" {
		t.Fatalf("ice %+v", s.ICEServers)
	}
}

func TestDecodeSessionStartEncoderKindIsAdditive(t *testing.T) {
	b := strings.Replace(startJSON, `"encoder": {`, `"encoder": {"encoder":"videotoolbox",`, 1)
	s, err := DecodeSessionStart([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if s.Encoder.EncoderKind() != media.EncoderVideoToolbox {
		t.Fatalf("encoder kind = %q", s.Encoder.EncoderKind())
	}
}

func TestDecodeSessionStartDefaultsAndRejects(t *testing.T) {
	s, err := DecodeSessionStart([]byte(`{"session_id":"s","repo_url":"r","branch":"b","sha":"` + sha + `","encoder":{},"ice_servers":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.IdleTimeoutS != DefaultIdleTimeoutS || s.Encoder.Preset != "p1" {
		t.Fatalf("defaults not applied: %+v", s)
	}
	bad := []string{
		`{"repo_url":"r","sha":"` + sha + `"}`,                                          // no session
		`{"session_id":"s","repo_url":"r","sha":"` + strings.ToUpper(sha) + `"}`,        // uppercase sha
		`{"session_id":"s","repo_url":"r","sha":"abc"}`,                                 // short sha
		`{"session_id":"s","sha":"` + sha + `"}`,                                        // no repo
		`{"session_id":"s","repo_url":"r","sha":"` + sha + `","encoder":{"tune":"hq"}}`, // bad arm
		`not json`,
	}
	for _, b := range bad {
		if _, err := DecodeSessionStart([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

func TestSignalData(t *testing.T) {
	off, err := NewOfferSignal("s", "", "v=0\r\n")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(off)
	if got := string(b); got != `{"session_id":"s","kind":"offer","data":{"type":"offer","sdp":"v=0\r\n"}}` {
		t.Fatalf("offer wire %s", got)
	}

	ans := Signal{Kind: KindAnswer, Data: json.RawMessage(`{"type":"answer","sdp":"v=0"}`)}
	if d, err := ans.Description(); err != nil || d.SDP != "v=0" || d.Type != "answer" {
		t.Fatalf("answer %+v %v", d, err)
	}
	bare := Signal{Kind: KindAnswer, Data: json.RawMessage(`"v=0"`)}
	if d, err := bare.Description(); err != nil || d.SDP != "v=0" {
		t.Fatalf("bare answer %+v %v", d, err)
	}
	if _, err := (Signal{Kind: KindAnswer, Data: json.RawMessage(`{}`)}).Description(); err == nil {
		t.Fatal("empty answer accepted")
	}

	ice := Signal{Kind: KindICE, Data: json.RawMessage(`{"candidate":"candidate:1 1 udp 1 1.2.3.4 5 typ host","sdpMid":"0","sdpMLineIndex":0}`)}
	c, ok, err := ice.Candidate()
	if err != nil || !ok || *c.SDPMid != "0" || *c.SDPMLineIndex != 0 {
		t.Fatalf("ice %+v %v %v", c, ok, err)
	}
	for _, end := range []string{`null`, `{"candidate":""}`, ``} {
		if _, ok, err := (Signal{Kind: KindICE, Data: json.RawMessage(end)}).Candidate(); ok || err != nil {
			t.Errorf("end-of-candidates %q read as ok=%v err=%v", end, ok, err)
		}
	}
	if _, err := NewOfferSignal("s", "", strings.Repeat("a", MaxSignalBytes)); err == nil {
		t.Fatal("oversized offer accepted")
	}
}

func TestStatus(t *testing.T) {
	st := NewStatus("s", StateBuilding, strings.Repeat("é", 300), -5)
	if len(st.Detail) > MaxDetailBytes || !strings.HasSuffix(st.Detail, "é") {
		t.Fatalf("detail %d bytes, split rune?", len(st.Detail))
	}
	if st.ElapsedMs != 0 {
		t.Fatalf("elapsed %d", st.ElapsedMs)
	}
	b, _ := json.Marshal(NewStatus("s", StateLive, "ok", 12))
	if string(b) != `{"session_id":"s","state":"live","detail":"ok","elapsed_ms":12}` {
		t.Fatalf("status wire %s", b)
	}
	for st, want := range map[string]bool{StateEnded: true, StateFailed: true, StateLive: false, StateBuilding: false, StateConnecting: false} {
		if Terminal(st) != want {
			t.Errorf("Terminal(%s)", st)
		}
	}
}

// Task 01a0dbd6: the widened frames, in the shapes core's
// Protocol.peer_open_payload/2, slots_payload/3 and parse_slot_activity/1
// produce and accept (core protocol.ex, "Widening").
func TestMultiPeerFrames(t *testing.T) {
	off, err := NewOfferSignal("s", "01a0e9b0-0000-7000-8000-00000000000a", "v=0")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(off)
	if got := string(b); got != `{"session_id":"s","kind":"offer","data":{"type":"offer","sdp":"v=0"},"peer_id":"01a0e9b0-0000-7000-8000-00000000000a"}` {
		t.Fatalf("peer-tagged offer wire %s", got)
	}

	var ans Signal
	if err := json.Unmarshal([]byte(`{"session_id":"s","kind":"answer","data":"v=0","peer_id":"p1"}`), &ans); err != nil || ans.PeerID != "p1" {
		t.Fatalf("peer-tagged answer %+v %v", ans, err)
	}
	var v1 Signal
	if err := json.Unmarshal([]byte(`{"session_id":"s","kind":"answer","data":"v=0","peer_id":null}`), &v1); err != nil || v1.PeerID != "" {
		t.Fatalf("null peer_id must read as the v1 peer: %+v %v", v1, err)
	}

	var open PeerRef
	if err := json.Unmarshal([]byte(`{"session_id":"s","peer_id":"p1"}`), &open); err != nil || open.PeerID != "p1" || open.SessionID != "s" {
		t.Fatalf("peer_open %+v %v", open, err)
	}

	var sl Slots
	raw := `{"session_id":"s","max_players":4,"peers":{"p1":{"0":0,"1":2},"spec":{},"bad":{"x":1,"-1":1,"3":-1}},"unknown":true}`
	if err := json.Unmarshal([]byte(raw), &sl); err != nil {
		t.Fatal(err)
	}
	tab := sl.Table()
	if sl.MaxPlayers != 4 || len(tab) != 3 || tab["p1"][0] != 0 || tab["p1"][1] != 2 || len(tab["p1"]) != 2 {
		t.Fatalf("table %v", tab)
	}
	if len(tab["spec"]) != 0 || len(tab["bad"]) != 0 {
		t.Fatalf("a spectator or a malformed entry bound a slot: %v", tab)
	}

	b, _ = json.Marshal(SlotActivity{SessionID: "s", Slots: []int{0, 2}})
	if string(b) != `{"session_id":"s","slots":[0,2]}` {
		t.Fatalf("slot activity wire %s", b)
	}
}
