package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Provenance and visibility (rfcs/wire-observability.md section 2): what a
// thread's parties were when it was created and at each hop, and which
// peers chose to keep their threads private. Bodies and agent interventions
// are authorized from these records, never from today's registry: a peer's
// circle can change after the fact.
// ============================================================================

// Wire visibility modes.
const (
	WireVisible = "visible"
	WirePrivate = "private"
)

// WireVisibilityStore persists each peer's wire visibility; *state.Store
// satisfies it.
type WireVisibilityStore interface {
	SetPeerWireVisibility(ctx context.Context, peerID, mode string) error
	PeerWireVisibilities(ctx context.Context) (map[string]string, error)
}

// WireProvenance is one party as it was when a stage recorded it.
type WireProvenance struct {
	PeerID  proto.PeerID   `json:"peer_id"`
	Circle  string         `json:"circle"`
	Role    proto.PeerRole `json:"role"`
	Private bool           `json:"private,omitempty"`
	Unknown bool           `json:"unknown,omitempty"` // the registry did not know the peer
}

// wireProvenanceKey is the trace detail key the parties are recorded under.
const wireProvenanceKey = "provenance"

// WithProvenance adds ids, as they are now, to a stage's detail: the stage
// that first names a thread's parties and each hop call it. A nil Wire adds
// nothing, and a thread recorded without provenance is protected.
func (w *Wire) WithProvenance(ctx context.Context, detail map[string]any, ids ...proto.PeerID) map[string]any {
	if recs := w.Provenance(ctx, ids...); len(recs) > 0 {
		if detail == nil {
			detail = map[string]any{}
		}
		detail[wireProvenanceKey] = recs
	}
	return detail
}

// Provenance returns ids as they are now. A nil Wire returns nil.
func (w *Wire) Provenance(ctx context.Context, ids ...proto.PeerID) []WireProvenance {
	if w == nil {
		return nil
	}
	out := make([]WireProvenance, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		rec := WireProvenance{PeerID: id, Private: w.Private(ctx, id), Unknown: true}
		if p, ok := w.peers.GetPeer(id); ok && p != nil {
			rec.Circle, rec.Role, rec.Unknown = p.Circle, p.Role, false
		}
		out = append(out, rec)
	}
	return out
}

// Private reports whether id chose private. A visibility table that cannot
// be read counts every peer as private, so a failure protects threads
// instead of exposing them.
func (w *Wire) Private(ctx context.Context, id proto.PeerID) bool {
	if w == nil || w.vis == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.loadVisibilityLocked(ctx); err != nil {
		if !w.visErrLogged {
			log.Printf("wire: load visibility (every peer counts as private until it loads): %v", err)
			w.visErrLogged = true
		}
		return true
	}
	return w.private[id]
}

// SetVisibility records id's wire visibility, visible or private. The write
// and the cache change together under the lock.
func (w *Wire) SetVisibility(ctx context.Context, id proto.PeerID, mode string) error {
	if mode != WireVisible && mode != WirePrivate {
		return fmt.Errorf("wire visibility must be %s or %s", WireVisible, WirePrivate)
	}
	if w.vis == nil {
		return fmt.Errorf("wire visibility needs the state store")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.loadVisibilityLocked(ctx); err != nil {
		return err
	}
	if err := w.vis.SetPeerWireVisibility(ctx, string(id), mode); err != nil {
		return err
	}
	w.private[id] = mode == WirePrivate
	return nil
}

// loadVisibilityLocked fills the cache from the store once. Caller holds w.mu.
func (w *Wire) loadVisibilityLocked(ctx context.Context) error {
	if w.private != nil {
		return nil
	}
	loaded, err := w.vis.PeerWireVisibilities(ctx)
	if err != nil {
		return err
	}
	w.private = make(map[proto.PeerID]bool, len(loaded))
	for id, mode := range loaded {
		w.private[proto.PeerID(id)] = mode == WirePrivate
	}
	return nil
}

// recordsParties reports whether a stage of a thread of kind must carry
// provenance: the stage that first names the parties (an ask's created, a
// notify's resolved_peer) and every hop.
func recordsParties(kind, stage string) bool {
	switch stage {
	case "created", "delegated", "reassigned":
		return kind == "ask"
	case "resolved_peer":
		return kind == "notify"
	}
	return false
}

// threadProvenance collects the parties a thread's stages recorded, and
// whether every stage that should have recorded them did. A rolled-back hop
// still counts: its parties were offered the thread.
func threadProvenance(kind string, rows []state.TraceRow) ([]WireProvenance, bool) {
	var out []WireProvenance
	named := false
	for _, r := range rows {
		if !recordsParties(kind, r.Stage) {
			continue
		}
		recs, ok := decodeProvenance(r.Detail[wireProvenanceKey])
		if !ok {
			return out, false
		}
		named = named || r.Stage == "created" || r.Stage == "resolved_peer"
		out = append(out, recs...)
	}
	return out, named
}

func decodeProvenance(v any) ([]WireProvenance, bool) {
	if v == nil {
		return nil, false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out []WireProvenance
	if err := json.Unmarshal(b, &out); err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// protectedBy names why a thread is protected, or "" when it is not: a
// party, now or when recorded, is the human, a surface service, private,
// or unknown to the registry; or its provenance is missing. parties starts
// with the asker and the holder, which a thread must name.
func protectedBy(parties []WireParty, recorded []WireProvenance, complete bool) string {
	for i, p := range parties {
		switch {
		case p.PeerID == "" && i < 2:
			return "unknown_party"
		case p.PeerID == "":
		case !p.Known:
			return "unknown_party"
		case p.Role == proto.RoleHuman || p.Role == proto.RoleService:
			return string(p.Role)
		case p.Private:
			return WirePrivate
		}
	}
	for _, r := range recorded {
		switch {
		case r.Unknown:
			return "unknown_party"
		case r.Role == proto.RoleHuman || r.Role == proto.RoleService:
			return string(r.Role)
		case r.Private:
			return WirePrivate
		}
	}
	if !complete {
		return "no_provenance"
	}
	return ""
}
