package peer

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// reconcile.go extends the Registry with the four Python-parity lazy_repair
// passes (demote-unsafe-connected, repair-stale-busy, evict-stale, expired-stash
// emission), the two-pass pending-reply redelivery on reconnect, and the
// ACP/in-process demote exemptions. It also defines the three injected seams the
// passes need: a PaneProbe (runtime evidence for offline peers), an AskTracker
// (the stash store), and a PeerDelivery (the notify path for stashed replies).
//
// ponytail: the spec names the stash store `*hub.AskTracker`, but hub imports
// peer, so a concrete hub type on the Registry would cycle. We invert the
// dependency exactly like Store: peer DEFINES the AskTracker/PeerDelivery
// interfaces here, hub IMPLEMENTS them, main WIRES them. The interface surface
// is the minimal honest subset of the Python AskTracker the reconciler calls —
// upgrade path is to grow it as new passes need more methods, never to import
// hub from peer.

// ---------------------------------------------------------------------------
// Injected seams.
// ---------------------------------------------------------------------------

// PaneProbe reports runtime evidence for an OFFLINE peer: the recorded agent_pid
// is alive OR the tmux pane still exists. It is the binding-evidence half of the
// evict/reap gate (agent_pid liveness is the PID half). Injected like
// Liveness/Transport; the production impl shells out to ps/tmux, tests fake it.
//
// Distinct from Transport.Ping: demoteUnsafeConnectedPeers probes a CONNECTED
// peer's self-report (pong.pane_alive) over the socket, NOT this probe.
type PaneProbe interface {
	// HasRuntimeEvidence reports whether the recorded agent_pid is alive OR the
	// tmux pane still exists for this peer (the offline-peer gate).
	HasRuntimeEvidence(p *proto.Peer) bool
}

// StashedAsk is the read-only projection of an AskTracker Ask the reconciler
// needs. The Ask is NEVER mutated outside AskTracker locks; the reconciler reads
// these fields and asks the tracker to mutate atomically.
type StashedAsk struct {
	CorrelationID  string
	FromPeerID     proto.PeerID // the asker (redelivery target)
	FromPeerName   proto.DisplayName
	ToPeerID       proto.PeerID // the answerer (notify "from")
	ToPeerName     proto.DisplayName
	PendingReply   *string
	PendingReplyAt *time.Time
	// Asker identity tuple captured at stash time (pass-2 rebind). Empty when the
	// asker lacked a complete stable identity; only same-id reconnect can then
	// redeliver.
	AskerIdentity *AskerIdentity
}

// AskerIdentity is the asker's stable identity tuple at stash time. Path is the
// normalized form (normalizeIdentityPath); machine is never "" or "unknown".
type AskerIdentity struct {
	DisplayName proto.DisplayName
	Circle      string
	Backend     proto.AgentType
	Path        string
	Machine     string
}

// AskTracker is the minimal subset of the Python AskTracker the reconciler uses.
// All methods are best-effort and must not panic on a missing/closed ask. The
// Ask is mutated only inside the tracker's own lock via the Mark/Rebind/Evict
// methods; the Take/Snapshot methods return immutable StashedAsk projections.
type AskTracker interface {
	// TakePendingRepliesForAsker snapshots open asks targeting this asker that
	// carry a stashed reply (pass-1 same-id reconnect). Snapshot, not drain, so a
	// failed redelivery leaves the stash for the next reconnect.
	TakePendingRepliesForAsker(asker proto.PeerID) []StashedAsk

	// TakeOrphanPendingRepliesMatching is the pure pass-2 filter: asks with a
	// stashed reply whose asker_identity matches the tuple exactly AND whose
	// from_peer_id is NOT in livePeerIDs (the original asker id is gone). No
	// liveness lookups, no uniqueness gating — the caller (Registry) owns those.
	TakeOrphanPendingRepliesMatching(t AskerIdentity, livePeerIDs map[proto.PeerID]struct{}) []StashedAsk

	// MarkPendingReplyDelivered clears a delivered stash and closes the open ask
	// with reason. newFrom rebinds from_peer_id (pass-2); pass nil for same-id.
	MarkPendingReplyDelivered(cid string, newFrom *proto.PeerID, reason string) bool

	// SnapshotPendingRepliesForPeer returns asks involving this peer that carry a
	// stashed reply (reap/evict loss emission). Does not mutate.
	SnapshotPendingRepliesForPeer(id proto.PeerID) []StashedAsk

	// SnapshotExpiredPendingReplies returns TTL-expired asks still carrying a
	// stashed reply (single owner of TTL-loss emission). Does not mutate.
	SnapshotExpiredPendingReplies() []StashedAsk

	// EvictExpired drops TTL-expired asks. includeStashed=true is the
	// registry-driven sweep (it already emitted the loss events).
	EvictExpired(includeStashed bool) int

	// ForgetPeer drops every ask involving this peer (bounds tracker memory by
	// the live peer set). Returns count dropped.
	ForgetPeer(id proto.PeerID) int
}

