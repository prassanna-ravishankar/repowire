package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// Closing a peer that a terminal offline already soft-retired must still make
// the retirement hard; otherwise a live pid or a certified runtime reclaims a
// killed peer's id (#530/#535).
func TestClosePeerHardensAnExistingSoftRetirement(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)
	pid := 4343
	r.live = fakeLive{alive: map[int]bool{pid: true}}
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "alpha", Backend: proto.AgentCodex,
		Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent, AgentPID: &pid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkOffline(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClosePeer(ctx, id, "operator_close"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	hard := store.retired[id].Hard
	store.mu.Unlock()
	if !hard || !r.retired[id].Hard {
		t.Fatal("ClosePeer on a soft-retired peer left the retirement soft")
	}
	claimed := id
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "alpha", Backend: proto.AgentCodex, Path: ptr("/work/x"),
		Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: &claimed, RuntimeIdentityVerified: true}); err != ErrPeerClosed {
		t.Fatalf("certified reclaim of a closed peer = %v, want ErrPeerClosed", err)
	}
}
