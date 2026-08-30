package proto

import (
	"encoding/json"
	"testing"
)

func TestConnectFrameRoundTrip(t *testing.T) {
	pid := PeerID("repow-default-abcd1234")
	path := "/work/repo"
	in := ConnectFrame{
		Type:        FrameConnect,
		DisplayName: "alice",
		Circle:      "default",
		Backend:     AgentClaudeCode,
		Path:        &path,
		Role:        RoleAgent,
		PeerID:      &pid,
	}

	raw, err := MarshalFrame(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ft, err := ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if ft != FrameConnect {
		t.Fatalf("type = %q, want %q", ft, FrameConnect)
	}

	var out ConnectFrame
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.DisplayName != in.DisplayName {
		t.Errorf("display_name = %q, want %q", out.DisplayName, in.DisplayName)
	}
	if out.Backend != AgentClaudeCode {
		t.Errorf("backend = %q, want %q", out.Backend, AgentClaudeCode)
	}
	if out.PeerID == nil || *out.PeerID != pid {
		t.Errorf("peer_id = %v, want %q", out.PeerID, pid)
	}
}

func TestWireValuesMatchPython(t *testing.T) {
	cases := map[string]string{
		string(StatusOnline):         "online",
		string(StatusBusy):           "busy",
		string(StatusOffline):        "offline",
		string(RoleAgent):            "agent",
		string(RoleService):          "service",
		string(RoleOrchestrator):     "orchestrator",
		string(RoleHuman):            "human",
		string(AgentClaudeCode):      "claude-code",
		string(AgentOpenCode):        "opencode",
		string(AgentCodex):           "codex",
		string(AgentPi):              "pi",
		string(AgentMCPHTTP):         "mcp-http",
		string(TurnIdle):             "idle",
		string(TurnWorking):          "working",
		string(TurnAwaitingInput):    "awaiting_input",
		string(TurnPendingFirstTurn): "pending_first_turn",
		string(TurnUnknown):          "",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("wire value = %q, want %q", got, want)
		}
	}
}

func TestTmuxCircle(t *testing.T) {
	if got := TmuxCircle(CircleBoundarySession, "mesh", "@7"); got != "mesh" {
		t.Fatalf("session circle = %q", got)
	}
	if got := TmuxCircle(CircleBoundaryWindow, "mesh", "@7"); got != "window-7" {
		t.Fatalf("window circle = %q", got)
	}
	for _, got := range []string{
		TmuxCircle(CircleBoundarySession, "", "@7"),
		TmuxCircle(CircleBoundaryWindow, "mesh", ""),
		TmuxCircle(CircleBoundaryWindow, "mesh", "not-an-id"),
		TmuxCircle("other", "mesh", "@7"),
	} {
		if got != "" {
			t.Fatalf("missing/invalid evidence produced %q", got)
		}
	}
}

func TestStatusFrameOmitsNilTurnState(t *testing.T) {
	raw, err := MarshalFrame(StatusFrame{Type: FrameStatus, Status: StatusOnline})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["turn_state"]; ok {
		t.Errorf("turn_state should be omitted when nil, got %v", string(raw))
	}
}