// PeerDelivery is the notify seam for redelivering a stashed reply to a
// reconnected asker. bypassCircle is always true here (the reply is owed to the
// asker regardless of circle membership). A ValueError/TransportError-equivalent
// returns a non-nil error and the stash is left in place.
type PeerDelivery interface {
	Notify(ctx context.Context, from proto.PeerID, to proto.PeerID, text string, bypassCircle bool) error
}

// ExperimentsConfig carries the reconciliation-relevant experiment flags.
type ExperimentsConfig struct {
	ACPBrokerClient bool
}

// ---------------------------------------------------------------------------
// New contradiction codes (mirror daemon/diagnostics.py).
// ---------------------------------------------------------------------------

const (
	ContradictionOnlineButNoWS = "ONLINE_BUT_NO_WS"
	ContradictionPaneMissing   = "PANE_MISSING"
	ContradictionAgentPIDDead  = "AGENT_PID_DEAD"

	severityError = "error"
)

// ---------------------------------------------------------------------------
// reconcileState bundles the fields reconcile.go adds to the Registry. It is
// embedded as a value on Registry (set via WithReconciliation) so the existing
// registry.go struct literal stays untouched except for one field.
// ---------------------------------------------------------------------------

// reconcileState is the additional registry state the reconciler owns. Guarded
// by the Registry mutex for the maps it shares; the contradiction set has its
// own lock so emit-once works without holding the big lock.
type reconcileState struct {
	asks        AskTracker
	delivery    PeerDelivery
	paneProbe   PaneProbe
	experiments ExperimentsConfig

	staleBusyTTL time.Duration
	evictMaxAge  time.Duration

	paneStrikes map[proto.PeerID]int

	contraMu      sync.Mutex
	contraEmitted map[contraKey]struct{}
}

type contraKey struct {
	id   proto.PeerID
	code string
}

// WithReconciliation configures the lifecycle-reconciliation seams before the
// registry begins serving requests. All args are nil-safe.
func (r *Registry) WithReconciliation(
	asks AskTracker,
	delivery PeerDelivery,
	paneProbe PaneProbe,
	exp ExperimentsConfig,
	staleBusyTTL, evictMaxAge time.Duration,
) {
	r.rec.asks = asks
	r.rec.delivery = delivery
	r.rec.paneProbe = paneProbe
	r.rec.experiments = exp
	r.rec.staleBusyTTL = staleBusyTTL
	r.rec.evictMaxAge = evictMaxAge
}

// ---------------------------------------------------------------------------
// Contradiction helpers (transition-only dedup; parallel diagnostics.py).
// ---------------------------------------------------------------------------

