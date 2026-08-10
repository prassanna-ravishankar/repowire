package hub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// TestClaimRoleEndpoint is the primary handler test: POST /peers/claim-role
// promotes an existing live peer to orchestrator and returns the Python wire
// shape. The registry must reflect the new role keyed by peer_id, and the
// circle's orchestrator lookup must then find it.
func TestClaimRoleEndpoint(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	path := "/work/myproj"
	reg := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name:    "myproj-claude-code",
		Path:    &path,
		Backend: proto.AgentClaudeCode,
		Circle:  strptr("default"),
	})
	if reg.Code != http.StatusOK {
		t.Fatalf("setup register: want 200, got %d (%s)", reg.Code, reg.Body.String())
	}
	var regResp RegisterResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &regResp); err != nil {
		t.Fatalf("decode register: %v", err)
	}

	rec := postLifecycleJSON(t, mux, "/peers/claim-role", ClaimRoleRequest{
		PeerName: "myproj-claude-code",
		Role:     proto.RoleOrchestrator,
		Circle:   strptr("default"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("claim-role: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp ClaimRoleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ClaimRoleResponse: %v", err)
	}
	if resp.PeerID != regResp.PeerID {
		t.Fatalf("peer_id: want %q, got %q", regResp.PeerID, resp.PeerID)
	}
	if resp.PeerName != "myproj-claude-code" {
		t.Fatalf("peer_name: want myproj-claude-code, got %q", resp.PeerName)
	}
	if resp.Role != proto.RoleOrchestrator {
		t.Fatalf("role: want orchestrator, got %q", resp.Role)
	}
	if resp.Circle != "default" {
		t.Fatalf("circle: want default, got %q", resp.Circle)
	}
	if resp.AlreadyHeld {
		t.Fatalf("already_held must be false on a fresh claim")
	}
	if resp.PreviousHolders == nil {
		t.Fatalf("previous_holders must serialize as [] not null")
	}

	p, ok := h.reg.GetPeer(proto.PeerID(regResp.PeerID))
	if !ok || p.Role != proto.RoleOrchestrator {
		t.Fatalf("registry must reflect orchestrator role on the claimed peer")
	}
	if orch, ok := h.reg.GetOrchestrator("default"); !ok || orch.PeerID != p.PeerID {
		t.Fatalf("GetOrchestrator must return the claimed peer")
	}
}

// TestClaimRoleRejectsNonOrchestrator: only orchestrator is claimable; any other
// role is a 400 before the registry is touched.
func TestClaimRoleRejectsNonOrchestrator(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := postLifecycleJSON(t, mux, "/peers/claim-role", ClaimRoleRequest{
		PeerName: "whoever",
		Role:     proto.RoleAgent,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-orchestrator role: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestClaimRoleUnknownPeer: claiming for a peer that does not exist is a 404.
func TestClaimRoleUnknownPeer(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := postLifecycleJSON(t, mux, "/peers/claim-role", ClaimRoleRequest{
		PeerName: "ghost",
		Role:     proto.RoleOrchestrator,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown peer: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}
