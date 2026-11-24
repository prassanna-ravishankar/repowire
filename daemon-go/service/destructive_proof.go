package service

import (
	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
)

// DestructiveProof is the kill-authorization result (port of _DestructivePaneProof).
type DestructiveProof struct {
	OK          bool
	PaneID      string
	TmuxSession string
	Mode        string
	ErrCode     string
	Hint        string
}

// DestructivePaneProof ports _destructive_pane_proof: the only accepted proofs
// are (1) the pane is in the in-process spawned set, (2) durable spawn ownership
// validates, or (3) live pane metadata (the ws-hook meta.json) names THIS
// peer_id and its agent still runs in the pane. Path match alone is NEVER proof.
// A live pane whose metadata names a different peer is a mismatch (distinct from
// no metadata); both refuse. Whatever the mode, a known agent pid for the peer
// must be running in the pane (#529).
func (s *SpawnService) DestructivePaneProof(proc ProcessControl, p *proto.Peer) DestructiveProof {
	proof := s.paneProof(proc, p)
	if !proof.OK || len(PeerAgentPIDs(proof.PaneID, p)) == 0 {
		return proof
	}
	// A known agent outside a visible pane vetoes every Mode: the pane's proof
	// was earned by a runtime that is no longer there. An unseen pane is left to
	// the caller, which cannot kill it either.
	if ev := s.Container().Probe(proof.PaneID); ev != nil && !PaneRunsPeer(proc, ev, p) {
		return DestructiveProof{PaneID: proof.PaneID, TmuxSession: proof.TmuxSession, ErrCode: "agent_not_in_pane",
			Hint: "The peer's agent is not running in its proven pane; refusing to kill whatever runs there now."}
	}
	return proof
}

func (s *SpawnService) paneProof(proc ProcessControl, p *proto.Peer) DestructiveProof {
	own := s.Ownership()

	if p.PaneID != nil && *p.PaneID != "" && own.IsSpawned(*p.PaneID) {
		return DestructiveProof{OK: true, PaneID: *p.PaneID, Mode: "repowire_spawned_pane",
			TmuxSession: derefStr(p.TmuxSession)}
	}

	ownership := own.ValidateForPeer(p)
	if ownership.OK && ownership.Record != nil {
		return DestructiveProof{OK: true, PaneID: ownership.Record.PaneID,
			TmuxSession: ownership.Record.TmuxSession, Mode: "durable_spawn_ownership"}
	}
	if ownership.Error != "" && ownership.Error != "missing_ownership" {
		pane := derefStr(p.PaneID)
		if ownership.Record != nil {
			pane = ownership.Record.PaneID
		}
		ts := derefStr(p.TmuxSession)
		if ownership.Evidence != nil {
			ts = ownership.Evidence.TmuxSession
		} else if ownership.Record != nil {
			ts = ownership.Record.TmuxSession
		}
		return DestructiveProof{PaneID: pane, TmuxSession: ts, ErrCode: ownership.Error, Hint: ownership.Hint}
	}

	if p.PaneID == nil || *p.PaneID == "" {
		return DestructiveProof{ErrCode: "missing_pane",
			Hint: "Peer has no pane id, so Repowire has no pane to kill."}
	}

	ev := s.Container().Probe(*p.PaneID)
	if ev == nil {
		return DestructiveProof{PaneID: *p.PaneID, ErrCode: "pane_not_live",
			Hint: "The peer's recorded pane is not visible in tmux."}
	}

	// (3) Live pane metadata names THIS peer_id. The ws-hook writes the pane's
	// owning peer_id into ws-hook-<pane>.meta.json; a match proves this peer really
	// occupies the live pane (not merely shares its path). Parity with
	// spawn.py destructive proof mode 3. Path match alone is still NOT proof.
	if meta := hooks.ReadPaneRuntimeMetadata(*p.PaneID); meta != nil {
		if mpid, _ := meta["peer_id"].(string); mpid != "" {
			if mpid == string(p.PeerID) {
				if !PaneRunsPeer(proc, ev, p) {
					return DestructiveProof{PaneID: *p.PaneID, TmuxSession: ev.TmuxSession, ErrCode: "pane_metadata_stale",
						Hint: "Pane metadata names this peer, but its agent no longer runs in the pane."}
				}
				return DestructiveProof{OK: true, PaneID: *p.PaneID, TmuxSession: ev.TmuxSession,
					Mode: "verified_pane_metadata"}
			}
			// Meta names a DIFFERENT peer — the live pane is another peer's, not this
			// one. Distinguish from "no metadata" for diagnostics (parity with Python
			// pane_metadata_mismatch). Same blocking behavior either way.
			return DestructiveProof{PaneID: *p.PaneID, TmuxSession: ev.TmuxSession,
				ErrCode: "pane_metadata_mismatch",
				Hint:    "Live pane's peer_id metadata names a different peer. Refusing destructive control."}
		}
	}

	return DestructiveProof{PaneID: *p.PaneID, TmuxSession: ev.TmuxSession,
		ErrCode: "missing_pane_metadata",
		Hint:    "Live pane has no peer_id metadata. Path match alone is not enough for destructive controls."}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// StopOutcome is what StopProven did: the proof it acted on, and whether the
// proven place was stopped and forgotten.
type StopOutcome struct {
	Proof   DestructiveProof
	Stopped bool
}

// StopProven is the one way a spawned agent's place is stopped
// (rfcs/agent-container.md): destructive proof first, then Container.Stop,
// then the ownership record and the pane's runtime state are forgotten so a
// reused pane cannot be proved again. Without proof it touches nothing; a
// proven place that does not stop comes back with Stopped false. A non-empty
// want pins the place: a proof for any other pane is refused (pane_mismatch)
// and nothing is stopped.
func (s *SpawnService) StopProven(proc ProcessControl, p *proto.Peer, want string) StopOutcome {
	proof := s.DestructivePaneProof(proc, p)
	if proof.OK && want != "" && proof.PaneID != want {
		return StopOutcome{Proof: DestructiveProof{PaneID: proof.PaneID, TmuxSession: proof.TmuxSession, ErrCode: "pane_mismatch",
			Hint: "The proven pane is not the one this stop was for; refusing."}}
	}
	if !proof.OK || proof.PaneID == "" || !s.Container().Stop(proof.PaneID) {
		return StopOutcome{Proof: proof}
	}
	s.Ownership().Forget(proof.PaneID)
	hooks.ClearPaneRuntimeState(proof.PaneID)
	return StopOutcome{Proof: proof, Stopped: true}
}
