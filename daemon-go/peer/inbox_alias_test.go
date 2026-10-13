package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// TestCheckAccessAliasesInboxSurfacesToTheHuman: once a human peer exists, a
// phone bridge stops being an addressee. Addressing it (by name or peer_id)
// reaches the human, and a message the bridge sends comes from the human.
func TestCheckAccessAliasesInboxSurfacesToTheHuman(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	agentID, agent, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "1", Backend: proto.AgentClaudeCode, Path: ptr("/work/api"), Machine: "m", Role: proto.RoleAgent})
	if err != nil {
		t.Fatal(err)
	}
	name := proto.DisplayName("telegram")
	bridgeID, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "default", Backend: proto.AgentClaudeCode, Path: ptr("/telegram"), Machine: "m", Role: proto.RoleService, PreferredDisplayName: &name})
	if err != nil {
		t.Fatal(err)
	}

	// Before a human is registered the bridge is an ordinary service peer.
	if _, to, err := r.CheckAccess(ctx, string(agent), "telegram", false, nil); err != nil || to.PeerID != bridgeID {
		t.Fatalf("pre-human target = %v, %v", to, err)
	}

	human := proto.DisplayName("prass")
	humanID, _, err := r.AllocateAndRegister(ctx, AllocateParams{Circle: "default", Backend: proto.AgentHuman, Path: ptr("/human"), Machine: "m", Role: proto.RoleHuman, PreferredDisplayName: &human, Metadata: map[string]any{"in_process": true}})
	if err != nil {
		t.Fatal(err)
	}
	r.SetHumanPeer(humanID)

	for _, address := range []string{"telegram", string(bridgeID), "prass"} {
		if _, to, err := r.CheckAccess(ctx, string(agent), address, false, nil); err != nil || to.PeerID != humanID {
			t.Fatalf("CheckAccess(%q) = %v, %v; want the human", address, to, err)
		}
	}
	from, to, err := r.CheckAccess(ctx, "telegram", string(agentID), false, nil)
	if err != nil || from == nil || from.PeerID != humanID || to.PeerID != agentID {
		t.Fatalf("bridge as sender = %v -> %v, %v; want the human as sender", from, to, err)
	}
	// Agents are never aliased.
	if _, to, _ := r.CheckAccess(ctx, "telegram", string(agent), false, nil); to.PeerID != agentID {
		t.Fatalf("agent target aliased: %v", to)
	}
}