// emitContradictionCode emits a fail-loud peer_contradiction once per
// (peer_id, code) transition. Safe without the registry lock (the contra set has
// its own lock; appendEvent does not touch registry state).
func (r *Registry) emitContradictionCode(ctx context.Context, p *proto.Peer, code, severity, detail string) {
	rec := r.rec
	key := contraKey{id: p.PeerID, code: code}
	rec.contraMu.Lock()
	if _, seen := rec.contraEmitted[key]; seen {
		rec.contraMu.Unlock()
		return
	}
	rec.contraEmitted[key] = struct{}{}
	rec.contraMu.Unlock()
	r.appendEvent(ctx, Event{
		Type:      "peer_contradiction",
		Timestamp: time.Now().UTC(),
		PeerID:    p.PeerID,
		PeerName:  p.DisplayName,
		SessionID: p.PeerID,
		Payload:   map[string]any{"code": code, "severity": severity, "detail": detail},
	})
}

// clearContradiction forgets one contradiction so a recurrence re-emits once.
func (r *Registry) clearContradiction(id proto.PeerID, code string) {
	rec := r.rec
	rec.contraMu.Lock()
	delete(rec.contraEmitted, contraKey{id: id, code: code})
	rec.contraMu.Unlock()
}

// clearAllContradictions drops all contradiction state for a peer (on reap).
func (r *Registry) clearAllContradictions(id proto.PeerID) {
	rec := r.rec
	rec.contraMu.Lock()
	for k := range rec.contraEmitted {
		if k.id == id {
			delete(rec.contraEmitted, k)
		}
	}
	rec.contraMu.Unlock()
}

// ---------------------------------------------------------------------------
// 1. redeliverPendingReplies — two-pass stash drain on OFFLINE->live.
// ---------------------------------------------------------------------------

// redeliverPendingReplies drains stashed replies for an asker that just came
// back live. Scheduled as a background goroutine (via scheduleRedelivery) from
// fresh registration, same-id reconnect, and the UpdateStatus OFFLINE->live
// transition whenever a tracker is wired. Per-asker single-flight (claimRedelivery)
// makes overlapping triggers safe: the loser returns early so a stash is never
// sent twice. Best-effort: a failure leaves the stash for the next reconnect/sweep.
func (r *Registry) redeliverPendingReplies(ctx context.Context, asker proto.PeerID) {
	rec := r.rec
	if rec.asks == nil || rec.delivery == nil {
		return
	}

	// Single-flight per asker: register/reconnect/status can all schedule a worker
	// for the same asker concurrently. Snapshot-then-send-then-clear means two
	// overlapping workers would each send the same stash. The loser returns early;
	// the winner's snapshot already covers everything pending.
	if !r.claimRedelivery(asker) {
		return
	}
	defer r.releaseRedelivery(asker)

	// PASS 1: same-id reconnect.
	for _, ask := range rec.asks.TakePendingRepliesForAsker(asker) {
		r.deliverOneStashed(ctx, ask, asker, false)
	}

	// PASS 2: identity-tuple rebind. Read the now-live peer + compute the
	// uniqueness gate under the registry lock so the live-peer snapshot is
	// consistent with the gate decision.
	r.mu.RLock()
	ps, ok := r.peers[asker]
	if !ok {
		r.mu.RUnlock()
		return
	}
	p := ps.peer
	normPath := normalizeIdentityPath(p.Path)
	if p.DisplayName == "" || p.Circle == "" || p.Backend == "" ||
		p.Machine == "" || p.Machine == "unknown" || normPath == "" {
		r.mu.RUnlock()
		return
	}
	tuple := AskerIdentity{
		DisplayName: p.DisplayName,
		Circle:      p.Circle,
		Backend:     p.Backend,
		Path:        normPath,
		Machine:     p.Machine,
	}
	var matches []proto.PeerID
	live := make(map[proto.PeerID]struct{}, len(r.peers))
	for id, other := range r.peers {
		live[id] = struct{}{}
		if other.peer.DisplayName == tuple.DisplayName &&
			other.peer.Circle == tuple.Circle &&
			other.peer.Backend == tuple.Backend &&
			other.peer.Machine == tuple.Machine &&
			normalizeIdentityPath(other.peer.Path) == normPath {
			matches = append(matches, id)
		}
	}
	r.mu.RUnlock()

	// Uniqueness gate: an ambiguous tuple must never misroute. Refuse rebind
	// unless exactly one live peer matches and it is the asker itself.
	if len(matches) != 1 || matches[0] != asker {
		log.Printf("redeliver pass-2: ambiguous live tuple for %s (%d candidates); refusing rebind", asker, len(matches))
		return
	}

	for _, ask := range rec.asks.TakeOrphanPendingRepliesMatching(tuple, live) {
		r.deliverOneStashed(ctx, ask, asker, true)
	}
}

