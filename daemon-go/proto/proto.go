// Package proto defines the authoritative wire contract and identity types for
// the repowire daemon hub. It is a leaf package: it imports nothing from the
// rest of the project. Identity is split into two DISTINCT named types so that
// passing a DisplayName where a PeerID is required is a COMPILE error, which is
// the whole point of the Go port.
package proto

import (
	"encoding/json"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Identity newtypes. PeerID is immutable, daemon-assigned, unique, format
// "repow-{circle}-{uuid8}". DisplayName is human-facing, mutable, NOT unique.
// They are deliberately distinct named types: routing APIs take PeerID, wire
// frames carry DisplayName, and the compiler refuses to confuse them.
// ---------------------------------------------------------------------------

// PeerID is the canonical, daemon-assigned, immutable peer identifier.
type PeerID string

// DisplayName is the human-facing routing name. May collide across peers.
type DisplayName string

func (p PeerID) String() string      { return string(p) }
func (d DisplayName) String() string { return string(d) }

// CircleBoundary selects which tmux container defines the implicit circle.
type CircleBoundary string

const (
	CircleBoundarySession CircleBoundary = "session"
	CircleBoundaryWindow  CircleBoundary = "window"
)

func (b CircleBoundary) Valid() bool {
	return b == CircleBoundarySession || b == CircleBoundaryWindow
}

// TmuxCircle returns the circle implied by tmux evidence. Window IDs are
// server-unique and stable across window renames and index changes.
func TmuxCircle(boundary CircleBoundary, sessionName, windowID string) string {
	switch boundary {
	case CircleBoundarySession:
		return sessionName
	case CircleBoundaryWindow:
		id := strings.TrimPrefix(windowID, "@")
		if id == "" || strings.Trim(id, "0123456789") != "" {
			return ""
		}
		return "window-" + id
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Typed enums. String values MUST match the Python wire contract exactly.
// ---------------------------------------------------------------------------

// PeerStatus is liveness state. Typed enum; not a bare string.
type PeerStatus string

const (
	StatusOnline  PeerStatus = "online"
	StatusBusy    PeerStatus = "busy"
	StatusOffline PeerStatus = "offline"
)

func (s PeerStatus) Valid() bool {
	switch s {
	case StatusOnline, StatusBusy, StatusOffline:
		return true
	}
	return false
}

// PeerRole is the peer's role in the mesh.
type PeerRole string

const (
	RoleAgent        PeerRole = "agent"
	RoleService      PeerRole = "service"
	RoleOrchestrator PeerRole = "orchestrator"
	RoleHuman        PeerRole = "human"
)

func (r PeerRole) Valid() bool {
	switch r {
	case RoleAgent, RoleService, RoleOrchestrator, RoleHuman:
		return true
	}
	return false
}

// BypassesCircles reports whether this role auto-bypasses circle boundaries.
func (r PeerRole) BypassesCircles() bool {
	return r == RoleService || r == RoleOrchestrator || r == RoleHuman
}

// TurnState is per-turn progress, orthogonal to PeerStatus. The empty string
// is the "unknown / nil" case (pre-feature peers, before any hook fired).
type TurnState string

const (
	TurnUnknown          TurnState = ""
	TurnIdle             TurnState = "idle"
	TurnWorking          TurnState = "working"
	TurnAwaitingInput    TurnState = "awaiting_input"
	TurnPendingFirstTurn TurnState = "pending_first_turn"
)

// AgentType is the coding-agent runtime backing a peer.
type AgentType string

const (
	AgentClaudeCode  AgentType = "claude-code"
	AgentOpenCode    AgentType = "opencode"
	AgentCodex       AgentType = "codex"
	AgentGemini      AgentType = "gemini"
	AgentAntigravity AgentType = "antigravity"
	AgentPi          AgentType = "pi"
	AgentMCPHTTP     AgentType = "mcp-http"
)

func (a AgentType) Valid() bool {
	switch a {
	case AgentClaudeCode, AgentOpenCode, AgentCodex, AgentGemini,
		AgentAntigravity, AgentPi, AgentMCPHTTP:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Peer: in-memory peer record. Identity fields use the newtypes.
// ---------------------------------------------------------------------------

// Peer is a member of the mesh. PeerID is identity; DisplayName is addressing.
type Peer struct {
	PeerID      PeerID      `json:"peer_id"`
	DisplayName DisplayName `json:"display_name"`
	Path        string      `json:"path"`
	Machine     string      `json:"machine"`

	TmuxSession *string `json:"tmux_session,omitempty"`
	PaneID      *string `json:"pane_id,omitempty"`

	Backend AgentType `json:"backend"`
	Model   *string   `json:"model,omitempty"`
	Circle  string    `json:"circle"`
	Role    PeerRole  `json:"role"`

	Status    PeerStatus `json:"status"`
	TurnState TurnState  `json:"turn_state,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`

	Metadata    map[string]any `json:"metadata,omitempty"`
	Description string         `json:"description"`
	AgentPID    *int           `json:"agent_pid,omitempty"`
}

// SessionMapping is the durable identity row persisted to peer_session_mappings.
// SessionID column in SQLite is the peer_id, so it is typed PeerID here.
type SessionMapping struct {
	SessionID   PeerID      `json:"session_id"`
	DisplayName DisplayName `json:"display_name"`
	Circle      string      `json:"circle"`
	Backend     AgentType   `json:"backend"`
	Path        *string     `json:"path,omitempty"`
	Role        PeerRole    `json:"role"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Description string      `json:"description"`
	Model       *string     `json:"model,omitempty"`
	AgentPID    *int        `json:"agent_pid,omitempty"`
}

// ---------------------------------------------------------------------------
// Capability advertisement constants (match protocol/capabilities.py).
// ---------------------------------------------------------------------------

const (
	CurrentHookVersion    = 1
	CapDeliveryReceipts   = "delivery_receipts"
	CapRuntimeInbox       = "runtime_inbox"
	CapThreadSteering     = "thread_steering"
	PaneUnsafeStrikeLimit = 3
)

func HasCapability(metadata map[string]any, capability string) bool {
	switch values := metadata["capabilities"].(type) {
	case []string:
		for _, value := range values {
			if value == capability {
				return true
			}
		}
	case []any:
		for _, value := range values {
			if value == capability {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Wire frames. JSON shape is IDENTICAL to the Python daemon. Frames are
// distinguished by their "type" field. ConnectFrame.PeerID is *PeerID so the
// claimed-id-on-reconnect case stays type-safe; DisplayName fields on outbound
// frames carry DisplayName (addressing), never PeerID.
// ---------------------------------------------------------------------------

// FrameType is the discriminator value of the "type" wire field.
type FrameType string

const (
	// Client -> Daemon
	FrameConnect           FrameType = "connect"
	FrameStatus            FrameType = "status"
	FrameUpdateDisplayName FrameType = "update_display_name"
	FramePong              FrameType = "pong"
	FrameDeliveryAck       FrameType = "delivery_ack"
	FrameError             FrameType = "error"
	// Daemon -> Client
	FrameConnected FrameType = "connected"
	FrameAsk       FrameType = "ask"
	FrameNotify    FrameType = "notify"
	FrameBroadcast FrameType = "broadcast"
	FramePing      FrameType = "ping"
)

// Envelope is the minimal pre-parse used to read the "type" field, then
// re-unmarshal into the concrete frame. Use json.RawMessage to defer.
type Envelope struct {
	Type FrameType `json:"type"`
}

// ConnectFrame is the first client->daemon message. Field names match Python.
type ConnectFrame struct {
	Type         FrameType      `json:"type"` // "connect"
	DisplayName  DisplayName    `json:"display_name"`
	Circle       string         `json:"circle"`
	Backend      AgentType      `json:"backend"`
	Path         *string        `json:"path,omitempty"`
	TmuxSession  *string        `json:"tmux_session,omitempty"`
	PaneID       *string        `json:"pane_id,omitempty"`
	CircleSource *string        `json:"circle_source,omitempty"`
	Role         PeerRole       `json:"role"`
	AuthToken    *string        `json:"auth_token,omitempty"`
	HookVersion  *int           `json:"hook_version,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty"`
	Model        *string        `json:"model,omitempty"`
	ModelDetails map[string]any `json:"model_details,omitempty"`
	AgentPID     *int           `json:"agent_pid,omitempty"`
	PeerID       *PeerID        `json:"peer_id,omitempty"` // claimed on reconnect
}

// ConnectedFrame is the daemon's reply. session_id IS the assigned peer_id.
type ConnectedFrame struct {
	Type        FrameType   `json:"type"` // "connected"
	SessionID   PeerID      `json:"session_id"`
	DisplayName DisplayName `json:"display_name"`
}

// StatusFrame reports liveness/turn (client -> daemon).
type StatusFrame struct {
	Type      FrameType  `json:"type"` // "status"
	Status    PeerStatus `json:"status"`
	TurnState *TurnState `json:"turn_state,omitempty"`
}

// UpdateDisplayNameFrame renames a peer (client -> daemon).
type UpdateDisplayNameFrame struct {
	Type        FrameType   `json:"type"` // "update_display_name"
	DisplayName DisplayName `json:"display_name"`
}

// ErrorFrame is bidirectional. Code is set for typed rejections (peer_retired).
type ErrorFrame struct {
	Type          FrameType `json:"type"` // "error"
	Code          string    `json:"code,omitempty"`
	Error         string    `json:"error"`
	CorrelationID *string   `json:"correlation_id,omitempty"`
}

// QueryFrame is daemon -> client; expects a ResponseFrame keyed by CorrelationID.
type QueryFrame struct {
	Type          FrameType   `json:"type"` // "query"
	CorrelationID string      `json:"correlation_id"`
	FromPeer      DisplayName `json:"from_peer"`
	Text          string      `json:"text"`
	DeliveryID    *string     `json:"delivery_id,omitempty"`
}

// MarshalFrame is a tiny helper so implementers serialize with stable shape.
func MarshalFrame(v any) ([]byte, error) { return json.Marshal(v) }
