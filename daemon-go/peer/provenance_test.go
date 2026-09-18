package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// Registration without provenance keeps what the daemon knows; only an unknown
// source is inferred from evidence. An explicit classification wins on both
// fresh mint and same-id reconnect, and the demotion survives a reconnect
// that omits provenance.
func TestAllocatePreservesProvenanceAcrossReconnect(t *testing.T) {
	r, _ := newRegistry(t)
	ctx := context.Background()
	path := "/p/app"
	denied := proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "parent-thread", Ephemeral: true, Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, Provenance: &denied,
		Metadata: map[string]any{"transport": "codex-app-server"}})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := r.GetPeer(id); p.Provenance != denied {
		t.Fatalf("fresh mint provenance = %+v", p.Provenance)
	}
	// Reconnect (ConnectFrame) carries no provenance: nothing is lost.
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, ClaimedPeerID: &id}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.GetPeer(id); p.Provenance != denied {
		t.Fatalf("reconnect without provenance changed it to %+v", p.Provenance)
	}
	// A fresh verdict wins outright.
	fresh := proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "parent-thread", Ephemeral: true, Addressable: true}
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, ClaimedPeerID: &id, Provenance: &fresh}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.GetPeer(id); p.Provenance != fresh {
		t.Fatalf("explicit update lost: %+v", p.Provenance)
	}

	// Hook peers infer source=hook; nothing else is guessed.
	hookID, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentClaudeCode, Path: &path, Role: proto.RoleAgent, HookVersion: true})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := r.GetPeer(hookID); p.Provenance != (proto.Provenance{Source: proto.SourceHook, Addressable: true}) {
		t.Fatalf("hook inference = %+v", p.Provenance)
	}
	// A daemon-restart reclaim of a mapping restores the stored provenance.
	r.persistMappings(ctx)
	r2, err := NewRegistry(ctx, r.store, fakeLive{alive: map[int]bool{}}, fakeTransport{connected: map[proto.PeerID]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r2.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Path: &path, Role: proto.RoleAgent, ClaimedPeerID: &id}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r2.GetPeer(id); p.Provenance != fresh {
		t.Fatalf("reclaimed provenance = %+v, want %+v", p.Provenance, fresh)
	}
}

func TestUpdateProvenanceEmitsPeerUpdated(t *testing.T) {
	r, _ := newRegistry(t)
	ctx := context.Background()
	id, name, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "c", Backend: proto.AgentCodex, Role: proto.RoleAgent})
	if err != nil {
		t.Fatal(err)
	}
	demoted := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	found, err := r.UpdateProvenance(ctx, string(name), demoted, nil)
	if err != nil || !found {
		t.Fatalf("UpdateProvenance = %v, %v", found, err)
	}
	if p, _ := r.GetPeer(id); p.Provenance != demoted {
		t.Fatalf("provenance = %+v", p.Provenance)
	}
	var updated bool
	for _, e := range r.GetEvents() {
		if e["type"] == "peer_updated" && e["peer_id"] == string(id) {
			updated = true
		}
	}
	if !updated {
		t.Fatal("no peer_updated event after demotion")
	}
}