// deliverOneStashed delivers a single stashed reply and, on success, asks the
// tracker to atomically clear+close (and rebind from_peer_id when rebind=true).
// The Ask is never mutated outside AskTracker locks. On notify error the stash
// is left exactly as found.
func (r *Registry) deliverOneStashed(ctx context.Context, ask StashedAsk, asker proto.PeerID, rebind bool) {
	rec := r.rec
	if ask.PendingReply == nil {
		return
	}
	if err := rec.delivery.Notify(ctx, ask.ToPeerID, asker, *ask.PendingReply, true); err != nil {
		log.Printf("redeliver: %s still undeliverable to %s: %v", ask.CorrelationID, asker, err)
		return
	}
	var newFrom *proto.PeerID
	if rebind {
		newFrom = &asker
	}
	rec.asks.MarkPendingReplyDelivered(ask.CorrelationID, newFrom, "ack_with_msg")
}

// ---------------------------------------------------------------------------
// 2. demoteUnsafeConnectedPeers — 3-strike PANE_MISSING terminal demote.
// ---------------------------------------------------------------------------

// demoteUnsafeConnectedPeers pings pane-backed CONNECTED peers and 3-strikes them
// out on contradicted pane death. Three honest "pane gone" verdicts RETIRE the
// identity so the reporting ws-hook cannot reconnect it back to life. Returns the
// number of peers demoted.
func (r *Registry) demoteUnsafeConnectedPeers(ctx context.Context) int {
	rec := r.rec
	if r.transport == nil {
		return 0
	}

	r.mu.RLock()
	var targets []proto.PeerID
	for id, ps := range r.peers {
		if ps.state != StateOnline && ps.state != StateBusy {
			continue
		}
		if ps.peer.PaneID == nil || *ps.peer.PaneID == "" {
			continue
		}
		if ps.peer.Backend == proto.AgentOpenCode {
			continue
		}
		if !r.transport.IsConnected(id) {
			continue
		}
		targets = append(targets, id)
	}
	// GC strike state for peers no longer tracked.
	for id := range rec.paneStrikes {
		if _, ok := r.peers[id]; !ok {
			delete(rec.paneStrikes, id)
		}
	}
	r.mu.RUnlock()

	count := 0
	for _, id := range targets {
		pong, err := r.transport.Ping(ctx, id, time.Second)
		// Ping failure (timeout/error) is inconclusive: no strike, no recovery.
		if err != nil {
			continue
		}
		alive, present := pong["pane_alive"].(bool)
		switch {
		case present && alive:
			// Recovery: clear strike + contradiction so a future PANE_MISSING re-emits.
			delete(rec.paneStrikes, id)
			r.clearContradiction(id, ContradictionPaneMissing)
			continue
		case !present:
			// Inconclusive (hook's tmux/ps shell-out failed): neither strike nor recovery.
			continue
		}
		// alive == false: a contradicted pane death.
		strikes := rec.paneStrikes[id] + 1
		rec.paneStrikes[id] = strikes
		if strikes < proto.PaneUnsafeStrikeLimit {
			continue
		}
		delete(rec.paneStrikes, id)
		r.mu.RLock()
		ps, ok := r.peers[id]
		var peerCopy *proto.Peer
		if ok {
			peerCopy = clonePeer(ps.peer) // value snapshot — read off-lock below
		}
		r.mu.RUnlock()
		if peerCopy != nil {
			detail := "connected pane is no longer alive"
			if peerCopy.PaneID != nil {
				detail = "connected pane " + *peerCopy.PaneID + " is no longer alive"
			}
			r.emitContradictionCode(ctx, peerCopy, ContradictionPaneMissing, severityError, detail)
		}
		// Terminal: retire the identity (terminal=true).
		_, _ = r.MarkOffline(ctx, id, true)
		count++
	}

	if count > 0 {
		log.Printf("demoted %d unsafe connected peers", count)
	}
	return count
}

