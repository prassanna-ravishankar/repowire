package peer

import (
	"context"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// After a daemon restart only durable mappings remain. A connect that claims
// the process's latest identity gets it back; one that claims an id long since
// pruned falls through to the name tuple, which picks the mapping holding the
// folder name: the identity the session once held (the 2026-11-25 rollout).
func TestRestartClaimDecidesWhichIdentityReturns(t *testing.T) {
	ctx := context.Background()
	const pid = 33498
	path := "/work/repowire"
	mapping := func(id, name string, agentPID int, at time.Time) *proto.SessionMapping {
		return &proto.SessionMapping{SessionID: proto.PeerID(id), DisplayName: proto.DisplayName(name), Circle: "1",
			Backend: proto.AgentClaudeCode, Path: ptr(path), Role: proto.RoleAgent, UpdatedAt: at, AgentPID: ptr(agentPID)}
	}
	for claim, want := range map[proto.PeerID]proto.PeerID{
		"repow-1-05ee4cfe": "repow-1-05ee4cfe",
		"repow-1-e9cb2c1d": "repow-1-01abdd10",
	} {
		store := newMemStore()
		for _, m := range []*proto.SessionMapping{
			mapping("repow-1-01abdd10", "repowire-claude-code", 500, time.Date(2026, 10, 16, 23, 9, 0, 0, time.UTC)),
			mapping("repow-1-05ee4cfe", "repowire-2-claude-code", pid, time.Date(2026, 11, 25, 6, 19, 11, 0, time.UTC)),
		} {
			store.mappings[m.SessionID] = m
		}
		r, err := NewRegistry(ctx, store, fakeLive{alive: map[int]bool{pid: true}}, fakeTransport{connected: map[proto.PeerID]bool{}})
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := r.AllocateAndRegister(ctx, AllocateParams{ClaimedPeerID: ptr(claim), Circle: "1", Backend: proto.AgentClaudeCode,
			Path: ptr(path), Machine: "m", Role: proto.RoleAgent, AgentPID: ptr(pid), PaneID: ptr("%29")})
		if err != nil {
			t.Fatal(err)
		}
		if id != want {
			t.Fatalf("claim %s reconnected as %s; want %s", claim, id, want)
		}
	}
}
