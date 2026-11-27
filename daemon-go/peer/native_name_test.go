package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func registerAt(t *testing.T, r *Registry, path string, backend proto.AgentType) proto.PeerID {
	t.Helper()
	id, _, err := r.AllocateAndRegister(context.Background(), AllocateParams{Circle: "one", Backend: backend, Path: &path, Role: proto.RoleAgent})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nameOf(t *testing.T, r *Registry, id proto.PeerID) proto.DisplayName {
	t.Helper()
	p, ok := r.GetPeer(id)
	if !ok {
		t.Fatalf("peer %s gone", id)
	}
	return p.DisplayName
}

// The runtime's own session name becomes the address, renamed in place.
func TestApplyNativeNameRenamesInPlace(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	id := registerAt(t, r, "/work/a", proto.AgentClaudeCode)

	name, ok := r.ApplyNativeName(ctx, id, "  Fix Telegram access ")
	if !ok || name != "fix-telegram-access" || nameOf(t, r, id) != name {
		t.Fatalf("ApplyNativeName = %q, %v; peer named %q", name, ok, nameOf(t, r, id))
	}
	if name, _ := r.ApplyNativeName(ctx, id, ""); name != "fix-telegram-access" {
		t.Fatalf("an empty native name renamed the peer to %q", name)
	}
	if _, ok := r.ApplyNativeName(ctx, "repow-one-missing", "x"); ok {
		t.Fatal("an unknown peer reported ok")
	}
}

// A live holder pushes the name to -2; repeating the same native name keeps
// the suffixed address instead of re-deriving it on every report.
func TestApplyNativeNameSuffixesPastLiveHolderAndOnlyActsOnChange(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	a := registerAt(t, r, "/work/a", proto.AgentClaudeCode)
	b := registerAt(t, r, "/work/b", proto.AgentCodex)

	r.ApplyNativeName(ctx, a, "Shared")
	if name, _ := r.ApplyNativeName(ctx, b, "Shared"); name != "shared-2" {
		t.Fatalf("second holder = %q, want shared-2", name)
	}
	if _, err := r.MarkOffline(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if name, _ := r.ApplyNativeName(ctx, b, "Shared"); name != "shared-2" {
		t.Fatalf("an unchanged native name was re-derived to %q", name)
	}
	if name, _ := r.ApplyNativeName(ctx, b, "Shared again"); name != "shared-again" {
		t.Fatalf("a changed native name = %q, want shared-again", name)
	}
}

// An offline ghost holding the name yields it.
func TestApplyNativeNameEvictsOfflineGhost(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	ghost := registerAt(t, r, "/work/a", proto.AgentClaudeCode)
	live := registerAt(t, r, "/work/b", proto.AgentCodex)
	r.ApplyNativeName(ctx, ghost, "Ghost")
	if _, err := r.MarkOffline(ctx, ghost, false); err != nil {
		t.Fatal(err)
	}
	if name, _ := r.ApplyNativeName(ctx, live, "Ghost"); name != "ghost" {
		t.Fatalf("live peer = %q, want ghost", name)
	}
	if _, ok := r.GetPeer(ghost); ok {
		t.Fatal("the offline ghost still holds a record")
	}
}

// A runtime title only seeds an empty description.
func TestSeedDescriptionOnlyWhenEmpty(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	id := registerAt(t, r, "/work/a", proto.AgentClaudeCode)

	if !r.SeedDescription(ctx, id, "Telegram access fix") {
		t.Fatal("an empty description was not seeded")
	}
	if _, err := r.UpdateDescription(ctx, string(id), "explicit", nil); err != nil {
		t.Fatal(err)
	}
	if r.SeedDescription(ctx, id, "Another title") {
		t.Fatal("a seed overwrote an explicit description")
	}
	if p, _ := r.GetPeer(id); p.Description != "explicit" {
		t.Fatalf("description = %q, want explicit", p.Description)
	}
}
