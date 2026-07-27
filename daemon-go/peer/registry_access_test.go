package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// TestCheckAccess_CircleGatingParity verifies the ported peer_registry.check_access
// authorization rules: same-circle is allowed, a cross-circle pair without
// bypass is the fail-loud boundary violation, and bypass (flag OR a
// circle-bypassing role) permits the cross-circle access. Unknown target → error.
func TestCheckAccess_CircleGatingParity(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	// Two same-named peers in different circles plus an orchestrator (bypasses).
	_, alphaName, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/api"), Machine: "m",
		Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	_, betaName, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentClaudeCode, Path: ptr("/work/web"), Machine: "m",
		Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register beta: %v", err)
	}
	_, orchName, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "gamma", Backend: proto.AgentClaudeCode, Path: ptr("/work/orch"), Machine: "m",
		Role: proto.RoleOrchestrator,
	})
	if err != nil {
		t.Fatalf("register orch: %v", err)
	}

	// Cross-circle agent→agent without bypass → boundary violation (non-nil err).
	if _, _, err := r.CheckAccess(ctx, string(alphaName), string(betaName), false, nil); err == nil {
		t.Fatalf("cross-circle agent access: expected boundary error, got nil")
	}

	// Same sender/target circle is allowed (alpha→alpha is trivially same circle).
	if _, _, err := r.CheckAccess(ctx, string(alphaName), string(alphaName), false, nil); err != nil {
		t.Fatalf("same-circle access errored: %v", err)
	}

	// bypassCircle=true permits the cross-circle pair.
	if _, _, err := r.CheckAccess(ctx, string(alphaName), string(betaName), true, nil); err != nil {
		t.Fatalf("bypassCircle cross-circle access errored: %v", err)
	}

	// An orchestrator on the TARGET side bypasses circles by role (no flag).
	if _, to, err := r.CheckAccess(ctx, string(alphaName), string(orchName), false, nil); err != nil || to == nil {
		t.Fatalf("orchestrator target bypass: to=%v err=%v, want (peer,nil)", to, err)
	}

	// Unknown target is a fail-loud error (→ 404), regardless of bypass.
	if _, _, err := r.CheckAccess(ctx, string(alphaName), "no-such-peer", true, nil); err == nil {
		t.Fatalf("unknown target: expected error, got nil")
	}

	// Unknown SENDER is best-effort: from is nil, the call still proceeds (notify
	// semantics). Target is alpha (same circle as nothing → allowed when from nil).
	from, to, err := r.CheckAccess(ctx, "ghost-sender", string(alphaName), false, nil)
	if err != nil {
		t.Fatalf("unknown sender: expected proceed, got err %v", err)
	}
	if from != nil {
		t.Fatalf("unknown sender: from = %v, want nil", from)
	}
	if to == nil {
		t.Fatalf("unknown sender: to = nil, want resolved target")
	}
}

// TestCheckAccess_AmbiguousTargetFailsLoud asserts an unscoped display_name
// matching >1 peer (in different circles, no pane to disambiguate) is a fail-loud
// error rather than a silent guess (issue #136 misroute).
func TestCheckAccess_AmbiguousTargetFailsLoud(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	// Same path+backend in two circles → colliding display_name, both online, no
	// pane → ambiguous.
	_, n1, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m", Role: proto.RoleAgent,
	})
	_, n2, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m", Role: proto.RoleAgent,
	})
	if n1 != n2 {
		t.Fatalf("precondition: expected colliding names, got %q %q", n1, n2)
	}

	if _, _, err := r.CheckAccess(ctx, "", string(n1), true, nil); err == nil {
		t.Fatalf("ambiguous target: expected fail-loud error, got nil")
	}

	// GetPeerByName surfaces the same ambiguity error.
	if _, err := r.GetPeerByName(string(n1), nil); err == nil {
		t.Fatalf("GetPeerByName ambiguous: expected error, got nil")
	}

	// Scoping to a circle disambiguates → no error.
	circle := "alpha"
	if p, err := r.GetPeerByName(string(n1), &circle); err != nil || p == nil || p.Circle != "alpha" {
		t.Fatalf("GetPeerByName scoped: p=%v err=%v, want alpha peer", p, err)
	}
}

// TestResolvePeerStrict_Cardinality covers the 0/1/N branches the destructive
// routes (kill/restart/spawn) switch on.
func TestResolvePeerStrict_Cardinality(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	idA, name, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m", Role: proto.RoleAgent,
	})
	r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m", Role: proto.RoleAgent,
	})

	// 0: no match.
	if got := r.ResolvePeerStrict("nobody", nil); len(got) != 0 {
		t.Fatalf("no-match: len = %d, want 0", len(got))
	}

	// 1: peer_id hit is unambiguous even when the name collides.
	if got := r.ResolvePeerStrict(string(idA), nil); len(got) != 1 || got[0].PeerID != idA {
		t.Fatalf("peer_id hit: got %d candidates, want 1 == %q", len(got), idA)
	}

	// N: ambiguous display_name returns all candidates (caller disambiguates).
	if got := r.ResolvePeerStrict(string(name), nil); len(got) != 2 {
		t.Fatalf("ambiguous name: got %d candidates, want 2", len(got))
	}

	// 1 after circle scoping.
	circle := "alpha"
	if got := r.ResolvePeerStrict(string(name), &circle); len(got) != 1 || got[0].Circle != "alpha" {
		t.Fatalf("circle-scoped: got %d candidates, want 1 in alpha", len(got))
	}
}

// TestUpdateModelAndMetadataByName covers the /session/update write seams: model
// updates live + durable mapping; metadata merges into live state; unknown peer
// is a no-op (found=false), and an ambiguous name fails loud.
func TestUpdateModelAndMetadataByName(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	id, name, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/api"), Machine: "m", Role: proto.RoleAgent,
		Metadata: map[string]any{"k0": "v0"},
	})

	// Model update by name: found + live + mapping carry the new model.
	found, err := r.UpdateModelByName(ctx, string(name), "claude-opus-4")
	if err != nil || !found {
		t.Fatalf("UpdateModelByName: found=%v err=%v", found, err)
	}
	if p, _ := r.GetPeer(id); p.Model == nil || *p.Model != "claude-opus-4" {
		t.Fatalf("model not applied to live peer: %v", p.Model)
	}

	// Metadata merge keeps existing keys and adds new ones.
	found, err = r.UpdateMetadataByName(ctx, string(name), map[string]any{"k1": "v1"})
	if err != nil || !found {
		t.Fatalf("UpdateMetadataByName: found=%v err=%v", found, err)
	}
	p, _ := r.GetPeer(id)
	if p.Metadata["k0"] != "v0" || p.Metadata["k1"] != "v1" {
		t.Fatalf("metadata merge wrong: %v", p.Metadata)
	}

	// Unknown peer is a no-op (found=false, no error).
	if found, err := r.UpdateModelByName(ctx, "ghost", "x"); found || err != nil {
		t.Fatalf("unknown peer model: found=%v err=%v, want (false,nil)", found, err)
	}
}
