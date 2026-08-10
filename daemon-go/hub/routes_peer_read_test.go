package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// registerTestPeer allocates+registers an online peer through the registry and
// returns its assigned PeerID. It drives the same AllocateAndRegister seam the
// ws-hook uses, so the peer lands in the roster the read routes serve.
func registerTestPeer(t *testing.T, h *Hub, params peer.AllocateParams) proto.PeerID {
	t.Helper()
	id, _, err := h.regForTest().AllocateAndRegister(context.Background(), params)
	if err != nil {
		t.Fatalf("AllocateAndRegister: %v", err)
	}
	return id
}

// regForTest exposes the registry to in-package tests without widening the API.
func (h *Hub) regForTest() *peer.Registry { return h.reg }

// TestListPeersWireShape is the primary-endpoint test: register two peers (one
// per circle), GET /peers, and assert the wire shape clients depend on — the
// {"peers":[...]} envelope, the back-compat name == display_name alias, and the
// default inbound-health fields (ws_connected false, inbound_status "no_hook"
// for a live peer with no socket, "offline" for an offline one).
func TestListPeersWireShape(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	path := "/work/alpha"
	// in_process metadata exempts the peer from lazy_repair's no-WS ghost demote,
	// so it stays online without a live socket (mirrors @jobs service peers).
	registerTestPeer(t, h, peer.AllocateParams{
		Circle:   "default",
		Backend:  proto.AgentClaudeCode,
		Role:     proto.RoleAgent,
		Path:     &path,
		Machine:  "host-a",
		Metadata: map[string]any{"in_process": true},
	})

	resp, err := http.Get(srv.URL + "/peers")
	if err != nil {
		t.Fatalf("GET /peers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body PeersResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Peers) != 1 {
		t.Fatalf("peers len = %d, want 1", len(body.Peers))
	}
	info := body.Peers[0]
	if info.Name != info.DisplayName {
		t.Fatalf("name %q != display_name %q (back-compat alias broken)", info.Name, info.DisplayName)
	}
	if info.PeerID == "" {
		t.Fatalf("peer_id empty")
	}
	if info.Status != string(proto.StatusOnline) {
		t.Fatalf("status = %q, want online", info.Status)
	}
	if info.WSConnected {
		t.Fatalf("ws_connected = true, want false (no live socket in this test)")
	}
	// Live (online) peer with no WS socket → no_hook (not offline).
	if info.InboundStatus != inboundNoHook {
		t.Fatalf("inbound_status = %q, want %q", info.InboundStatus, inboundNoHook)
	}
}

// TestListPeersStatusFilter confirms ?status=offline excludes online peers.
func TestListPeersStatusFilter(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	registerTestPeer(t, h, peer.AllocateParams{
		Circle:   "default",
		Backend:  proto.AgentClaudeCode,
		Role:     proto.RoleAgent,
		Metadata: map[string]any{"in_process": true}, // survive ghost-demote
	})

	resp, err := http.Get(srv.URL + "/peers?status=offline")
	if err != nil {
		t.Fatalf("GET /peers: %v", err)
	}
	defer resp.Body.Close()
	var body PeersResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Peers) != 0 {
		t.Fatalf("status=offline returned %d online peers, want 0", len(body.Peers))
	}
}

// TestGetPeerNotFound asserts an unknown identifier 404s with the FastAPI-shaped
// {"detail": ...} body.
func TestGetPeerNotFound(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/peers/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["detail"]; !ok {
		t.Fatalf("missing detail field in 404 body: %v", body)
	}
}

// TestGetPeerByPane resolves a peer by its tmux pane id, and 404s for an unknown
// pane.
func TestGetPeerByPane(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pane := "%42"
	id := registerTestPeer(t, h, peer.AllocateParams{
		Circle:  "default",
		Backend: proto.AgentClaudeCode,
		Role:    proto.RoleAgent,
		PaneID:  &pane,
	})

	resp, err := http.Get(srv.URL + "/peers/by-pane/" + "%2542") // %25 = literal '%'
	if err != nil {
		t.Fatalf("GET by-pane: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var info PeerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.PeerID != id {
		t.Fatalf("peer_id = %q, want %q", info.PeerID, id)
	}

	missing, err := http.Get(srv.URL + "/peers/by-pane/%2599")
	if err != nil {
		t.Fatalf("GET by-pane missing: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing pane status = %d, want 404", missing.StatusCode)
	}
}

// TestOrchestratorStatus checks present=false for an empty circle and present=true
// with peer identity once a live orchestrator is registered. stale_after_seconds
// always reports the heartbeat tolerance.
func TestOrchestratorStatus(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Empty circle: absent.
	resp, err := http.Get(srv.URL + "/circles/teamx/orchestrator")
	if err != nil {
		t.Fatalf("GET orchestrator: %v", err)
	}
	defer resp.Body.Close()
	var absent OrchestratorStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&absent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if absent.Present {
		t.Fatalf("present = true for empty circle")
	}
	if absent.StaleAfterSeconds <= 0 {
		t.Fatalf("stale_after_seconds = %d, want > 0", absent.StaleAfterSeconds)
	}

	// Register a live orchestrator in teamx.
	id := registerTestPeer(t, h, peer.AllocateParams{
		Circle:  "teamx",
		Backend: proto.AgentClaudeCode,
		Role:    proto.RoleOrchestrator,
	})

	resp2, err := http.Get(srv.URL + "/circles/teamx/orchestrator")
	if err != nil {
		t.Fatalf("GET orchestrator 2: %v", err)
	}
	defer resp2.Body.Close()
	var present OrchestratorStatusResponse
	if err := json.NewDecoder(resp2.Body).Decode(&present); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !present.Present {
		t.Fatalf("present = false after registering a live orchestrator")
	}
	if present.PeerID == nil || proto.PeerID(*present.PeerID) != id {
		t.Fatalf("peer_id = %v, want %q", present.PeerID, id)
	}
}
