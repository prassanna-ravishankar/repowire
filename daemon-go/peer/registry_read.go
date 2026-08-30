package peer

import (
	"fmt"
	"sort"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// registry_read.go holds the addressing-side resolvers that take a human string
// (peer_id OR display_name) rather than a canonical proto.PeerID, plus the
// orchestrator/heartbeat accessors the routes need. Identity-keyed lookups
// (GetPeer(proto.PeerID), GetPeerByPane, GetAllPeers) live in registry.go /
// events.go; this file deliberately does NOT redeclare them.

// defaultHeartbeatInterval mirrors config.models DaemonConfig.heartbeat_interval.
const defaultHeartbeatInterval = 30 * time.Second

// HeartbeatTolerance is how stale a peer's last_seen may get before it counts as
// dead: two heartbeat intervals. Mirrors PeerRegistry.heartbeat_tolerance.
func (r *Registry) HeartbeatTolerance() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.heartbeatInterval * 2
}

func (r *Registry) GetMapping(id proto.PeerID) (*proto.SessionMapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.mappings[id]
	if !ok {
		return nil, false
	}
	copy := *m
	return &copy, true
}

// ResolvePeer resolves an addressing string (peer_id OR display_name), optionally
// scoped to a circle, to the canonical peer. Unlike events.go's best-effort
// ResolveByIdentifier, this is the FAIL-LOUD resolver the route group uses: an
// unscoped display_name matching >1 viable peer that pane ownership can't
// disambiguate returns an error (→ HTTP 409), never a silent guess (issue #136
// misroute). (nil,nil) means "no such peer" (→ HTTP 404). Mirrors
// PeerRegistry._lookup_peer_unlocked.
func (r *Registry) ResolvePeer(identifier string, circle *string) (*proto.Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Clone at the PUBLIC boundary only: resolvePeerLocked returns a live pointer
	// because identity-mutating wrappers (touch/description) resolve then mutate
	// under one write lock. Off-lock route callers must get a snapshot instead.
	p, err := r.resolvePeerLocked(identifier, circle)
	r.applyDescriptionTTLLocked(p)
	return clonePeer(p), err
}

// resolvePeerLocked is ResolvePeer's body; callers must hold at least the read
// lock. Factored out so identity-mutating wrappers (touch/description) can
// resolve then mutate under one write lock without a lock upgrade.
func (r *Registry) resolvePeerLocked(identifier string, circle *string) (*proto.Peer, error) {
	// peer_id hit is unambiguous.
	if ps, ok := r.peers[proto.PeerID(identifier)]; ok {
		return ps.peer, nil
	}

	var matches []*proto.Peer
	for _, ps := range r.peers {
		if string(ps.peer.DisplayName) == identifier {
			matches = append(matches, ps.peer)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if circle != nil {
		filtered := matches[:0:0]
		for _, p := range matches {
			if p.Circle == *circle {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			// Human and service peers are intentionally addressable across
			// circles. Fall back only when the global name is unique and the
			// target's role is itself circle-bypassing; ordinary agents remain
			// strictly scoped and ambiguous names still fail loud.
			if len(matches) == 1 && matches[0].Role.BypassesCircles() {
				return matches[0], nil
			}
			return nil, nil
		}
		matches = filtered
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	// Prefer non-offline candidates; fall back to all matches.
	var active []*proto.Peer
	for _, p := range matches {
		if p.Status != proto.StatusOffline {
			active = append(active, p)
		}
	}
	candidates := active
	if len(candidates) == 0 {
		candidates = matches
	}

	// Ambiguous: >1 viable candidate, no explicit circle. Pane ownership is the
	// only safe tiebreaker; anything else is a guess (issue #136). Refuse loud.
	if circle == nil && len(candidates) > 1 {
		var paned []*proto.Peer
		for _, p := range candidates {
			if p.PaneID != nil && *p.PaneID != "" {
				paned = append(paned, p)
			}
		}
		if len(paned) == 1 {
			return paned[0], nil
		}
		circleSet := map[string]struct{}{}
		for _, p := range candidates {
			circleSet[p.Circle] = struct{}{}
		}
		circles := make([]string, 0, len(circleSet))
		for c := range circleSet {
			circles = append(circles, c)
		}
		sort.Strings(circles)
		return nil, fmt.Errorf(
			"ambiguous peer name %q: matches in circles %v. Specify a circle= or pass a peer_id",
			identifier, circles,
		)
	}

	// Circle-scoped (or single active) preference: connected, then pane-owned,
	// then most-recently-seen. Mirrors _lookup_peer_unlocked.preference.
	best := candidates[0]
	bestKey := r.preferenceKey(best)
	for _, p := range candidates[1:] {
		k := r.preferenceKey(p)
		if k.greater(bestKey) {
			best, bestKey = p, k
		}
	}
	return best, nil
}

// preferenceKey ranks a peer for tie-breaking: (connected, pane-owned, last_seen).
type preferenceKey struct {
	connected bool
	paned     bool
	lastSeen  int64
}

func (a preferenceKey) greater(b preferenceKey) bool {
	if a.connected != b.connected {
		return a.connected
	}
	if a.paned != b.paned {
		return a.paned
	}
	return a.lastSeen > b.lastSeen
}

func (r *Registry) preferenceKey(p *proto.Peer) preferenceKey {
	connected := r.transport != nil && r.transport.IsConnected(p.PeerID)
	var ls int64
	if p.LastSeen != nil {
		ls = p.LastSeen.UnixNano()
	}
	return preferenceKey{
		connected: connected,
		paned:     p.PaneID != nil && *p.PaneID != "",
		lastSeen:  ls,
	}
}

// GetOrchestrator returns the live orchestrator for a circle, or (nil,false).
// Live = role=orchestrator, status online/busy, last_seen within
// HeartbeatTolerance. When several match, the most-recently-seen wins. Mirrors
// PeerRegistry.get_orchestrator.
func (r *Registry) GetOrchestrator(circle string) (*proto.Peer, bool) {
	tolerance := r.HeartbeatTolerance()
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now().UTC()
	var best *proto.Peer
	for _, ps := range r.peers {
		p := ps.peer
		if p.Circle != circle || p.Role != proto.RoleOrchestrator {
			continue
		}
		if p.Status != proto.StatusOnline && p.Status != proto.StatusBusy {
			continue
		}
		if p.LastSeen == nil || now.Sub(*p.LastSeen) > tolerance {
			continue
		}
		if best == nil || (best.LastSeen != nil && p.LastSeen.After(*best.LastSeen)) {
			best = p
		}
	}
	if best == nil {
		return nil, false
	}
	return clonePeer(best), true // snapshot for the off-lock route reader
}
