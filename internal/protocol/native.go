package protocol

// Native mode shares the runtime websocket, but has no renderer or encoder.
const FeatureNative = "game_native_host"
const EventNativeStart = "play_native_start"
const EventNativeStop = "play_native_stop"
const EventNativeReconcile = "play_native_reconcile"
const EventNativeStatus = "play_native_status"

type NativeStart struct {
	SessionID       string         `json:"session_id"`
	Mode            string         `json:"mode"`
	Generation      int            `json:"generation"`
	BuildID         string         `json:"build_id"`
	ProtocolVersion int            `json:"protocol_version"`
	Capacity        int            `json:"capacity"`
	IdleTimeoutS    int            `json:"idle_timeout_s"`
	Build           map[string]any `json:"build"`
}
type NativeStatus struct {
	SessionID      string   `json:"session_id"`
	Generation     int      `json:"generation"`
	State          string   `json:"state"`
	Endpoint       string   `json:"endpoint,omitempty"`
	CredentialHash string   `json:"credential_hash,omitempty"`
	Admissions     []string `json:"admissions"`
}
