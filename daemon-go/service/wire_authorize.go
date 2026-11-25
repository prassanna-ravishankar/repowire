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

// inCircle reports whether c is circle. An empty circle is a missing one,
// as everywhere else in the daemon (registration refuses it), so it matches
// nothing: a record or a peer without a circle fails closed.
func inCircle(c, circle string) bool {
	return c != "" && c == circle
}

func circleName(c string) string {
	if c == "" {
		return "(none)"
	}
	return c
}

// OrchestratorMay reports why an orchestrator holding circle may not act on
// t, or nil when it may. Every party must be in circle both as its stage
// recorded it and as the registry knows it now, so a party that moved circle
// since fails closed either way. The recorded parties include an
// orchestrator that reassigned the ask; it is not among From, Holder and Via,
// so only its recorded circle and visibility count. dest, when not nil, is
// where a reassign would move the ask: it must be in circle now, an agent or
// an orchestrator, and not private, since it becomes a party.
func (w *Wire) OrchestratorMay(ctx context.Context, t WireThread, circle string, dest *proto.Peer) error {
	switch {
	case circle == "":
		return WireRefusal{"the orchestrator has no circle"}
	case t.Kind != "ask":
		return WireRefusal{"only an ask can be acted on"}
	case !t.Open:
		return WireRefusal{"the ask is closed"}
	case t.Protected:
		return WireRefusal{"the thread is protected (" + t.ProtectedBy + ")"}
	}
	for _, r := range t.Provenance {
		if !inCircle(r.Circle, circle) {
			return WireRefusal{fmt.Sprintf("party %s was in circle %s, not %s", r.PeerID, circleName(r.Circle), circle)}
		}
	}
	for _, p := range append([]WireParty{t.From, t.Holder}, t.Via...) {
		// Protected already refuses an unknown or missing asker or holder.
		if p.Known && !inCircle(p.Circle, circle) {
			return WireRefusal{fmt.Sprintf("party %s is in circle %s now, not %s", p.PeerID, circleName(p.Circle), circle)}
		}
	}
	if dest == nil {
		return nil
	}
	switch {
	case !inCircle(dest.Circle, circle):
		return WireRefusal{fmt.Sprintf("%s is in circle %s, not %s", dest.DisplayName, circleName(dest.Circle), circle)}
	case dest.Role == proto.RoleHuman || dest.Role == proto.RoleService:
		return WireRefusal{fmt.Sprintf("%s is a %s peer", dest.DisplayName, dest.Role)}
	case w.Private(ctx, dest.PeerID):
		return WireRefusal{fmt.Sprintf("%s keeps its threads private", dest.DisplayName)}
	}
	return nil
}

// AuthorizeOrchestrator is OrchestratorMay for the thread cid as it stands
// now. Callers hold the ask's claim, so no hand-off can add a party between
// this check and the action. A party's circle, role or visibility can still
// change meanwhile, since the claim does not cover the registry; the check
// reads them at the moment of the action, which is as late as it can.
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
