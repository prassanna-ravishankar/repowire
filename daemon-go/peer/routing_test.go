package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// description_set_at dates a peer's self-claim. It moves only when the
// description changes and survives a daemon restart via the durable mapping.
func TestDescriptionSetAtPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)
	id, name, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.DescriptionSetAt(id) != nil {
		t.Fatal("a fresh peer has not described itself")
	}
	if _, err := r.UpdateDescription(ctx, string(name), "parsing", nil); err != nil {
		t.Fatal(err)
	}
	first := r.DescriptionSetAt(id)
	if first == nil {
		t.Fatal("UpdateDescription must stamp description_set_at")
	}
	if _, err := r.UpdateDescription(ctx, string(name), "parsing", nil); err != nil {
		t.Fatal(err)
	}
	if again := r.DescriptionSetAt(id); !again.Equal(*first) {
		t.Fatal("an unchanged description must not refresh its age")
	}
	r.persistMappings(ctx)
	if store.mappings[id].DescriptionSetAt == nil {
		t.Fatal("description_set_at not persisted")
	}

	restarted, err := NewRegistry(ctx, store, fakeLive{alive: map[int]bool{}}, fakeTransport{connected: map[proto.PeerID]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.DescriptionSetAt(id); got == nil || !got.Equal(*first) {
		t.Fatalf("after restart description_set_at = %v, want %v", got, first)
	}
	if _, _, err := restarted.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent, ClaimedPeerID: &id,
	}); err != nil {
		t.Fatal(err)
	}
	if got := restarted.DescriptionSetAt(id); got == nil || !got.Equal(*first) {
		t.Fatalf("reclaim lost description_set_at: %v", got)
	}
}
