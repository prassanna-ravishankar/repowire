package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// TestUnregisterPeer_AmbiguousNameFailsLoud asserts UnregisterPeer's
// display_name branch now resolves via resolvePeerLocked instead of a manual
// first-match scan: an unscoped name matching two peers in different circles
// is a fail-loud error and deletes neither, while scoping to a circle
// disambiguates and deletes exactly that peer.
func TestUnregisterPeer_AmbiguousNameFailsLoud(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	// Same path+backend in two circles → colliding display_name (same pattern as
	// TestCheckAccess_AmbiguousTargetFailsLoud).
	alphaID, n1, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	betaID, n2, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register beta: %v", err)
	}
	if n1 != n2 {
		t.Fatalf("precondition: expected colliding names, got %q %q", n1, n2)
	}

	// Unscoped: ambiguous → fail-loud error, neither peer removed.
	if found, err := r.UnregisterPeer(ctx, string(n1), nil); err == nil {
		t.Fatalf("ambiguous unregister: expected error, got found=%v err=nil", found)
	}
	if _, ok := r.GetPeer(alphaID); !ok {
		t.Fatalf("ambiguous unregister: alpha peer was deleted, want untouched")
	}
	if _, ok := r.GetPeer(betaID); !ok {
		t.Fatalf("ambiguous unregister: beta peer was deleted, want untouched")
	}

	// Scoped to a circle: disambiguates → deletes exactly that peer.
	circle := "alpha"
	found, err := r.UnregisterPeer(ctx, string(n1), &circle)
	if err != nil {
		t.Fatalf("scoped unregister: unexpected error %v", err)
	}
	if !found {
		t.Fatalf("scoped unregister: expected found=true")
	}
	if _, ok := r.GetPeer(alphaID); ok {
		t.Fatalf("scoped unregister: alpha peer still present, want removed")
	}
	if _, ok := r.GetPeer(betaID); !ok {
		t.Fatalf("scoped unregister: beta peer removed, want untouched")
	}
}
