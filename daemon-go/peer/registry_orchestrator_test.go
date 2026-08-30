package peer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestGetOrchestratorConnectedTransportOutlivesHeartbeat(t *testing.T) {
	transport := fakeTransport{connected: map[proto.PeerID]bool{}}
	r, err := NewRegistry(context.Background(), newMemStore(), fakeLive{alive: map[int]bool{}}, transport)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	id, _, err := r.AllocateAndRegister(context.Background(), AllocateParams{
		Circle: "0", Backend: proto.AgentClaudeCode, Role: proto.RoleOrchestrator,
	})
	if err != nil {
		t.Fatalf("register orchestrator: %v", err)
	}

	stale := time.Now().UTC().Add(-2 * r.HeartbeatTolerance())
	r.mu.Lock()
	r.peers[id].peer.LastSeen = &stale
	r.mu.Unlock()

	if _, ok := r.GetOrchestrator("0"); ok {
		t.Fatal("stale disconnected orchestrator reported present")
	}

	transport.connected[id] = true
	got, ok := r.GetOrchestrator("0")
	if !ok || got.PeerID != id {
		t.Fatalf("connected orchestrator = (%v, %v), want peer_id %q", got, ok, id)
	}
}

func TestClaimRoleCannotDemoteConnectedOrchestratorWithStaleHeartbeat(t *testing.T) {
	transport := fakeTransport{connected: map[proto.PeerID]bool{}}
	r, err := NewRegistry(context.Background(), newMemStore(), fakeLive{alive: map[int]bool{}}, transport)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	holderID, _, err := r.AllocateAndRegister(context.Background(), AllocateParams{
		Circle: "0", Backend: proto.AgentClaudeCode, Role: proto.RoleOrchestrator,
	})
	if err != nil {
		t.Fatalf("register holder: %v", err)
	}
	candidateID, _, err := r.AllocateAndRegister(context.Background(), AllocateParams{
		Circle: "0", Backend: proto.AgentCodex, Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register candidate: %v", err)
	}

	stale := time.Now().UTC().Add(-2 * r.HeartbeatTolerance())
	r.mu.Lock()
	r.peers[holderID].peer.LastSeen = &stale
	r.mu.Unlock()
	transport.connected[holderID] = true

	_, err = r.ClaimSpecialRole(context.Background(), string(candidateID), proto.RoleOrchestrator, nil, true)
	var conflict *RoleClaimConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("ClaimSpecialRole error = %v, want RoleClaimConflictError", err)
	}
}
