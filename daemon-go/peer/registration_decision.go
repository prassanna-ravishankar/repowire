package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// RegistrationRules versions the identity rules AllocateAndRegister applies
// (docs/concepts/peer-identity-lifecycle.md). Bump it when a step changes what
// it selects, so a replay of the journal can tell rule sets apart.
const RegistrationRules = 1

// registrationDecision is one registration as AllocateAndRegister decided it
// (#493): what the registrant proposed and proved, what every identity step
// concluded about which candidate, which pids it found alive, and the id it
// selected or why it refused. It is journaled once, as registration_decision,
// so an offline reader can replay the rules without the mutable final-state
// tables. Built and emitted under r.mu.
type registrationDecision struct {
	seq      int64
	at       time.Time
	params   AllocateParams
	path     string
	baseName proto.DisplayName
	kind     string // reconnect | registered | sticky_orchestrator
	steps    []map[string]any
	pids     []map[string]any
	by       string // the step that selected the id
}

// Step labels, as the registration code names them.
const (
	stepRetirementGuard = "a"
	stepRuntimePID      = "a2"
	stepClaim           = "b"
	stepSessionBinding  = "b1"
	stepNameTuple       = "b2"
	stepMint            = "c"
	stepPane            = "pane"
)

// beginDecisionLocked opens the record for the registration in progress.
func (r *Registry) beginDecisionLocked(params AllocateParams, path string, at time.Time) {
	r.decisionSeq++
	r.deciding = &registrationDecision{seq: r.decisionSeq, at: at, params: params, path: path, baseName: baseDisplayName(params)}
}

// noteLocked records one step's verdict on a candidate: selected, rejected,
// skipped, refused, ambiguous, unretired or yielded, with the reason and the
// candidate's retirement at that moment. "selected" marks the step that chose
// the id; a later selection (the sticky orchestrator) replaces it. No-op
// outside a registration.
func (r *Registry) noteLocked(step, outcome string, id proto.PeerID, reason string, candidates ...proto.PeerID) {
	d := r.deciding
	if d == nil {
		return
	}
	n := map[string]any{"step": step, "outcome": outcome, "reason": reason, "peer_id": nil}
	if id != "" {
		n["peer_id"] = string(id)
		n["retirement"] = r.retirementLocked(id)
	}
	if len(candidates) > 0 {
		list := make([]map[string]any, len(candidates))
		for i, c := range candidates {
			list[i] = map[string]any{"peer_id": string(c), "retirement": r.retirementLocked(c)}
		}
		n["candidates"] = list
	}
	if outcome == "selected" {
		d.by = step
	}
	d.steps = append(d.steps, n)
}

// pidAliveLocked is Liveness.PIDAlive, recorded in the registration in
// progress so the record shows every liveness answer the rules consulted.
func (r *Registry) pidAliveLocked(pid int) bool {
	alive := r.live.PIDAlive(pid)
	if d := r.deciding; d != nil {
		d.pids = append(d.pids, map[string]any{"pid": pid, "alive": alive})
	}
	return alive
}

// retirementLocked is id's retirement now: none, soft or hard.
func (r *Registry) retirementLocked(id proto.PeerID) string {
	ret, ok := r.retired[id]
	switch {
	case !ok:
		return "none"
	case ret.Hard:
		return "hard"
	}
	return "soft"
}

// decisionRefLocked correlates an event with the registration in progress
// (peer_online carries it).
func (r *Registry) decisionRefLocked() map[string]any {
	if d := r.deciding; d != nil {
		return map[string]any{"run_id": r.runID, "decision_seq": d.seq}
	}
	return nil
}

// finishDecisionLocked journals the registration as decided and closes it.
// err is the refusal, nil when the registration went through.
func (r *Registry) finishDecisionLocked(ctx context.Context, id proto.PeerID, name proto.DisplayName, err error) {
	d := r.deciding
	if d == nil {
		return
	}
	r.deciding = nil
	p := d.params
	payload := map[string]any{
		"rules":       RegistrationRules,
		"run_id":      r.runID,
		"seq":         d.seq,
		"observed_at": d.at.Format(time.RFC3339Nano),
		"inputs": map[string]any{
			"backend":                string(p.Backend),
			"path":                   orNil(d.path),
			"path_key":               pathKey(d.path),
			"base_name":              string(d.baseName),
			"preferred_display_name": derefOrNil(p.PreferredDisplayName),
			"circle":                 p.Circle,
			"circle_source":          orNil(p.CircleSource),
			"role":                   orNil(string(p.Role)),
			"machine":                orNil(p.Machine),
		},
		// An absent field is null; a supplied claim the daemon did not
		// verify is the claim with runtime_identity_verified false.
		"evidence": map[string]any{
			"agent_pid":                 derefOrNil(p.AgentPID),
			"parent_pid":                derefOrNil(p.ParentPID),
			"runtime_session_id":        orNil(runtimeSessionID(p.Metadata)),
			"claimed_peer_id":           derefOrNil(p.ClaimedPeerID),
			"runtime_identity_verified": p.RuntimeIdentityVerified,
			"pane_id":                   derefOrNil(p.PaneID),
		},
		"steps":      orEmpty(d.steps),
		"pid_checks": orEmpty(d.pids),
		"selected":   nil,
	}
	ev := Event{Type: "registration_decision", Timestamp: d.at, PeerID: id, PeerName: name, SessionID: id, Payload: payload}
	if err != nil {
		payload["outcome"] = "refused"
		payload["error"] = err.Error()
		if p.ClaimedPeerID != nil {
			ev.PeerID, ev.SessionID = *p.ClaimedPeerID, *p.ClaimedPeerID
		}
	} else {
		payload["outcome"] = d.kind
		selected := map[string]any{"peer_id": string(id), "display_name": string(name), "step": d.by}
		if ps, ok := r.peers[id]; ok {
			selected["circle"] = ps.peer.Circle
		}
		payload["selected"] = selected
	}
	// An audit record for offline replay: the journal only, never the live
	// window or its stream, where a restart would double every lifecycle line
	// and push older events out.
	r.journal(ctx, ev)
}

// pathKey is a stable opaque key for a workspace path. The local journal
// keeps the raw path next to it; an export for analysis keeps the key and
// drops the path. Null without a path.
func pathKey(path string) any {
	if path == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:8])
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefOrNil[T ~string | ~int](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

func orEmpty(list []map[string]any) []map[string]any {
	if list == nil {
		return []map[string]any{}
	}
	return list
}
