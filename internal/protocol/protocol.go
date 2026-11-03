// Package protocol is the host side of core's Platform.GameStream.Protocol
// (apps/platform/lib/platform/game_stream/protocol.ex, task 01a0db5f stage 2).
// That moduledoc is the contract; this package mirrors it and nothing more.
//
// Core -> host (broadcasts on runtime:<runtime_id>):
//
//	play_session_start  {session_id, task_id?, repo_url, branch, sha, encoder, ice_servers, idle_timeout_s}
//	play_session_stop   {session_id, reason}
//	play_signal         {session_id, kind: answer|ice, data}
//
// Host -> core (pushes on the same topic):
//
//	play_session_status {session_id, state: building|connecting|live|ended|failed, detail, elapsed_ms}
//	play_signal         {session_id, kind: offer|ice, data}
//
// Unknown keys are ignored in both directions.
//
// Widening (task 01a0dbd6, core protocol.ex "Widening: peers, player slots
// and spectators"). A host that declares client_info.features
// "game_stream_multi" (internal/suite) is sent, and sends:
//
//	core -> host  play_signal        + optional peer_id (the peer that answered)
//	core -> host  play_peer_open     {session_id, peer_id}   create or REPLACE, then offer
//	core -> host  play_peer_close    {session_id, peer_id}   close, release its slots
//	core -> host  play_slots         {session_id, max_players, peers: {peer_id: {src: slot}}}
//	host -> core  play_signal        + peer_id on every offer/ice
//	host -> core  play_slot_activity {session_id, slots: [slot]}  at most 4 per second
//
// A peer is one WebRTC connection (one LiveView); a peer with no slots is a
// spectator. play_slots is a FULL snapshot; JSON carries src and the peer map
// keys as strings. An old core sends none of these and no peer_id, and the
// host keeps v1: one implicit peer whose src 0 is slot 0.
//
// Resume (task 01a0dbd6 stage 6). A session outlives a dropped suite
// socket: media is browser <-> host and never passes through core, so the
// viewers keep watching while the socket reconnects. After the rejoin the
// host sends ONE
//
//	host -> core  play_session_status + resume: true, peers: [peer_id]
//
// with its current state and the peers it still holds. Core keeps the
// session for its grace window after the channel drops; the resume status
// cancels that, and core re-sends play_peer_open for each peer it holds that
// the host does not, play_peer_close for each the host holds that it does
// not, and a fresh play_slots. An old core ignores the two keys and has
// already failed the session, so the host's grace just runs out.
//
// Touch (task 01a0fe45, core protocol.ex "Touch"). The host declares
// client_info.features "game_stream_touch" (internal/suite FeatureTouch);
// core turns the browser's touch surface on only for a host that does. The
// frames themselves never pass through core:
//
//	browser -> host  input-events  {"t":"touch","src":0,"id":0..9,"ph":"down|move|up|cancel","x":0..1,"y":0..1}
//	host -> addon    link          {"t":"st","d":slot,"i":slot*10+id,"p":bool,"c":bool,"x":..,"y":..}
//	host -> addon    link          {"t":"sd","d":slot,"i":slot*10+id,"x":..,"y":..}
//
// The slot comes from play_slots, never the message; internal/input
// validates, rate-bounds and releases them.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/Startup-Suite/play-host/internal/media"
)

// Event names on the runtime channel.
const (
	EventSessionStart  = "play_session_start"
	EventSessionStop   = "play_session_stop"
	EventSignal        = "play_signal"
	EventSessionStatus = "play_session_status"
	EventPeerOpen      = "play_peer_open"
	EventPeerClose     = "play_peer_close"
	EventSlots         = "play_slots"
	EventSlotActivity  = "play_slot_activity"
	EventPeerView      = "play_peer_view"
)

// Session states, in order. Ended and failed are terminal.
const (
	StateBuilding   = "building"
	StateConnecting = "connecting"
	StateLive       = "live"
	StateEnded      = "ended"
	StateFailed     = "failed"
)

// Signal kinds.
const (
	KindOffer  = "offer"  // host -> viewer
	KindAnswer = "answer" // viewer -> host
	KindICE    = "ice"    // both
)