// ---------------------------------------------------------------------------
// 3. repairStaleBusyPeers — BUSY+working+stale -> ONLINE.
// ---------------------------------------------------------------------------

// repairStaleBusyPeers is a demand-driven fallback for interrupts/cancels where a
// backend never emits Stop/AfterAgent. It drives Busy--Stop-->Online through the
// FSM, ignoring awaiting_input and any peer with recent progress. Returns count.
func (r *Registry) repairStaleBusyPeers(ctx context.Context) int {
	rec := r.rec
	if rec.staleBusyTTL <= 0 {
		return 0
	}
	cutoff := time.Now().UTC().Add(-rec.staleBusyTTL)

	r.mu.Lock()
	type repaired struct {
		id   proto.PeerID
		name proto.DisplayName
	}
	var done []repaired
	now := time.Now().UTC()
	for id, ps := range r.peers {
		if ps.state != StateBusy || ps.peer.TurnState != proto.TurnWorking {
			continue
		}
		if ps.peer.LastSeen == nil || !ps.peer.LastSeen.Before(cutoff) {
			continue
		}
		next, err := Apply(ps.state, EventStop)
		if err != nil {
			r.emitContradictionLocked(ctx, id, ps.peer.DisplayName, ps.state, EventStop)
			continue
		}
		ps.state = next
		if s, ok := next.ToStatus(); ok {
			ps.peer.Status = s
		}
		ps.peer.TurnState = proto.TurnIdle
		ps.peer.LastSeen = &now
		done = append(done, repaired{id, ps.peer.DisplayName})
	}
	r.mu.Unlock()

	for _, d := range done {
		r.appendEvent(ctx, Event{Type: "peer_status", Timestamp: now, PeerID: d.id, PeerName: d.name, SessionID: d.id,
			Payload: map[string]any{"status": string(proto.StatusOnline), "reason": "stale_busy_repair"}})
	}
	if len(done) > 0 {
		log.Printf("repaired %d stale busy peers", len(done))
	}
	return len(done)
}

// ---------------------------------------------------------------------------
// 4. evictStalePeers + emitAndEvictExpiredStashes.
// ---------------------------------------------------------------------------

// runtimeEvidenceIDs probes paneProbe.HasRuntimeEvidence for every candidate with
// agent_pid>0 OR a pane_id, off the registry lock and in parallel (mirroring the
// Python asyncio.gather to_thread fan-out). Returns the set of peers WITH
// evidence (spared). A nil paneProbe yields the empty set (no peer spared).
func (r *Registry) runtimeEvidenceIDs(candidates []*proto.Peer) map[proto.PeerID]struct{} {
	rec := r.rec
	out := make(map[proto.PeerID]struct{})
	if rec.paneProbe == nil {
		return out
	}
	type result struct {
		id  proto.PeerID
		has bool
	}
	var wg sync.WaitGroup
	resCh := make(chan result, len(candidates))
	for _, p := range candidates {
		if !((p.AgentPID != nil && *p.AgentPID > 0) || (p.PaneID != nil && *p.PaneID != "")) {
			continue
		}
		wg.Add(1)
		go func(peer *proto.Peer) {
			defer wg.Done()
			resCh <- result{peer.PeerID, rec.paneProbe.HasRuntimeEvidence(peer)}
		}(p)
	}
	wg.Wait()
	close(resCh)
	for res := range resCh {
		if res.has {
			out[res.id] = struct{}{}
		}
	}
	return out
}

