package peer

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// TestScheduleRedelivery_FiresOnFreshRegister_NoACPFlag is the regression for the
// redelivery wiring bug: stashed-reply redelivery was gated on the ACP experiment
// flag (off in production) and never scheduled from AllocateAndRegister, so owed
// replies never drained on SessionStart. It must now fire on a fresh registration
// (pass-2 identity-tuple rebind) regardless of the ACP flag.
func TestScheduleRedelivery_FiresOnFreshRegister_NoACPFlag(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	asks := newFakeAsks()
	delivery := &fakeDelivery{}
	r.WithReconciliation(asks, delivery, fakeProbe{}, ExperimentsConfig{}, 0, 0) // ACP OFF

	reply := "owed-answer"
	asks.add(StashedAsk{
		CorrelationID: "ask-orphan", FromPeerID: "repow-alpha-deadid", FromPeerName: "asker",
		ToPeerID: "repow-alpha-answerer", ToPeerName: "answerer", PendingReply: &reply,
		AskerIdentity: &AskerIdentity{
			DisplayName: "proj-claude-code", Circle: "alpha", Backend: proto.AgentClaudeCode,
			Path: normalizeIdentityPath("/work/proj"), Machine: "host1",
		},
	})

	// Reborn asker registers under a fresh id; scheduleRedelivery must rebind+deliver.
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/proj"), Machine: "host1", Role: proto.RoleAgent,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		delivery.mu.Lock()
		n := len(delivery.calls)
		delivery.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("redelivery did not fire on fresh register with ACP flag off")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTurnState_AppliedOnFreshAndReconnect is the regression for turn_state being
// dropped: AllocateParams.TurnState must reach the peer on both a fresh register
// and a same-id reconnect (so pending_first_turn from hooks survives).
func TestTurnState_AppliedOnFreshAndReconnect(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	working := proto.TurnState("working")

	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "ops", Backend: proto.AgentClaudeCode, Path: ptr("/w/x"), Machine: "m",
		Role: proto.RoleAgent, TurnState: &working,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if p, _ := r.GetPeer(id); p.TurnState != working {
		t.Fatalf("fresh turn_state = %q, want working", p.TurnState)
	}

	idle := proto.TurnState("idle")
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &id, Backend: proto.AgentClaudeCode, Path: ptr("/w/x"), Machine: "m",
		TurnState: &idle,
	}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if p, _ := r.GetPeer(id); p.TurnState != idle {
		t.Fatalf("reconnect turn_state = %q, want idle", p.TurnState)
	}
}

// TestReclaim_MappingWinsOverCallerDefaults is the regression for a fresh reclaim
// (daemon restart / eviction) silently demoting role, moving circle, or dropping
// description: when a persisted mapping exists for the reclaimed id it is the
// durable source of truth and wins over the caller's per-transport defaults.
func TestReclaim_MappingWinsOverCallerDefaults(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	idA, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "ops", Backend: proto.AgentClaudeCode, Path: ptr("/w/x"), Machine: "m",
		Role: proto.RoleOrchestrator,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r.mappings[idA].Description = "long running task"
	// Simulate eviction: the durable mapping outlives the live peer record.
	delete(r.peers, idA)

	// Reborn ws-hook reclaims the id with caller DEFAULTS (agent / global).
	idB, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &idA, Circle: "global", Backend: proto.AgentClaudeCode, Path: ptr("/w/x"),
		Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if idB != idA {
		t.Fatalf("reclaim minted %s, want reuse of %s", idB, idA)
	}
	p, _ := r.GetPeer(idA)
	if p.Role != proto.RoleOrchestrator {
		t.Fatalf("reclaim demoted role to %q, want orchestrator", p.Role)
	}
	if p.Circle != "ops" {
		t.Fatalf("reclaim moved circle to %q, want ops", p.Circle)
	}
	if p.Description != "long running task" {
		t.Fatalf("reclaim dropped description: %q", p.Description)
	}
}
