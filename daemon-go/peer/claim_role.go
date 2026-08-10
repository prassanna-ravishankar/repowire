package peer

import (
	"context"
	"fmt"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// claim_role.go ports the singleton special-role claim from
// repowire.daemon.peer_registry.claim_special_role. It is a narrow v0.13 repair
// hook: only the orchestrator role can be claimed, the target peer must already
// exist (this never allocates), and a fresh ONLINE/BUSY holder in the same
// circle always blocks the claim — force cannot demote a live orchestrator.
// Offline or stale holders (and durable mapping-only rows) are demoted to AGENT
// in both live state and the persisted mappings so a restart does not
// reintroduce the bad role.

// RoleClaimConflictError is the typed conflict returned when a fresh, live peer
// already holds the requested singleton role in the target circle. Mirrors the
// Python RoleClaimConflictError; the route layer maps it to HTTP 409.
type RoleClaimConflictError struct{ msg string }

func (e *RoleClaimConflictError) Error() string { return e.msg }

// ClaimResult is the outcome of a successful ClaimSpecialRole. PreviousHolders is
// the set of peer_ids (live peers + mapping-only rows) demoted out of the role by
// this claim. AlreadyHeld is true when the target already held the role (an
// idempotent re-claim). Mirrors the Python RoleClaimResult (collapsed to ids).
type ClaimResult struct {
	Peer            *proto.Peer
	AlreadyHeld     bool
	PreviousHolders []string
}

// ClaimSpecialRole claims a singleton special role (only RoleOrchestrator) for an
// existing live peer. Returns (nil,nil) when no peer matches the identifier (→
// HTTP 404), a *RoleClaimConflictError when a fresh holder blocks the claim (→
// 409), or a plain error for a bad role / ambiguous identifier (→ 400). Mirrors
// PeerRegistry.claim_special_role.
func (r *Registry) ClaimSpecialRole(ctx context.Context, name string, role proto.PeerRole, circle *string, force bool) (*ClaimResult, error) {
	if role != proto.RoleOrchestrator {
		return nil, fmt.Errorf("Only role=orchestrator can be claimed")
	}
	now := time.Now().UTC()
	tolerance := r.HeartbeatTolerance()

	freshHolder := func(p *proto.Peer) bool {
		return p.Role == role &&
			(p.Status == proto.StatusOnline || p.Status == proto.StatusBusy) &&
			p.LastSeen != nil && now.Sub(*p.LastSeen) <= tolerance
	}

	r.mu.Lock()
	target, terr := r.resolvePeerLocked(name, circle)
	if terr != nil {
		r.mu.Unlock()
		return nil, terr // ambiguous identifier → fail loud (409 upstream)
	}
	if target == nil {
		r.mu.Unlock()
		return nil, nil
	}
	targetCircle := target.Circle
	if circle != nil && *circle != "" {
		targetCircle = *circle
	}

	// A fresh, live holder (other than the target) always blocks — even under
	// force, which cannot demote a live orchestrator. Pick the most-recently-seen
	// blocker for the message (mirrors the Python max(by last_seen)).
	var holder *proto.Peer
	for _, ps := range r.peers {
		p := ps.peer
		if p.PeerID == target.PeerID || p.Circle != targetCircle || !freshHolder(p) {
			continue
		}
		if holder == nil || lastSeenAfter(p, holder) {
			holder = p
		}
	}
	if holder != nil {
		r.mu.Unlock()
		forceNote := ""
		if force {
			forceNote = "; force cannot demote a fresh live orchestrator"
		}
		return nil, &RoleClaimConflictError{msg: fmt.Sprintf(
			"role=%s is already held by %s (%s) in circle %s%s",
			role, holder.DisplayName, holder.PeerID, targetCircle, forceNote,
		)}
	}

	var previous []string
	// Demote stale/offline live holders to AGENT (live state + durable mapping).
	for _, ps := range r.peers {
		p := ps.peer
		if p.PeerID == target.PeerID || p.Circle != targetCircle || p.Role != role {
			continue
		}
		previous = append(previous, string(p.PeerID))
		p.Role = proto.RoleAgent
		if m, ok := r.mappings[p.PeerID]; ok && m.Role != proto.RoleAgent {
			m.Role = proto.RoleAgent
			m.UpdatedAt = now
			r.markMappingsDirtyLocked()
		}
	}

	alreadyHeld := target.Role == role
	target.Role = role
	target.LastSeen = &now
	if m, ok := r.mappings[target.PeerID]; ok {
		m.Role = role
		m.UpdatedAt = now
		r.markMappingsDirtyLocked()
	}

	// Mapping-only holders (no live peer) also get demoted so restart hydration
	// doesn't reintroduce a competing orchestrator.
	for sid, m := range r.mappings {
		if sid == target.PeerID || m.Circle != targetCircle || m.Role != role {
			continue
		}
		previous = append(previous, string(sid))
		m.Role = proto.RoleAgent
		m.UpdatedAt = now
		r.markMappingsDirtyLocked()
	}

	result := &ClaimResult{Peer: clonePeer(target), AlreadyHeld: alreadyHeld, PreviousHolders: previous}
	evName := target.DisplayName
	pid := target.PeerID
	r.mu.Unlock()

	r.appendEvent(ctx, Event{
		Type:      "role_claimed",
		Timestamp: now,
		PeerID:    pid,
		PeerName:  evName,
		SessionID: pid,
		Payload: map[string]any{
			"role":             string(role),
			"circle":           targetCircle,
			"force":            force,
			"previous_holders": previous,
		},
	})
	return result, nil
}