// runtimeMarker is the (agent_pid, pane_id) tuple used as the TOCTOU guard: a
// peer whose runtime changed in the probe window must survive.
func runtimeMarker(p *proto.Peer) (int, string) {
	pid := 0
	if p.AgentPID != nil {
		pid = *p.AgentPID
	}
	pane := ""
	if p.PaneID != nil {
		pane = *p.PaneID
	}
	return pid, pane
}

// evictStalePeers hard-prunes long-OFFLINE peers past prune_max_age_hours,
// gated on runtime evidence (agent_pid liveness + binding evidence). A peer WITH
// evidence is SPARED; a peer WITHOUT is EVICTED (deleted, NOT retired — that
// asymmetry vs reap is intentional). Returns count evicted.
func (r *Registry) evictStalePeers(ctx context.Context) int {
	rec := r.rec
	if rec.evictMaxAge <= 0 {
		return 0
	}

	r.mu.RLock()
	cutoff := time.Now().UTC().Add(-rec.evictMaxAge)
	var stale []*proto.Peer
	for _, ps := range r.peers {
		if ps.state != StateOffline {
			continue
		}
		if ps.peer.LastSeen == nil || !ps.peer.LastSeen.Before(cutoff) {
			continue
		}
		// Snapshot a VALUE COPY under the lock. The off-lock evidence probe must
		// not read the live *proto.Peer (data race vs allocate/reconnect/status
		// writers), and the TOCTOU guard below must compare the snapshot against
		// the CURRENT live peer — a shared pointer would make that guard a no-op.
		cp := *ps.peer
		stale = append(stale, &cp)
	}
	r.mu.RUnlock()

	evidence := r.runtimeEvidenceIDs(stale)

	// Re-validate under the second lock (TOCTOU guard).
	r.mu.Lock()
	cutoff = time.Now().UTC().Add(-rec.evictMaxAge)
	var spared, evicted []*proto.Peer
	for _, peer := range stale {
		ps, ok := r.peers[peer.PeerID]
		if !ok || ps.state != StateOffline ||
			ps.peer.LastSeen == nil || !ps.peer.LastSeen.Before(cutoff) {
			continue
		}
		curPID, curPane := runtimeMarker(ps.peer)
		oldPID, oldPane := runtimeMarker(peer)
		if curPID != oldPID || curPane != oldPane {
			continue // runtime changed in the probe window; survive
		}
		if _, ok := evidence[peer.PeerID]; ok {
			spared = append(spared, clonePeer(ps.peer)) // stays in the map; emitted off-lock
			continue
		}
		evicted = append(evicted, ps.peer) // removed from the map below; no concurrent mutator
		delete(r.peers, peer.PeerID)
		delete(r.mappings, peer.PeerID)
		r.clearAllContradictions(peer.PeerID)
	}
	r.mu.Unlock()

	for _, peer := range spared {
		r.emitOfflineStillHasEvidence(ctx, peer, "stale_evict_with_runtime_evidence", cutoff, rec.evictMaxAge)
	}

	// Stash-loss ordering: snapshot -> emit -> forget so observers see the loss
	// event before the ask disappears. evict simply deletes (no retirement).
	if rec.asks != nil {
		var lost []StashedAsk
		for _, peer := range evicted {
			lost = append(lost, rec.asks.SnapshotPendingRepliesForPeer(peer.PeerID)...)
		}
		for _, ask := range lost {
			r.emitPendingReplyLost(ctx, ask, "stale_evict")
		}
		for _, peer := range evicted {
			rec.asks.ForgetPeer(peer.PeerID)
		}
	}

	for _, peer := range evicted {
		if err := r.store.DeleteMapping(ctx, peer.PeerID); err != nil {
			log.Printf("repowire: stale-evict DeleteMapping failed for %s: %v", peer.PeerID, err)
		}
	}
	if len(evicted) > 0 {
		log.Printf("evicted %d stale offline peers", len(evicted))
	}
	if len(spared) > 0 {
		log.Printf("spared %d long-offline peers with runtime evidence", len(spared))
	}
	return len(evicted)
}

