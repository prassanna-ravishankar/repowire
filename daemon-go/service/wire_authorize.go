package service

import (
	"context"
	"fmt"

	"github.com/repowire/repowire/daemon-go/proto"
)

// ============================================================================
// Orchestrator authority over a thread (rfcs/wire-actions.md "Common
// rules"). The operator may act on any local ask; an orchestrator only on an
// unprotected ask whose every recorded party was in the circle it holds the
// role for. Past parties are judged by what their stages recorded, never by
// today's registry, since a peer's circle can change after the fact.
// ============================================================================

// WireRefusal is an orchestrator action the thread's records forbid.
type WireRefusal struct {
	Reason string
}

func (r WireRefusal) Error() string { return "wire action refused: " + r.Reason }

// wireCircle treats a peer registered without a circle as in default.
func wireCircle(c string) string {
	if c == "" {
		return "default"
	}
	return c
}

// OrchestratorMay reports why an orchestrator holding circle may not act on
// t, or nil when it may. dest, when not nil, is where a reassign would move
// the ask: it must be in circle now, an agent or an orchestrator, and not
// private, since it becomes a party.
func (w *Wire) OrchestratorMay(ctx context.Context, t WireThread, circle string, dest *proto.Peer) error {
	circle = wireCircle(circle)
	switch {
	case t.Kind != "ask":
		return WireRefusal{"only an ask can be acted on"}
	case !t.Open:
		return WireRefusal{"the ask is closed"}
	case t.Protected:
		return WireRefusal{"the thread is protected (" + t.ProtectedBy + ")"}
	}
	for _, r := range t.Provenance {
		if wireCircle(r.Circle) != circle {
			return WireRefusal{fmt.Sprintf("party %s was in circle %s, not %s", r.PeerID, wireCircle(r.Circle), circle)}
		}
	}
	if dest == nil {
		return nil
	}
	switch {
	case wireCircle(dest.Circle) != circle:
		return WireRefusal{fmt.Sprintf("%s is in circle %s, not %s", dest.DisplayName, wireCircle(dest.Circle), circle)}
	case dest.Role == proto.RoleHuman || dest.Role == proto.RoleService:
		return WireRefusal{fmt.Sprintf("%s is a %s peer", dest.DisplayName, dest.Role)}
	case w.Private(ctx, dest.PeerID):
		return WireRefusal{fmt.Sprintf("%s keeps its threads private", dest.DisplayName)}
	}
	return nil
}

// AuthorizeOrchestrator is OrchestratorMay for the thread cid as the records
// stand now. Callers hold the ask's claim, so no hand-off can add a party
// between this check and the action.
func (w *Wire) AuthorizeOrchestrator(ctx context.Context, cid, circle string, dest *proto.Peer) error {
	t, ok, err := w.Thread(ctx, cid)
	if err != nil {
		return err
	}
	if !ok {
		return WireRefusal{"no thread " + cid}
	}
	return w.OrchestratorMay(ctx, t, circle, dest)
}
