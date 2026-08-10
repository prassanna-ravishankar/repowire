package peer

import (
	"context"
	"fmt"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// registry_access.go is the FOUNDATION cross-cutting seam: the access-control,
// addressing, and live-state-update resolvers the hub delivery / ask-lifecycle /
// session routes depend on. Ported from repowire/daemon/peer_registry.py
// (check_access, resolve_peer_strict, update_peer_model/metadata). Proto
// discipline holds: proto.PeerID is identity, proto.DisplayName is addressing —
// these resolve an addressing string ONCE (fail-loud on ambiguity) then key on
// the canonical PeerID. The hub's narrow accessRegistry / askRoutesRegistry seams
// collapse to *peer.Registry once these land; the method bodies satisfy those
// interfaces without change.
//
// ClaimSpecialRole + ClaimResult + RoleClaimConflictError live in claim_role.go
// (same package, same owner). SubscribeEvents lives in events.go beside the
// subscriber set it wakes. Both are deliberately not redeclared here.

// CheckAccess resolves sender/target and enforces circle access for a non-WS
// dispatch (the ACP-broker / delivery path). Mirrors peer_registry.check_access:
// the target must resolve (unknown/ambiguous → non-nil err); from is best-effort
// (nil for an unknown sender — notify proceeds, matching Python). A circle
// boundary violation is a non-nil err. Roles that bypass circles
// (RoleService/Orchestrator/Human, via proto.PeerRole.BypassesCircles) skip the
// boundary check, as does bypassCircle=true.
func (r *Registry) CheckAccess(ctx context.Context, fromPeer, toPeer string, bypassCircle bool, circle *string) (from, to *proto.Peer, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	target, terr := r.resolvePeerLocked(toPeer, circle)
	if terr != nil {
		return nil, nil, terr // ambiguous target → fail loud (409)
	}
	if target == nil {
		return nil, nil, fmt.Errorf("Unknown peer: %s", toPeer)
	}

	// from_peer is resolved best-effort, scoped to the target's circle first
	// (matching _resolve_from_peer_unlocked), then unscoped. An ambiguous-name
	// error on the sender side is swallowed: an unresolved sender proceeds.
	targetCircle := target.Circle
	fromObj, ferr := r.resolvePeerLocked(fromPeer, &targetCircle)
	if ferr != nil || fromObj == nil {
		fromObj, _ = r.resolvePeerLocked(fromPeer, nil)
	}

	if cerr := checkCircleAccess(fromObj, target, bypassCircle); cerr != nil {
		return clonePeer(fromObj), nil, cerr
	}
	// Clone at the public boundary: delivery/broadcast/query callers read these
	// off-lock while register/status/metadata writers mutate the live structs.
	return clonePeer(fromObj), clonePeer(target), nil
}

// checkCircleAccess enforces the circle boundary given already-resolved peers.
// Mirrors _check_circle_access_by_peers: bypass / unknown-either-side / a
// circle-bypassing role on either side all permit; a same-resolution mismatch
// is the boundary violation.
func checkCircleAccess(from, to *proto.Peer, bypass bool) error {
	if bypass || from == nil || to == nil {
		return nil
	}
	if from.Role.BypassesCircles() || to.Role.BypassesCircles() {
		return nil
	}
	if from.Circle != to.Circle {
		return fmt.Errorf(
			"Circle boundary: %s (%s) cannot access %s (%s)",
			from.DisplayName, from.Circle, to.DisplayName, to.Circle,
		)
	}
	return nil
}

// GetPeerByName resolves a display_name (or peer_id) within an optional circle
// scope to the single canonical peer. Ambiguity is a fail-loud error (→ 409),
// matching the askRoutesRegistry seam and peer_registry.get_peer's
// _lookup_peer_unlocked. (nil,nil) means no such peer (→ 404).
func (r *Registry) GetPeerByName(name string, circle *string) (*proto.Peer, error) {
	return r.ResolvePeer(name, circle)
}

// ResolvePeerStrict resolves an addressing string (peer_id OR display_name),
// optionally circle-scoped, to ALL matches — for destructive ops (kill/restart/
// spawn-collision) where silently picking a winner is wrong. Returns nil (no
// match), a single-element slice (peer_id hit or unique name), or N candidates
// (ambiguous name). Mirrors peer_registry.resolve_peer_strict; callers branch on
// len. Never returns an error: ambiguity is surfaced as len>1, not a failure.
func (r *Registry) ResolvePeerStrict(identifier string, circle *string) []*proto.Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	inCircle := func(p *proto.Peer) bool { return circle == nil || p.Circle == *circle }

	// peer_id hit is unambiguous (single).
	if ps, ok := r.peers[proto.PeerID(identifier)]; ok && inCircle(ps.peer) {
		return []*proto.Peer{clonePeer(ps.peer)}
	}

	var byName []*proto.Peer
	for _, ps := range r.peers {
		if string(ps.peer.DisplayName) == identifier && inCircle(ps.peer) {
			byName = append(byName, clonePeer(ps.peer))
		}
	}
	return byName
}

// UpdateModelByName updates a peer's observed runtime model in live + durable
// state, addressed by peer_id or display_name. Mirrors
// peer_registry.update_peer_model: unknown peer → (false,nil) no-op; ambiguous
// name → (false,err). A no-op model match still returns found=true (the peer
// exists), matching Python's early return.
func (r *Registry) UpdateModelByName(ctx context.Context, identifier, model string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.resolvePeerLocked(identifier, nil)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	if p.Model != nil && *p.Model == model {
		return true, nil
	}
	now := time.Now().UTC()
	m := model
	p.Model = &m
	p.LastSeen = &now
	if mp, ok := r.mappings[p.PeerID]; ok && (mp.Model == nil || *mp.Model != model) {
		mp.Model = &m
		mp.UpdatedAt = now
		r.markMappingsDirtyLocked()
	}
	return true, nil
}

// UpdateMetadataByName merges metadata into a peer's live state, addressed by
// peer_id or display_name. Mirrors
// peer_registry.update_peer_metadata: unknown peer → (false,nil); ambiguous name
// → (false,err). Metadata is a live-only field (not persisted in the mapping),
// so no durable write — matching Python.
func (r *Registry) UpdateMetadataByName(ctx context.Context, identifier string, metadata map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.resolvePeerLocked(identifier, nil)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	if p.Metadata == nil {
		p.Metadata = make(map[string]any, len(metadata))
	}
	for k, v := range metadata {
		p.Metadata[k] = v
	}
	now := time.Now().UTC()
	p.LastSeen = &now
	return true, nil
}