// emitAndEvictExpiredStashes is the single owner of TTL-loss emission: snapshot
// expired stashes, emit pending_reply_lost, then evict including stashed. The
// recipient-facing lazy evictor passes includeStashed=false so this path always
// wins the stashed asks.
func (r *Registry) emitAndEvictExpiredStashes(ctx context.Context) {
	rec := r.rec
	if rec.asks == nil {
		return
	}
	for _, ask := range rec.asks.SnapshotExpiredPendingReplies() {
		r.emitPendingReplyLost(ctx, ask, "ttl_evicted")
	}
	rec.asks.EvictExpired(true)
}

// emitOfflineStillHasEvidence records the spare event for an offline peer that
// still has runtime evidence (not deleted).
func (r *Registry) emitOfflineStillHasEvidence(ctx context.Context, p *proto.Peer, reason string, cutoff time.Time, ttl time.Duration) {
	var lastSeen any
	if p.LastSeen != nil {
		lastSeen = p.LastSeen.Format(time.RFC3339)
	}
	var pane any
	if p.PaneID != nil {
		pane = *p.PaneID
	}
	var pid any
	if p.AgentPID != nil {
		pid = *p.AgentPID
	}
	r.appendEvent(ctx, Event{
		Type:      "offline_peer_still_has_runtime_evidence",
		Timestamp: time.Now().UTC(),
		PeerID:    p.PeerID,
		PeerName:  p.DisplayName,
		SessionID: p.PeerID,
		Payload: map[string]any{
			"peer_id":      string(p.PeerID),
			"display_name": string(p.DisplayName),
			"backend":      string(p.Backend),
			"path":         p.Path,
			"pane_id":      pane,
			"agent_pid":    pid,
			"last_seen":    lastSeen,
			"cutoff":       cutoff.Format(time.RFC3339),
			"ttl_seconds":  ttl.Seconds(),
			"reason":       reason,
		},
	})
}

// emitPendingReplyLost emits a pointer-only pending_reply_lost event. It carries
// enough to look up the lost correlation and the answerer; never reply text,
// asker path, or asker machine.
func (r *Registry) emitPendingReplyLost(ctx context.Context, ask StashedAsk, reason string) {
	var dn, circle, backend any
	if ask.AskerIdentity != nil {
		dn = string(ask.AskerIdentity.DisplayName)
		circle = ask.AskerIdentity.Circle
		backend = string(ask.AskerIdentity.Backend)
	}
	var at any
	if ask.PendingReplyAt != nil {
		at = ask.PendingReplyAt.Format(time.RFC3339)
	}
	r.appendEvent(ctx, Event{
		Type:      "pending_reply_lost",
		Timestamp: time.Now().UTC(),
		PeerID:    ask.FromPeerID,
		PeerName:  ask.FromPeerName,
		SessionID: ask.FromPeerID,
		Payload: map[string]any{
			"correlation_id":     ask.CorrelationID,
			"answerer_peer_id":   string(ask.ToPeerID),
			"answerer_name":      string(ask.ToPeerName),
			"asker_name":         string(ask.FromPeerName),
			"asker_display_name": dn,
			"asker_circle":       circle,
			"asker_backend":      backend,
			"asker_peer_id":      string(ask.FromPeerID),
			"reason":             reason,
			"pending_reply_at":   at,
		},
	})
}

// normalizeIdentityPath is the canonical path form for identity matching
// (mirrors registry_identity.normalize_identity_path). filepath.Clean is the
// Go equivalent of os.path.normpath; symlink realpath resolution is deferred.
//
// ponytail: Python also realpath()s the symlink chain; we Clean only. Upgrade
// path is filepath.EvalSymlinks with a Clean fallback when the path doesn't
// exist on disk — deferred until a symlinked-workspace rebind is observed.
func normalizeIdentityPath(raw string) string {
	if raw == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(raw); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(raw)
}