// MaxDetailBytes is core's @max_detail_bytes. Core truncates rather than
// rejects; the host truncates first so what it logs is what core shows.
const MaxDetailBytes = 500

// MaxSignalBytes is core's @max_signal_bytes on the JSON-encoded data.
const MaxSignalBytes = 64 * 1024

// DefaultIdleTimeoutS is core's @default_idle_timeout_s.
const DefaultIdleTimeoutS = 600

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ICEServer is an RTCIceServer-shaped map. urls may be a string or a list.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// UnmarshalJSON accepts "urls" as either a string or a list of strings.
func (s *ICEServer) UnmarshalJSON(b []byte) error {
	var raw struct {
		URLs       json.RawMessage `json:"urls"`
		Username   string          `json:"username"`
		Credential string          `json:"credential"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Username, s.Credential = raw.Username, raw.Credential
	var one string
	if err := json.Unmarshal(raw.URLs, &one); err == nil {
		s.URLs = []string{one}
		return nil
	}
	return json.Unmarshal(raw.URLs, &s.URLs)
}

// SessionStart is play_session_start.
type SessionStart struct {
	SessionID    string       `json:"session_id"`
	TaskID       string       `json:"task_id,omitempty"`
	RepoURL      string       `json:"repo_url"`
	Branch       string       `json:"branch"`
	SHA          string       `json:"sha"`
	Encoder      media.Preset `json:"encoder"`
	ICEServers   []ICEServer  `json:"ice_servers"`
	IdleTimeoutS int          `json:"idle_timeout_s"`
}

// DecodeSessionStart parses and validates a start payload. Encoder fields
// core leaves out keep the stage-1 control values.
func DecodeSessionStart(b []byte) (SessionStart, error) {
	s := SessionStart{Encoder: media.ControlPreset()}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("play_session_start: %w", err)
	}
	if s.SessionID == "" {
		return s, errors.New("play_session_start: missing session_id")
	}
	if !shaRe.MatchString(s.SHA) {
		return s, fmt.Errorf("play_session_start: sha %q is not 40 lowercase hex", s.SHA)
	}
	if s.RepoURL == "" {
		return s, errors.New("play_session_start: missing repo_url")
	}
	if s.IdleTimeoutS <= 0 {
		s.IdleTimeoutS = DefaultIdleTimeoutS
	}
	if err := s.Encoder.Validate(); err != nil {
		return s, fmt.Errorf("play_session_start: encoder: %w", err)
	}
	return s, nil
}

// SessionStop is play_session_stop.
type SessionStop struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// Signal is play_signal in either direction. Data is relayed opaquely by
// core; its shape is agreed between host and viewer (see OfferData,
// AnswerData, ICEData).
type Signal struct {
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	// PeerID names the peer (multi-peer core); empty is the v1 single peer,
	// and is then left off the wire.
	PeerID string `json:"peer_id,omitempty"`
}

// PeerRef is play_peer_open and play_peer_close.
type PeerRef struct {
	SessionID string `json:"session_id"`
	PeerID    string `json:"peer_id"`
}

// Slots is play_slots: a full, idempotent snapshot of every attached peer
// (spectators with an empty map). Peers is peer_id -> src -> slot, with src
// a string on the wire (a JSON object key).
type Slots struct {
	SessionID  string                    `json:"session_id"`
	MaxPlayers int                       `json:"max_players"`
	Peers      map[string]map[string]int `json:"peers"`
}

// Table converts the snapshot to peer_id -> src -> slot. An entry whose src
// is not a non-negative integer, or whose slot is negative, is dropped: the
// table is the only thing input is bound by, so a malformed entry must bind
// nothing rather than guess.
func (s Slots) Table() map[string]map[int]int {
	out := make(map[string]map[int]int, len(s.Peers))
	for peer, m := range s.Peers {
		t := make(map[int]int, len(m))
		for k, slot := range m {
			src, err := strconv.Atoi(k)
			if err != nil || src < 0 || slot < 0 {
				continue
			}
			t[src] = slot
		}
		out[peer] = t
	}
	return out
}

// SlotActivity is play_slot_activity.
type SlotActivity struct {
	SessionID string `json:"session_id"`
	Slots     []int  `json:"slots"`
}

// SessionDescription is the data of an offer or answer: an
// RTCSessionDescriptionInit.
type SessionDescription struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// ICECandidate is the data of an ice signal: an RTCIceCandidateInit. A null
// data (or an empty candidate) is end-of-candidates.
type ICECandidate struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// Description reads an offer/answer's data. A bare JSON string is taken as
// the SDP itself.
func (s Signal) Description() (SessionDescription, error) {
	var d SessionDescription
	var bare string
	if json.Unmarshal(s.Data, &bare) == nil && bare != "" {
		return SessionDescription{Type: s.Kind, SDP: bare}, nil
	}
	if err := json.Unmarshal(s.Data, &d); err != nil {
		return d, fmt.Errorf("%s data: %w", s.Kind, err)
	}
	if d.SDP == "" {
		return d, fmt.Errorf("%s data has no sdp", s.Kind)
	}
	if d.Type == "" {
		d.Type = s.Kind
	}
	return d, nil
}

// Candidate reads an ice signal's data. ok is false for end-of-candidates.
func (s Signal) Candidate() (c ICECandidate, ok bool, err error) {
	if len(s.Data) == 0 || string(s.Data) == "null" {
		return c, false, nil
	}
	if err := json.Unmarshal(s.Data, &c); err != nil {
		return c, false, fmt.Errorf("ice data: %w", err)
	}
	return c, c.Candidate != "", nil
}

// NewOfferSignal builds the host's offer. peerID is empty for the v1 peer.
func NewOfferSignal(sessionID, peerID, sdp string) (Signal, error) {
	data, err := json.Marshal(SessionDescription{Type: KindOffer, SDP: sdp})
	if err != nil {
		return Signal{}, err
	}
	if len(data) > MaxSignalBytes {
		return Signal{}, fmt.Errorf("offer is %d bytes, over core's %d cap", len(data), MaxSignalBytes)
	}
	return Signal{SessionID: sessionID, Kind: KindOffer, Data: data, PeerID: peerID}, nil
}

// Status is play_session_status.
type Status struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// ResumeStatus is the one status a host sends after the suite socket
// rejoins while a session is running (see "Resume"). Peers is never null on
// the wire: an empty list means the host holds no peer.
type ResumeStatus struct {
	Status
	Resume bool     `json:"resume"`
	Peers  []string `json:"peers"`
}

// NewResumeStatus builds a ResumeStatus; peers nil becomes [].
func NewResumeStatus(st Status, peers []string) ResumeStatus {
	if peers == nil {
		peers = []string{}
	}
	return ResumeStatus{Status: st, Resume: true, Peers: peers}
}

// NewStatus builds a status, truncating detail to core's cap on a rune
// boundary and clamping elapsed to >= 0 (core rejects a negative one).
func NewStatus(sessionID, state, detail string, elapsedMs int64) Status {
	return Status{SessionID: sessionID, State: state, Detail: TruncateDetail(detail), ElapsedMs: max(elapsedMs, 0)}
}

// Terminal reports whether state is ended or failed.
func Terminal(state string) bool { return state == StateEnded || state == StateFailed }

// TruncateDetail cuts d to at most MaxDetailBytes without splitting a rune.
func TruncateDetail(d string) string {
	if len(d) <= MaxDetailBytes {
		return d
	}
	cut := MaxDetailBytes
	for cut > 0 && !utf8.RuneStart(d[cut]) {
		cut--
	}
	return d[:cut]
}

// Peer view kinds for play_peer_view.
const (
	ViewSlot   = "slot"
	ViewShared = "shared"
)

// MaxViewSlot is the highest slot core accepts in a play_peer_view.
const MaxViewSlot = 31

// PeerView is play_peer_view (task 01a1a34c): which encode a peer's
// connection carries. Slot is omitted when View is "shared". Sent only by a
// host that declares game_stream_per_slot, never for the v1 peer (id "").
type PeerView struct {
	SessionID string `json:"session_id"`
	PeerID    string `json:"peer_id"`
	View      string `json:"view"`
	Slot      *int   `json:"slot,omitempty"`
}
