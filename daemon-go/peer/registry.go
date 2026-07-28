package peer

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/proto"
)

// Liveness probes whether a recorded agent process is still running. Injected so
// the registry stays testable; the production impl in main shells out to the OS
// (syscall.Kill(pid, 0)).
type Liveness interface {
	PIDAlive(pid int) bool
}

// Transport is the subset of the WebSocket hub the registry needs to reconcile
// liveness and sever a retired peer's socket. Injected; the hub implements it.
type Transport interface {
	IsConnected(proto.PeerID) bool
	Close(proto.PeerID) error
	// Ping probes a CONNECTED peer's self-report (pong.pane_alive) used by
	// demoteUnsafeConnectedPeers. Best-effort: timeout/error is inconclusive.
	Ping(ctx context.Context, id proto.PeerID, timeout time.Duration) (map[string]any, error)
}

// ProcessProbe resolves process ancestry and a tmux pane's root pid for the
// destructive pane-claim proof: it tells a process that genuinely runs in a pane
// from a subprocess that merely inherited TMUX_PANE. Injected like Liveness;
// production shells out to ps/tmux, tests fake it. Best-effort by contract — an
// unprobeable result (ok=false) lets a claim through, matching the Python guard's
// safe default. A nil probe on the Registry behaves the same way.
type ProcessProbe interface {
	// Ancestors returns the ancestor pids of pid (excluding itself). ok=false when
	// the process table is unprobeable (inconclusive).
	Ancestors(pid int) (map[int]struct{}, bool)
	// PaneRootPID returns the tmux pane's root process pid. ok=false when the pane
	// is unprobeable.
	PaneRootPID(paneID string) (int, bool)
}

// paneHeartbeatTolerance bounds how stale a peer's LastSeen may be before it is
// no longer "live" for pane-ownership decisions.
// ponytail: 2× the 30s default heartbeat (Python heartbeat_tolerance). Not yet
// config-driven — wire from DaemonConfig.heartbeat_interval when config lands.
const paneHeartbeatTolerance = 60 * time.Second

// AllocateParams carries everything allocate_and_register needs. Identity-
// sensitive routing only ever flows through proto.PeerID (ClaimedPeerID); the
// human-facing DisplayName is derived, never an input key for routing.
type AllocateParams struct {
	Circle        string
	Backend       proto.AgentType
	Model         *string
	Path          *string
	PaneID        *string
	TmuxSession   *string
	Machine       string
	Role          proto.PeerRole
	ClaimedPeerID *proto.PeerID
	Metadata      map[string]any
	AgentPID      *int
	// TurnState, when supplied, is applied to the peer on both fresh registration
	// and same-id reconnect (parity with the Python initial turn_state). nil leaves
	// the peer's turn_state untouched on reconnect / zero on fresh.
	TurnState *proto.TurnState
	// ParentPID is the registering hook's parent process id (SessionStart hooks
	// send it). Used only by the direct-child pane-hijack guard: a claimant whose
	// parent_pid is the live pane holder's agent_pid is a subprocess inheriting
	// TMUX_PANE, and is rejected.
	ParentPID *int
}

// ErrPeerRetired is returned by AllocateAndRegister when a claim names a retired
// peer_id without proof of a live agent — an orphan ws-hook trying to resurrect
// a terminally-offlined identity.
var ErrPeerRetired = fmt.Errorf("peer: retired peer_id cannot be reclaimed without a live agent")

// ErrPaneHijackRejected is returned when a fresh SessionStart claims a pane held
// by a live peer whose agent is the claimant's parent process — a subprocess
// inheriting TMUX_PANE trying to register on its parent's pane. A hard rejection
// (mapped to 409) so the hook leaves the incumbent untouched.
var ErrPaneHijackRejected = fmt.Errorf("peer: pane claim rejected (direct-child hijack)")

// peerState pairs the wire-facing proto.Peer with its authoritative lifecycle
// state. The FSM state is the source of truth; proto.Peer.Status is a projection
// kept in lockstep via Apply -> ToStatus, never assigned independently.
type peerState struct {
	peer  *proto.Peer
	state LifecycleState
}

// Registry is the lifecycle heart: peer state keyed by PeerID, a separate
// DisplayName index for addressing, durable mappings, retirement records, and
// demand-driven lazy_repair. All routing-sensitive lookups use PeerID.
type Registry struct {
	mu              sync.RWMutex
	peers           map[proto.PeerID]*peerState
	mappings        map[proto.PeerID]*proto.SessionMapping
	mappingsDirty   bool
	mappingsVersion uint64
	retired         map[proto.PeerID]time.Time

	store     Store
	live      Liveness
	transport Transport
	proc      ProcessProbe // optional; nil → pane-claim proof defaults to "let through"

	repairMu   sync.Mutex
	lastRepair time.Time

	// redeliverActive single-flights stashed-reply redelivery per asker. Multiple
	// triggers (register + reconnect + OFFLINE->live status) can fire concurrently
	// for the same asker; without this, two workers snapshot the same un-cleared
	// stash and BOTH send it (snapshot happens before MarkPendingReplyDelivered).
	redeliverMu     sync.Mutex
	redeliverActive map[proto.PeerID]struct{}

	retiredTTL        time.Duration
	reapTTL           time.Duration
	heartbeatInterval time.Duration
	descriptionTTL    time.Duration
	descriptionSetAt  map[proto.PeerID]time.Time

	// rec holds the lifecycle-reconciliation seams + state (AskTracker,
	// PaneProbe, PeerDelivery, strike counters, contradiction dedup).
	rec *reconcileState

	// evlog is the in-memory dashboard event buffer (last 500, mirrors the
	// Python EventLog).
	evlog *eventBuffer

	// OnOffline is a hook the hub sets so a terminal/transport offline can
	// cascade query cancellation (the hub owns the QueryTracker). The registry
	// stays query-agnostic. Called with the registry lock released.
	OnOffline func(proto.PeerID)

	// OnTerminalOffline observes conclusive executor death after transport
	// teardown and query cancellation. The daemon wires this to durable-job
	// completion so assigned work cannot remain delivered/running forever.
	// Called with the registry lock released.
	OnTerminalOffline func(proto.PeerID, string)

	// closeMu/closed/wg track detached goroutines spawned via spawnTracked
	// (redelivery, LazyRepairAsync) so Close can join them before the caller
	// closes the underlying Store. See spawnTracked and Close.
	closeMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

const (
	defaultRetiredTTL = 72 * time.Hour
	defaultReapTTL    = 30 * time.Minute // spike value; config later
	repairDebounce    = 30 * time.Second
)

// NewRegistry hydrates the in-memory state from the Store: live mappings plus
// retirement records still inside the TTL window.
func NewRegistry(ctx context.Context, store Store, live Liveness, transport Transport) (*Registry, error) {
	r := &Registry{
		peers:             make(map[proto.PeerID]*peerState),
		mappings:          make(map[proto.PeerID]*proto.SessionMapping),
		retired:           make(map[proto.PeerID]time.Time),
		redeliverActive:   make(map[proto.PeerID]struct{}),
		store:             store,
		live:              live,
		transport:         transport,
		retiredTTL:        defaultRetiredTTL,
		reapTTL:           defaultReapTTL,
		heartbeatInterval: defaultHeartbeatInterval,
		descriptionTTL:    15 * time.Minute,
		descriptionSetAt:  make(map[proto.PeerID]time.Time),
		rec: &reconcileState{
			paneStrikes:   make(map[proto.PeerID]int),
			contraEmitted: make(map[contraKey]struct{}),
		},
		evlog: &eventBuffer{},
	}

	mappings, err := store.LoadMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("load mappings: %w", err)
	}
	for _, m := range mappings {
		r.mappings[m.SessionID] = m
	}

	cutoff := time.Now().UTC().Add(-r.retiredTTL)
	retired, err := store.LoadRetired(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("load retired: %w", err)
	}
	for id, at := range retired {
		r.retired[id] = at
	}
	return r, nil
}

// ConfigureDurations applies the daemon's liveness/read-repair TTLs.
func (r *Registry) ConfigureDurations(heartbeatInterval, reapTTL, descriptionTTL time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if heartbeatInterval > 0 {
		r.heartbeatInterval = heartbeatInterval
	}
	r.reapTTL = reapTTL
	r.descriptionTTL = descriptionTTL
}

func (r *Registry) applyDescriptionTTLLocked(peer *proto.Peer) {
	if peer == nil || peer.Description == "" || r.descriptionTTL <= 0 {
		return
	}
	setAt, ok := r.descriptionSetAt[peer.PeerID]
	if !ok {
		r.descriptionSetAt[peer.PeerID] = time.Now().UTC()
		return
	}
	if time.Since(setAt) < r.descriptionTTL {
		return
	}
	peer.Description = ""
	delete(r.descriptionSetAt, peer.PeerID)
	if mapping := r.mappings[peer.PeerID]; mapping != nil {
		mapping.Description = ""
		mapping.UpdatedAt = time.Now().UTC()
		r.markMappingsDirtyLocked()
	}
}

// AllocateAndRegister allocates (or reclaims) a peer identity and registers it
// ONLINE. Returns the canonical PeerID and the assigned DisplayName.
func (r *Registry) AllocateAndRegister(ctx context.Context, params AllocateParams) (proto.PeerID, proto.DisplayName, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// (a) Retirement guard. A claim naming a retired peer_id is an orphan ws-hook
	// reconnect unless it proves a live agent. Checked against `retired` (not
	// `peers`) so it covers ids already evicted from the registry.
	if params.ClaimedPeerID != nil {
		if _, isRetired := r.retired[*params.ClaimedPeerID]; isRetired {
			if params.AgentPID == nil || !r.live.PIDAlive(*params.AgentPID) {
				return "", "", ErrPeerRetired
			}
			r.unretireLocked(ctx, *params.ClaimedPeerID)
		}
	}

	now := time.Now().UTC()

	// (b) Name-collision reclaim: reuse an existing identity when the runtime
	// peer_id matches, or an existing peer holding the target display_name is
	// Offline (clean takeover).
	displayName := r.buildDisplayName(params)

	var id proto.PeerID
	if params.ClaimedPeerID != nil {
		cid := *params.ClaimedPeerID
		claimPath := ""
		if params.Path != nil {
			claimPath = *params.Path
		}
		// Reuse a claimed peer_id ONLY when it still describes the same identity:
		// same backend and a compatible path. Stale pane/cert metadata from another
		// workspace or backend must not take over a live id — that reintroduces the
		// misbind class one level above routing. Mirrors PeerRegistry's stale-claim
		// guards (live peer + persisted mapping). On mismatch, fall through to
		// reclaim/mint.
		if ps, ok := r.peers[cid]; ok {
			if claimMatchesIdentity(ps.peer.Backend, ps.peer.Path, params.Backend, claimPath) {
				id = cid
			} else {
				log.Printf("repowire: ignoring stale peer_id claim %s (existing=%s backend=%s/%s path=%q/%q)",
					cid, ps.peer.DisplayName, ps.peer.Backend, params.Backend, ps.peer.Path, claimPath)
			}
		} else if m, ok := r.mappings[cid]; ok {
			mPath := ""
			if m.Path != nil {
				mPath = *m.Path
			}
			if claimMatchesIdentity(m.Backend, mPath, params.Backend, claimPath) {
				id = cid
			} else {
				log.Printf("repowire: ignoring stale persisted peer_id claim %s (mapping=%s backend=%s/%s path=%q/%q)",
					cid, m.DisplayName, m.Backend, params.Backend, mPath, claimPath)
			}
		}
	}
	if id == "" {
		if reclaimed, ok := r.reclaimableOfflineLocked(displayName, params.Circle, params.Backend); ok {
			id = reclaimed
		}
	}

	// (b2) Durable-mapping adoption by identity. After a restart r.mappings is
	// hydrated but r.peers is empty, and a SessionStart whose hook metadata lacks
	// peer_id would otherwise mint a NEW id and lose the peer's durable role/circle/
	// description. Reuse the persisted mapping keyed on identity, not peer_id, so
	// the (e) mapping-wins block restores those fields. Parity with
	// PeerRegistry._find_or_allocate_mapping.
	if id == "" {
		if adopted, ok := r.findReusableMappingLocked(displayName, params.Circle, params.Backend); ok {
			id = adopted
		}
	}

	// (c) Mint a fresh peer_id when not reclaiming/adopting.
	if id == "" {
		id = proto.PeerID(fmt.Sprintf("repow-%s-%s", params.Circle, uuid.NewString()[:8]))
	}

	// (d) In-place reconnect when the id names a LIVE peer. Update liveness only;
	// PRESERVE role/circle/display_name/path/description and MERGE metadata. A
	// reconnect frame that omits role/circle/metadata must not demote, relocate,
	// or strip the peer (parity with PeerRegistry same-id reconnect — otherwise an
	// orchestrator silently becomes role=agent on its next SessionStart).
	if existing, isLive := r.peers[id]; isLive {
		// A (re)register means the hook just (re)started: reset to ONLINE, parity
		// with PeerRegistry assigning initial_status on reconnect. Stale BUSY must
		// not survive a reconnect (a genuine in-progress turn re-reports BUSY via
		// the next UserPromptSubmit); the pane-handoff initial-OFFLINE case is
		// applied afterward by the route's post-register MarkOffline.
		target := existing.state
		switch existing.state {
		case StateOffline:
			if n, err := Apply(StateOffline, EventReconnect); err == nil {
				target = n
			}
		case StateBusy:
			if n, err := Apply(StateBusy, EventStop); err == nil {
				target = n
			}
		}
		existing.state = target
		if status, ok := target.ToStatus(); ok {
			existing.peer.Status = status
		}
		existing.peer.LastSeen = &now
		if params.Model != nil {
			existing.peer.Model = params.Model
		}
		if params.PaneID != nil {
			existing.peer.PaneID = params.PaneID
			// Displace any OTHER peer still holding this pane (stale after a
			// restart in the same tmux pane), parity with PeerRegistry._release_pane.
			if *params.PaneID != "" {
				r.releasePaneLocked(ctx, *params.PaneID, id, now)
			}
		}
		if params.TmuxSession != nil {
			existing.peer.TmuxSession = params.TmuxSession
		}
		if params.Machine != "" && params.Machine != "unknown" {
			existing.peer.Machine = params.Machine
		}
		if params.AgentPID != nil {
			existing.peer.AgentPID = params.AgentPID
		}
		if params.TurnState != nil {
			existing.peer.TurnState = *params.TurnState
		}
		if len(params.Metadata) > 0 {
			merged := make(map[string]any, len(existing.peer.Metadata)+len(params.Metadata))
			for k, v := range existing.peer.Metadata {
				merged[k] = v
			}
			for k, v := range params.Metadata {
				merged[k] = v
			}
			existing.peer.Metadata = merged
		}
		if m := r.mappings[id]; m != nil {
			m.UpdatedAt = now
			if params.Model != nil {
				m.Model = params.Model
			}
			if params.AgentPID != nil {
				m.AgentPID = params.AgentPID
			}
			r.markMappingsDirtyLocked()
		}
		r.appendEvent(ctx, Event{Type: "peer_online", Timestamp: now, PeerID: id, PeerName: existing.peer.DisplayName, SessionID: id})
		r.scheduleRedelivery(ctx, id)
		return id, existing.peer.DisplayName, nil
	}

	// (d.5) Pane-claim evidence gates — a FRESH registration (the id is not a live
	// peer) claiming a pane. Env inheritance (TMUX_PANE) is NOT proof of running in
	// the pane. Mirrors PeerRegistry: (1) reject a direct-child hijack hard; (2)
	// never displace a sticky orchestrator; (3) require proof before taking a live
	// holder's pane, else register pane-less. Pane ownership transfer is destructive
	// — fail loud (event) and keep the incumbent rather than silently stealing.
	effectivePane := params.PaneID
	if effectivePane != nil && *effectivePane != "" {
		pane := *effectivePane
		// (1) Direct-child hijack: claimant's parent_pid IS the live holder's agent.
		if params.ParentPID != nil {
			for _, ps := range r.peers {
				if ps.peer.PaneID == nil || *ps.peer.PaneID != pane {
					continue
				}
				if ps.peer.AgentPID == nil || *ps.peer.AgentPID != *params.ParentPID {
					continue
				}
				connected := r.transport != nil && r.transport.IsConnected(ps.peer.PeerID)
				if !recentlySeen(ps.peer, now) && !connected {
					continue
				}
				r.appendEvent(ctx, Event{Type: "pane_claim_rejected", Timestamp: now, PeerID: ps.peer.PeerID, PeerName: ps.peer.DisplayName, SessionID: ps.peer.PeerID,
					Payload: map[string]any{"pane_id": pane, "holder_peer_id": string(ps.peer.PeerID), "claimant_parent_pid": *params.ParentPID, "holder_agent_pid": *ps.peer.AgentPID, "outcome": "registration_rejected"}})
				return "", "", fmt.Errorf("%w: pane %s held by %s (%s); claimant parent_pid=%d matches holder agent_pid=%d",
					ErrPaneHijackRejected, pane, ps.peer.DisplayName, ps.peer.PeerID, *params.ParentPID, *ps.peer.AgentPID)
			}
		}
		// (2) Sticky orchestrator: reuse the configured orchestrator workspace
		// identity; otherwise do not displace and register pane-less.
		if h := r.isFreshOrchestratorPaneLocked(pane, now); h != nil {
			if h.peer.Backend == params.Backend && h.peer.Circle == params.Circle {
				h.peer.LastSeen = &now
				if params.Model != nil {
					h.peer.Model = params.Model
				}
				if params.TurnState != nil {
					h.peer.TurnState = *params.TurnState
				}
				return h.peer.PeerID, h.peer.DisplayName, nil
			}
			log.Printf("repowire: sticky orchestrator %s (%s) holds pane %s; registering claimant pane-less",
				h.peer.DisplayName, h.peer.PeerID, pane)
			effectivePane = nil
		}
	}
	// (3) Destructive pane-claim proof against a live holder.
	if effectivePane != nil && *effectivePane != "" {
		pane := *effectivePane
		if holder := r.livePaneHolderLocked(pane, now); holder != nil {
			if proven, why := r.paneClaimProvenLocked(pane, holder.peer, params.AgentPID); !proven {
				log.Printf("repowire: rejecting pane claim for %s: held by live %s (%s); %s — registering pane-less",
					pane, holder.peer.DisplayName, holder.peer.PeerID, why)
				r.appendEvent(ctx, Event{Type: "pane_claim_rejected", Timestamp: now, PeerID: holder.peer.PeerID, PeerName: holder.peer.DisplayName, SessionID: holder.peer.PeerID,
					Payload: map[string]any{"pane_id": pane, "holder_peer_id": string(holder.peer.PeerID), "reason": why, "outcome": "registered_pane_less"}})
				effectivePane = nil
			}
		}
	}

	// (e) Fresh registration, or reclaim of an OFFLINE/evicted id. Restore the
	// durable fields (role/circle/model) from a persisted mapping when the request
	// omits them, so reclaiming a known id never silently demotes role or moves
	// circle.
	next, err := Apply(StateUnregistered, EventConnect)
	if err != nil {
		// Unreachable for a well-formed FSM; fail loud rather than paper over.
		r.emitContradiction(ctx, id, displayName, "Unregistered", EventConnect)
		return "", "", fmt.Errorf("allocate: %w", err)
	}
	status, _ := next.ToStatus()

	// When a persisted mapping exists for this id (a RECLAIM of a known identity —
	// daemon restart or offline takeover), the mapping is the durable source of
	// truth for circle/role/description/display_name: it WINS over the caller's
	// per-transport default ("global" for HTTP, "default" for ws), so a reclaim
	// never silently demotes role, moves circle, or drops the description. model:
	// caller wins, else mapping. No mapping → a truly fresh mint uses the caller's
	// values. Parity with PeerRegistry fresh-creation restore.
	role, circle, model, description := params.Role, params.Circle, params.Model, ""
	if m := r.mappings[id]; m != nil {
		role, circle, description, displayName = m.Role, m.Circle, m.Description, m.DisplayName
		if model == nil {
			model = m.Model
		}
	}

	p := &proto.Peer{
		PeerID:      id,
		DisplayName: displayName,
		Backend:     params.Backend,
		Circle:      circle,
		Role:        role,
		Status:      status,
		Model:       model,
		PaneID:      effectivePane,
		TmuxSession: params.TmuxSession,
		Machine:     params.Machine,
		Metadata:    params.Metadata,
		Description: description,
		AgentPID:    params.AgentPID,
		LastSeen:    &now,
	}
	if params.TurnState != nil {
		p.TurnState = *params.TurnState
	}
	if params.Path != nil {
		p.Path = *params.Path
	}
	r.peers[id] = &peerState{peer: p, state: next}

	// A proven pane claim displaces any other (non-sticky) peer still holding it.
	if effectivePane != nil && *effectivePane != "" {
		r.releasePaneLocked(ctx, *effectivePane, id, now)
	}

	// Persist mapping in-memory; disk flush is deferred to lazy_repair.
	r.mappings[id] = &proto.SessionMapping{
		SessionID:   id,
		DisplayName: displayName,
		Circle:      circle,
		Backend:     params.Backend,
		Path:        params.Path,
		Role:        role,
		UpdatedAt:   now,
		Description: description,
		Model:       model,
		AgentPID:    params.AgentPID,
	}
	r.markMappingsDirtyLocked()

	r.appendEvent(ctx, Event{Type: "peer_online", Timestamp: now, PeerID: id, PeerName: displayName, SessionID: id})
	r.scheduleRedelivery(ctx, id)
	return id, displayName, nil
}

// scheduleRedelivery drains any stashed replies owed to a just-(re)registered
// asker — same-id reconnect (pass-1) and fresh-id identity-tuple rebind (pass-2).
// Safe to call while holding r.mu: it launches an async goroutine that re-acquires
// the lock. Overlapping calls (e.g. register + a later UpdateStatus in the same
// flow) are made safe by the per-asker single-flight in redeliverPendingReplies
// (claimRedelivery), not by snapshotting alone. No tracker → no-op.
func (r *Registry) scheduleRedelivery(ctx context.Context, id proto.PeerID) {
	if rec := r.rec; rec.asks != nil {
		// Detach from the triggering request/WS context: a one-shot register or
		// status request returns immediately, and a canceled ctx would strand the
		// owed reply (the stash stays in place until some later reconnect). Keep
		// any context values for tracing, drop cancellation. (Python's
		// asyncio.create_task is likewise not bound to the request scope.)
		r.spawnTracked(func() { r.redeliverPendingReplies(context.WithoutCancel(ctx), id) })
	}
}

// spawnTracked runs f in a background goroutine that Close can join. No-op
// after Close (the goroutine is simply never started).
//
// The closed-gate is load-bearing, not decorative: srv.Shutdown stops accepting
// new connections but does not drain already-hijacked WS connections (the read
// loop owns the socket once the handshake completes), so a live WS handler can
// still call into the registry and spawn a tracked goroutine WHILE Close() is
// running. Without the gate that races either a wg.Add after Wait has already
// returned (sync.WaitGroup panics on that) or an untracked goroutine that
// outlives Close() and hits store.Close() mid-flight.
func (r *Registry) spawnTracked(f func()) {
	r.closeMu.Lock()
	if r.closed {
		r.closeMu.Unlock()
		return
	}
	r.wg.Add(1)
	r.closeMu.Unlock()
	go func() {
		defer r.wg.Done()
		f()
	}()
}

// LazyRepairAsync is the tracked async form of LazyRepair. Every fire-and-forget
// call site (health, /ws, /events, messaging routes) must spawn through this,
// not a bare `go r.LazyRepair(...)`, so Close can join the goroutine before the
// caller closes the Store.
func (r *Registry) LazyRepairAsync(ctx context.Context) {
	r.spawnTracked(func() { r.LazyRepair(ctx) })
}

// Close joins every goroutine spawned via spawnTracked (redelivery,
// LazyRepairAsync). Call after stopping the dispatch loops and PeerDelivery,
// before store.Close() — a tracked goroutine racing a closed SQLite store would
// panic or error mid-write.
func (r *Registry) Close() {
	r.closeMu.Lock()
	r.closed = true
	r.closeMu.Unlock()
	r.wg.Wait()
	r.persistMappings(context.Background())
}

// claimRedelivery returns true if the caller won the single-flight slot for this
// asker (and must releaseRedelivery when done); false if a worker is already
// redelivering for this asker — the loser returns early so the same stash is
// never sent twice by overlapping workers.
func (r *Registry) claimRedelivery(id proto.PeerID) bool {
	r.redeliverMu.Lock()
	defer r.redeliverMu.Unlock()
	if _, busy := r.redeliverActive[id]; busy {
		return false
	}
	r.redeliverActive[id] = struct{}{}
	return true
}

func (r *Registry) releaseRedelivery(id proto.PeerID) {
	r.redeliverMu.Lock()
	delete(r.redeliverActive, id)
	r.redeliverMu.Unlock()
}

// claimMatchesIdentity reports whether a claimed peer_id may be reused for an
// incoming registration: same backend, and a compatible path (a path matches
// when either side is unset, mirroring the Python guard). Same rule for a live
// peer and a persisted mapping.
func claimMatchesIdentity(existingBackend proto.AgentType, existingPath string, claimBackend proto.AgentType, claimPath string) bool {
	sameBackend := existingBackend == claimBackend
	samePath := existingPath == "" || claimPath == "" || existingPath == claimPath
	return sameBackend && samePath
}

// WithProcessProbe injects the ps/tmux probe the destructive pane-claim proof
// uses. Optional: a nil probe makes the proof "let through" on the can't-decide
// path (the Python best-effort default), but production should wire it.
func (r *Registry) WithProcessProbe(p ProcessProbe) { r.proc = p }

// recentlySeen reports whether a peer's LastSeen is within heartbeat tolerance.
func recentlySeen(p *proto.Peer, now time.Time) bool {
	return p.LastSeen != nil && now.Sub(*p.LastSeen) <= paneHeartbeatTolerance
}

// livePaneHolderLocked returns the pane's holder when it is live with a
// verifiably-alive agent process, else nil. "Live" = status Online/Busy, a
// recorded agent_pid still running, and either heartbeat-fresh or transport-
// connected. A holder whose agent is gone is the legitimate pane-reuse case.
// Must hold r.mu.
func (r *Registry) livePaneHolderLocked(paneID string, now time.Time) *peerState {
	for _, ps := range r.peers {
		if ps.peer.PaneID == nil || *ps.peer.PaneID != paneID {
			continue
		}
		if ps.peer.Status != proto.StatusOnline && ps.peer.Status != proto.StatusBusy {
			continue
		}
		if ps.peer.AgentPID == nil || !r.live.PIDAlive(*ps.peer.AgentPID) {
			continue
		}
		connected := r.transport != nil && r.transport.IsConnected(ps.peer.PeerID)
		if recentlySeen(ps.peer, now) || connected {
			return ps
		}
	}
	return nil
}

// isFreshOrchestratorPaneLocked returns the live orchestrator holding paneID, or
// nil. Used to make orchestrator pane ownership sticky against displacement by a
// temporary same-pane peer. Must hold r.mu.
func (r *Registry) isFreshOrchestratorPaneLocked(paneID string, now time.Time) *peerState {
	for _, ps := range r.peers {
		if ps.peer.PaneID == nil || *ps.peer.PaneID != paneID {
			continue
		}
		if ps.peer.Role != proto.RoleOrchestrator {
			continue
		}
		if ps.peer.Status != proto.StatusOnline && ps.peer.Status != proto.StatusBusy {
			continue
		}
		if recentlySeen(ps.peer, now) {
			return ps
		}
	}
	return nil
}

// paneClaimProvenLocked decides whether a fresh claim may displace a live pane
// holder. Env inheritance is not proof: proof requires the claimant's agent
// process to actually run in the pane — it is the holder's own agent, or its
// ancestor chain reaches the pane root pid without passing through the holder's
// agent. Probes are best-effort; an inconclusive probe (or a claimant that
// reported no agent_pid) lets the claim through. Must hold r.mu.
func (r *Registry) paneClaimProvenLocked(paneID string, holder *proto.Peer, claimantPID *int) (bool, string) {
	if claimantPID == nil {
		return true, ""
	}
	if holder.AgentPID != nil && *claimantPID == *holder.AgentPID {
		return true, ""
	}
	if r.proc == nil {
		return true, ""
	}
	ancestors, ok := r.proc.Ancestors(*claimantPID)
	if !ok {
		return true, ""
	}
	if holder.AgentPID != nil {
		if _, isSubprocess := ancestors[*holder.AgentPID]; isSubprocess {
			return false, fmt.Sprintf("claimant agent_pid=%d is a subprocess of the live holder's agent_pid=%d", *claimantPID, *holder.AgentPID)
		}
	}
	paneRoot, ok := r.proc.PaneRootPID(paneID)
	if !ok {
		return true, ""
	}
	if *claimantPID == paneRoot {
		return true, ""
	}
	if _, inTree := ancestors[paneRoot]; inTree {
		return true, ""
	}
	return false, fmt.Sprintf("claimant agent_pid=%d is not in pane %s's process tree (pane_pid=%d)", *claimantPID, paneID, paneRoot)
}

// releasePaneLocked clears paneID from any peer that holds it (except newID) and
// drives that peer OFFLINE through the FSM — losing the pane means its ws-hook is
// no longer the live owner. A fresh orchestrator is never flipped or detached:
// pane ownership is sticky for it. Must hold r.mu. Mirrors PeerRegistry._release_pane.
func (r *Registry) releasePaneLocked(ctx context.Context, paneID string, newID proto.PeerID, now time.Time) {
	for sid, ps := range r.peers {
		if sid == newID || ps.peer.PaneID == nil || *ps.peer.PaneID != paneID {
			continue
		}
		isFreshOrch := ps.peer.Role == proto.RoleOrchestrator &&
			(ps.peer.Status == proto.StatusOnline || ps.peer.Status == proto.StatusBusy) &&
			recentlySeen(ps.peer, now)
		if isFreshOrch {
			log.Printf("repowire: _release_pane preserving fresh orchestrator %s (%s) on pane %s", ps.peer.DisplayName, ps.peer.PeerID, paneID)
			continue
		}
		next, err := Apply(ps.state, EventPaneDisplaced)
		if err != nil {
			r.emitContradictionLocked(ctx, ps.peer.PeerID, ps.peer.DisplayName, ps.state, EventPaneDisplaced)
			continue
		}
		ps.state = next
		if status, ok := next.ToStatus(); ok {
			ps.peer.Status = status
		}
		ps.peer.PaneID = nil
		ps.peer.LastSeen = &now
		r.appendEvent(ctx, Event{Type: "peer_offline", Timestamp: now, PeerID: ps.peer.PeerID, PeerName: ps.peer.DisplayName, SessionID: ps.peer.PeerID,
			Payload: map[string]any{"reason": "pane_displaced", "pane_id": paneID, "new_peer_id": string(newID)}})
	}
}

// buildDisplayName derives the addressing name, auto-suffixing past distinct
// LIVE peers that already hold the candidate (mirrors PeerRegistry.
// _build_display_name). Format: {folder}-{backend}, then {folder}-{N}-{backend}
// for N=2,3,… A candidate held only by an Offline peer (or by the same runtime
// session) is NOT suffixed — that peer is a clean-takeover/reconnect target and
// the base name is reclaimed by reclaimableOfflineLocked / the ClaimedPeerID
// path. Without this loop two live peers on the same path collide on one
// display_name and ask/notify addressing becomes ambiguous. Must hold lock.
func (r *Registry) buildDisplayName(params AllocateParams) proto.DisplayName {
	folder := "peer"
	if params.Path != nil && *params.Path != "" {
		folder = baseFolder(*params.Path)
	}
	base := fmt.Sprintf("%s-%s", folder, params.Backend)
	incomingSession := runtimeSessionID(params.Metadata)

	candidate := base
	for suffix := 2; ; suffix++ {
		var blocker *peerState
		for _, ps := range r.peers {
			if ps.peer.DisplayName == proto.DisplayName(candidate) && ps.peer.Circle == params.Circle {
				blocker = ps
				break
			}
		}
		if blocker == nil {
			return proto.DisplayName(candidate)
		}
		// Same logical peer reconnecting (matching runtime session) or a dead
		// peer holding the name: keep the base name — identity reuse is handled
		// by the ClaimedPeerID / reclaimableOfflineLocked path in caller.
		sameSession := incomingSession != "" && runtimeSessionID(blocker.peer.Metadata) == incomingSession
		if sameSession || blocker.state == StateOffline {
			return proto.DisplayName(candidate)
		}
		// Held by a distinct live peer — try the next suffix.
		candidate = fmt.Sprintf("%s-%d-%s", folder, suffix, params.Backend)
	}
}

// runtimeSessionID extracts the runtime (hook) session id from a peer's
// metadata. Mirrors PeerRegistry._runtime_session_id: two peers can't share a
// runtime session (except transiently during a fork), so it is a stable
// reconnect identity for name reclaim.
func runtimeSessionID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	for _, key := range []string{"hook_session_id", "runtime_session_id", "session_id"} {
		if v, ok := metadata[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// reclaimableOfflineLocked returns a PeerID whose Offline peer currently holds
// the given (display_name, circle, backend) — a clean takeover candidate.
func (r *Registry) reclaimableOfflineLocked(name proto.DisplayName, circle string, backend proto.AgentType) (proto.PeerID, bool) {
	for id, ps := range r.peers {
		if ps.peer.DisplayName == name &&
			ps.peer.Circle == circle &&
			ps.peer.Backend == backend &&
			ps.state == StateOffline {
			return id, true
		}
	}
	return "", false
}

// findReusableMappingLocked adopts a persisted mapping only for the same
// display name, circle, and backend. A missing circle must not silently restore
// the peer into an older circle.
func (r *Registry) findReusableMappingLocked(name proto.DisplayName, circle string, backend proto.AgentType) (proto.PeerID, bool) {
	for sid, m := range r.mappings {
		if m.DisplayName == name && m.Circle == circle && m.Backend == backend {
			return sid, true
		}
	}
	return "", false
}

// MarkOffline drives the peer offline. terminal=true retires the identity, severs
// its transport, and records the retirement so an orphan ws-hook cannot
// resurrect it. Returns the number of cancelled queries (via OnOffline).
func (r *Registry) MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error) {
	return r.MarkOfflineWithReason(ctx, id, terminal, "terminal_offline")
}

// MarkOfflineWithReason is MarkOffline with a truthful terminal cause for
// lifecycle observers. Non-terminal callers may pass an empty reason.
func (r *Registry) MarkOfflineWithReason(ctx context.Context, id proto.PeerID, terminal bool, reason string) (int, error) {
	r.mu.Lock()
	ps, ok := r.peers[id]
	if !ok {
		// Terminal offline for an id already evicted must still retire it, or the
		// orphan it came from could re-register through a persisted mapping.
		if terminal {
			r.retireLocked(ctx, id)
		}
		r.mu.Unlock()
		return 0, nil
	}

	event := EventTransportDisconnect
	if terminal {
		event = EventTerminalOffline
	}
	next, err := Apply(ps.state, event)
	if err != nil {
		r.emitContradiction(ctx, id, ps.peer.DisplayName, ps.state, event)
		r.mu.Unlock()
		return 0, nil // fail loud (event emitted), leave state unchanged
	}
	now := time.Now().UTC()
	ps.state = next
	if status, ok := next.ToStatus(); ok {
		ps.peer.Status = status
	}
	ps.peer.LastSeen = &now

	if terminal {
		r.retireLocked(ctx, id)
	}
	name := ps.peer.DisplayName
	r.mu.Unlock()

	if terminal && r.transport != nil {
		_ = r.transport.Close(id)
	}
	evType := "peer_offline"
	r.appendEvent(ctx, Event{Type: evType, Timestamp: now, PeerID: id, PeerName: name, SessionID: id})

	cancelled := 0
	if r.OnOffline != nil {
		r.OnOffline(id)
	}
	if terminal && r.OnTerminalOffline != nil {
		r.OnTerminalOffline(id, reason)
	}
	return cancelled, nil
}

// UpdateStatus applies a wire status frame through the FSM (e.g. a Stop hook
// reporting Online, a UserPromptSubmit reporting Busy). The status frame names a
// target PeerStatus; we translate it to the matching lifecycle event so illegal
// moves still fail loud.
func (r *Registry) UpdateStatus(ctx context.Context, id proto.PeerID, status proto.PeerStatus) error {
	r.mu.Lock()
	ps, ok := r.peers[id]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	event, ok := statusToEvent(status)
	if !ok {
		r.mu.Unlock()
		return nil
	}
	old := ps.state
	next, err := Apply(ps.state, event)
	if err != nil {
		r.emitContradiction(ctx, id, ps.peer.DisplayName, ps.state, event)
		r.mu.Unlock()
		return nil
	}
	now := time.Now().UTC()
	ps.state = next
	if s, ok := next.ToStatus(); ok {
		ps.peer.Status = s
	}
	ps.peer.LastSeen = &now
	name := ps.peer.DisplayName
	r.mu.Unlock()

	r.appendEvent(ctx, Event{Type: "peer_status", Timestamp: now, PeerID: id, PeerName: name, SessionID: id, Payload: map[string]any{"status": string(status)}})

	// OFFLINE->(ONLINE|BUSY) drains any stashed replies owed to this asker.
	// Redelivery is NOT ACP-specific (it was wrongly gated on the experiment flag,
	// which is off in production, so stashed replies never drained). Gate only on a
	// tracker being wired; no-tracker path has zero overhead.
	if old == StateOffline && (next == StateOnline || next == StateBusy) {
		r.scheduleRedelivery(ctx, id)
	}
	return nil
}

// statusToEvent maps a desired wire status onto the lifecycle event that reaches
// it from a live state. online->Stop (Busy->Online or Online->Online),
// busy->UserPromptSubmit, offline->TransportDisconnect.
func statusToEvent(status proto.PeerStatus) (LifecycleEvent, bool) {
	switch status {
	case proto.StatusOnline:
		return EventStop, true
	case proto.StatusBusy:
		return EventUserPromptSubmit, true
	case proto.StatusOffline:
		return EventTransportDisconnect, true
	}
	return "", false
}

// UpdateTurnState updates per-turn progress (orthogonal to lifecycle status).
func (r *Registry) UpdateTurnState(ctx context.Context, id proto.PeerID, ts proto.TurnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ps, ok := r.peers[id]; ok {
		ps.peer.TurnState = ts
		now := time.Now().UTC()
		ps.peer.LastSeen = &now
	}
}

// SetCircle moves a peer between circles, keeping the durable mapping in sync —
// the stale-mapping bug fix: peer AND mapping under one lock.
func (r *Registry) SetCircle(ctx context.Context, id proto.PeerID, circle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps, ok := r.peers[id]
	if !ok {
		return
	}
	ps.peer.Circle = circle
	if m, ok := r.mappings[id]; ok {
		m.Circle = circle
		m.UpdatedAt = time.Now().UTC()
		r.markMappingsDirtyLocked()
	}
}

// UpdateTmuxSession refreshes the peer's runtime tmux locator after a rename.
func (r *Registry) UpdateTmuxSession(ctx context.Context, id proto.PeerID, tmuxSession string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ps, ok := r.peers[id]; ok {
		ps.peer.TmuxSession = &tmuxSession
	}
}

// UpdateDisplayName renames a peer in place, preserving PeerID and keeping the
// mapping in sync. Evicts Offline ghosts holding the same (name, backend);
// returns false if a live peer already holds the name.
func (r *Registry) UpdateDisplayName(ctx context.Context, id proto.PeerID, name proto.DisplayName) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps, ok := r.peers[id]
	if !ok {
		return false, nil
	}
	var toEvict []proto.PeerID
	for otherID, other := range r.peers {
		if otherID == id || other.peer.DisplayName != name || other.peer.Backend != ps.peer.Backend {
			continue
		}
		if other.state == StateOffline {
			toEvict = append(toEvict, otherID)
		} else {
			return false, nil
		}
	}
	for _, e := range toEvict {
		delete(r.peers, e)
	}
	ps.peer.DisplayName = name
	if m, ok := r.mappings[id]; ok {
		m.DisplayName = name
		m.UpdatedAt = time.Now().UTC()
		r.markMappingsDirtyLocked()
	}
	return true, nil
}

// clonePeer returns a value-copy snapshot safe to hand to off-lock callers: the
// struct is copied and the Metadata map cloned. Other fields are pointers the
// registry only ever REASSIGNS under r.mu (never mutates in place), so a shallow
// copy of them is a stable snapshot. MUST be called while holding r.mu. Read APIs
// return clones so hub code can't race a concurrent mutator on a live *proto.Peer.
func clonePeer(p *proto.Peer) *proto.Peer {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Metadata = maps.Clone(p.Metadata)
	return &cp
}

// GetPeer returns a peer by PeerID. Routing-sensitive callers MUST hold a
// PeerID; the compiler refuses a DisplayName here.
func (r *Registry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps, ok := r.peers[id]
	if !ok {
		return nil, false
	}
	r.applyDescriptionTTLLocked(ps.peer)
	return clonePeer(ps.peer), true
}

// GetPeerByPane returns the peer occupying a tmux pane, or (nil,false). pane_id
// is runtime addressing (a tmux %N), not routing identity — the lookup walks the
// peers and matches on PaneID. Used by the tmux-lifecycle hooks to resolve a
// dead/renamed pane back to its peer.
func (r *Registry) GetPeerByPane(pane string) (*proto.Peer, bool) {
	if pane == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ps := range r.peers {
		if ps.peer.PaneID != nil && *ps.peer.PaneID == pane {
			r.applyDescriptionTTLLocked(ps.peer)
			return clonePeer(ps.peer), true
		}
	}
	return nil, false
}

// LazyRepair is demand-driven maintenance: demote ghosts, reap dangling offline
// peers, persist mappings, prune expired retirements. Debounced to ~1x/30s and
// never run on a timer. All status changes go through Apply; an illegal move
// emits a contradiction and leaves state unchanged.
func (r *Registry) LazyRepair(ctx context.Context) {
	// lastRepair is read AND written only under repairMu, so the debounce is
	// race-free. (An unsynchronized pre-lock peek here raced the write below —
	// the TryLock is cheap enough that the peek bought nothing but a data race.)
	if !r.repairMu.TryLock() {
		return
	}
	defer r.repairMu.Unlock()
	if time.Since(r.lastRepair) < repairDebounce {
		return
	}
	r.lastRepair = time.Now()

	// Pass order mirrors Python lazy_repair. The recovery sweep inside
	// demoteDisconnected runs first (clearing connection contradictions for any
	// peer the transport reports connected), then the demote/repair/evict passes.
	r.demoteDisconnected(ctx)
	r.demoteUnsafeConnectedPeers(ctx)
	r.repairStaleBusyPeers(ctx)
	r.reapDangling(ctx)
	r.evictStalePeers(ctx)
	r.emitAndEvictExpiredStashes(ctx)
	r.persistMappings(ctx)
	r.pruneRetired(ctx)
}

// demoteDisconnected marks Online/Busy transport-owned peers OFFLINE when they
// have no live WebSocket. Two sub-passes mirror Python:
//
//   - NON-pane peers demote purely on no-WS (their inbound transport is gone).
//   - PANE-backed peers are probed for runtime evidence (agent_pid alive OR tmux
//     pane present); only a peer with NEITHER socket NOR evidence is a dead ghost.
//
// A recovery sweep runs first, clearing connection contradictions for any peer
// the transport reports connected, so a future recurrence re-emits exactly once.
//
// ACP-brokered peers (metadata["acp"] != nil, flag on) and in-process @jobs
// service peers (metadata["in_process"] != nil) are exempt from BOTH sub-passes:
// their liveness is their subprocess / the daemon itself, not a WebSocket
// (repowire#206).
func (r *Registry) demoteDisconnected(ctx context.Context) {
	if r.transport == nil {
		return
	}
	rec := r.rec
	acpFlag := rec.experiments.ACPBrokerClient

	r.mu.Lock()
	// Recovery: any peer with a live socket clears its connection contradictions.
	for id := range r.peers {
		if r.transport.IsConnected(id) {
			r.clearContradiction(id, ContradictionOnlineButNoWS)
			r.clearContradiction(id, ContradictionAgentPIDDead)
		}
	}
	exempt := func(p *proto.Peer) bool {
		if acpFlag && p.Metadata != nil && p.Metadata["acp"] != nil {
			return true
		}
		return p.Metadata != nil && p.Metadata["in_process"] != nil
	}
	var candidates []proto.PeerID
	var paneCandidates []*proto.Peer
	for id, ps := range r.peers {
		if ps.state != StateOnline && ps.state != StateBusy {
			continue
		}
		if r.transport.IsConnected(id) {
			continue
		}
		if exempt(ps.peer) {
			continue
		}
		if ps.peer.PaneID != nil && *ps.peer.PaneID != "" {
			// Value-copy snapshot: the evidence probe reads these off-lock, so it
			// must not touch the live *proto.Peer (race vs concurrent writers).
			cp := *ps.peer
			paneCandidates = append(paneCandidates, &cp)
		} else {
			candidates = append(candidates, id)
		}
	}
	r.mu.Unlock()

	// Pane-backed sub-pass: probe runtime evidence off the lock; a pane with no
	// evidence joins the demote set.
	evidence := r.runtimeEvidenceIDs(paneCandidates)
	paneDead := make(map[proto.PeerID]struct{})
	for _, p := range paneCandidates {
		if _, ok := evidence[p.PeerID]; ok {
			continue
		}
		candidates = append(candidates, p.PeerID)
		paneDead[p.PeerID] = struct{}{}
	}

	now := time.Now().UTC()
	type demoted struct {
		id     proto.PeerID
		name   proto.DisplayName
		reason string
	}
	var done []demoted
	r.mu.Lock()
	for _, id := range candidates {
		ps, ok := r.peers[id]
		if !ok {
			continue
		}
		r.emitContradictionCode(ctx, ps.peer, ContradictionOnlineButNoWS, severityError,
			"peer is "+string(ps.peer.Status)+" but has no live WebSocket connection")
		_, isPaneDead := paneDead[id]
		if isPaneDead && ps.peer.AgentPID != nil {
			r.emitContradictionCode(ctx, ps.peer, ContradictionAgentPIDDead, severityError,
				"agent pid has no runtime evidence")
		}
		next, err := Apply(ps.state, EventGhostDemote)
		if err != nil {
			r.emitContradictionLocked(ctx, id, ps.peer.DisplayName, ps.state, EventGhostDemote)
			continue
		}
		ps.state = next
		if s, ok := next.ToStatus(); ok {
			ps.peer.Status = s
		}
		ps.peer.LastSeen = &now
		reason := "no_websocket_no_pane"
		if isPaneDead {
			reason = "no_websocket_no_runtime_evidence"
		}
		done = append(done, demoted{id, ps.peer.DisplayName, reason})
	}
	r.mu.Unlock()

	for _, d := range done {
		r.appendEvent(ctx, Event{Type: "peer_offline", Timestamp: now, PeerID: d.id, PeerName: d.name, SessionID: d.id,
			Payload: map[string]any{"reason": d.reason}})
	}
}

// reapDangling removes Offline peers past the reap TTL, gated on runtime
// evidence (agent_pid liveness + binding evidence via PaneProbe). A peer WITH
// evidence is SPARED and emits offline_peer_still_has_runtime_evidence; a peer
// WITHOUT is reaped: it drives Apply(Offline,Reap)->Retired, records the
// retirement (so an orphan ws-hook cannot resurrect it), severs its transport,
// and forgets its asks. Stash-loss ordering: snapshot -> emit -> forget so
// observers see pending_reply_lost before the ask disappears.
func (r *Registry) reapDangling(ctx context.Context) {
	rec := r.rec

	r.mu.RLock()
	cutoff := time.Now().UTC().Add(-r.reapTTL)
	var stale []*proto.Peer
	for _, ps := range r.peers {
		if ps.state != StateOffline {
			continue
		}
		if ps.peer.LastSeen == nil || !ps.peer.LastSeen.Before(cutoff) {
			continue
		}
		// Value-copy snapshot: off-lock probe must not read the live peer (race),
		// and the TOCTOU guard below compares this snapshot to the current peer.
		cp := *ps.peer
		stale = append(stale, &cp)
	}
	r.mu.RUnlock()

	evidence := r.runtimeEvidenceIDs(stale)

	now := time.Now().UTC()
	type reaped struct {
		id   proto.PeerID
		name proto.DisplayName
	}
	var done []reaped
	var spared []*proto.Peer

	// Re-validate under the second lock (TOCTOU guard): a peer that flipped or
	// got a new runtime in the probe window must survive.
	r.mu.Lock()
	cutoff = time.Now().UTC().Add(-r.reapTTL)
	for _, peer := range stale {
		ps, ok := r.peers[peer.PeerID]
		if !ok || ps.state != StateOffline ||
			ps.peer.LastSeen == nil || !ps.peer.LastSeen.Before(cutoff) {
			continue
		}
		curPID, curPane := runtimeMarker(ps.peer)
		oldPID, oldPane := runtimeMarker(peer)
		if curPID != oldPID || curPane != oldPane {
			continue
		}
		if _, ok := evidence[peer.PeerID]; ok {
			spared = append(spared, clonePeer(ps.peer)) // stays in the map; emitted off-lock
			continue
		}
		next, err := Apply(ps.state, EventReap)
		if err != nil {
			r.emitContradictionLocked(ctx, peer.PeerID, ps.peer.DisplayName, ps.state, EventReap)
			continue
		}
		_ = next // peer is being removed; Retired is its terminal state
		name := ps.peer.DisplayName
		delete(r.peers, peer.PeerID)
		delete(r.mappings, peer.PeerID)
		r.clearAllContradictions(peer.PeerID)
		r.retired[peer.PeerID] = now
		done = append(done, reaped{peer.PeerID, name})
	}
	r.mu.Unlock()

	for _, peer := range spared {
		r.emitOfflineStillHasEvidence(ctx, peer, "offline_ttl_with_runtime_evidence", cutoff, r.reapTTL)
	}

	// Stash-loss ordering: snapshot -> emit -> close transport -> forget.
	if rec.asks != nil {
		var lost []StashedAsk
		for _, d := range done {
			lost = append(lost, rec.asks.SnapshotPendingRepliesForPeer(d.id)...)
		}
		for _, ask := range lost {
			r.emitPendingReplyLost(ctx, ask, "offline_ttl_reap")
		}
	}

	for _, d := range done {
		if r.transport != nil {
			_ = r.transport.Close(d.id)
		}
		if rec.asks != nil {
			rec.asks.ForgetPeer(d.id)
		}
		if err := r.store.DeleteMapping(ctx, d.id); err != nil {
			log.Printf("repowire: reap DeleteMapping failed for %s: %v", d.id, err)
		}
		if err := r.store.Retire(ctx, d.id, now); err != nil {
			log.Printf("repowire: reap Retire FAILED for %s: %v (orphan may reclaim after restart)", d.id, err)
			r.appendEvent(ctx, Event{Type: "retire_persist_failed", Timestamp: now, PeerID: d.id, SessionID: d.id,
				Payload: map[string]any{"error": err.Error(), "reason": "reap"}})
		}
		r.appendEvent(ctx, Event{Type: "peer_reaped", Timestamp: now, PeerID: d.id, PeerName: d.name, SessionID: d.id,
			Payload: map[string]any{"reason": "offline_ttl"}})
	}
}

func (r *Registry) markMappingsDirtyLocked() {
	r.mappingsDirty = true
	r.mappingsVersion++
}

// persistMappings flushes changed mappings (deferred from mutation time).
func (r *Registry) persistMappings(ctx context.Context) {
	r.mu.RLock()
	if !r.mappingsDirty {
		r.mu.RUnlock()
		return
	}
	version := r.mappingsVersion
	snapshot := make([]*proto.SessionMapping, 0, len(r.mappings))
	for _, m := range r.mappings {
		cp := *m
		snapshot = append(snapshot, &cp)
	}
	r.mu.RUnlock()
	success := true
	for _, m := range snapshot {
		if err := r.store.UpsertMapping(ctx, m); err != nil {
			log.Printf("repowire: mapping flush failed for %s: %v", m.SessionID, err)
			success = false
		}
	}
	if success {
		r.mu.Lock()
		if r.mappingsVersion == version {
			r.mappingsDirty = false
		}
		r.mu.Unlock()
	}
}

// pruneRetired drops retirement records older than the TTL.
func (r *Registry) pruneRetired(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-r.retiredTTL)
	r.mu.Lock()
	var expired []proto.PeerID
	for id, at := range r.retired {
		if !at.After(cutoff) {
			expired = append(expired, id)
		}
	}
	for _, id := range expired {
		delete(r.retired, id)
	}
	r.mu.Unlock()
	for _, id := range expired {
		if err := r.store.Unretire(ctx, id); err != nil {
			log.Printf("repowire: prune Unretire failed for %s: %v", id, err)
		}
	}
}

// --- retirement helpers (must hold lock) ---

func (r *Registry) retireLocked(ctx context.Context, id proto.PeerID) {
	at := time.Now().UTC()
	r.retired[id] = at
	if err := r.store.Retire(ctx, id, at); err != nil {
		// Fail loud: the in-memory `retired` set only protects until restart. If
		// this write is lost, an orphan ws-hook can reclaim the id on the next
		// boot via its persisted mapping. Surface it, don't swallow.
		log.Printf("repowire: TERMINAL retire persist FAILED for %s: %v (orphan may reclaim after restart)", id, err)
		r.appendEvent(ctx, Event{Type: "retire_persist_failed", Timestamp: at, PeerID: id, SessionID: id,
			Payload: map[string]any{"error": err.Error()}})
	}
}

func (r *Registry) unretireLocked(ctx context.Context, id proto.PeerID) {
	delete(r.retired, id)
	if err := r.store.Unretire(ctx, id); err != nil {
		log.Printf("repowire: unretire persist failed for %s: %v", id, err)
	}
}

// --- event/contradiction helpers ---

func (r *Registry) appendEvent(ctx context.Context, e Event) {
	if e.EventID == "" {
		e.EventID = uuid.NewString()
	}
	if err := r.store.AppendEvent(ctx, e); err != nil {
		// Fail loud: durable audit history is lost on error. Still mirror to the
		// in-memory window (the live read surface) so the dashboard isn't blinded.
		log.Printf("repowire: append event %q for %s failed: %v", e.Type, e.PeerID, err)
	}
	// Mirror the durable journal row into the in-memory dashboard window so a
	// GET /events caller (and the SSE catch-up) sees lifecycle events alongside
	// the route-emitted chat_turn/query/response events. The buffer is the live
	// read surface; the store is the durable one.
	r.evlog.appendStructured(e)
}

// emitContradiction records a fail-loud peer_contradiction when an illegal
// transition is attempted. Safe to call without the lock held.
func (r *Registry) emitContradiction(ctx context.Context, id proto.PeerID, name proto.DisplayName, from LifecycleState, event LifecycleEvent) {
	r.appendEvent(ctx, Event{
		Type:      "peer_contradiction",
		Timestamp: time.Now().UTC(),
		PeerID:    id,
		PeerName:  name,
		SessionID: id,
		Payload: map[string]any{
			"from_state": string(from),
			"event":      string(event),
			"detail":     "illegal lifecycle transition rejected",
		},
	})
}

// emitContradictionLocked is emitContradiction usable while the lock is held: it
// records the same journal row (AppendEvent does not touch registry state).
func (r *Registry) emitContradictionLocked(ctx context.Context, id proto.PeerID, name proto.DisplayName, from LifecycleState, event LifecycleEvent) {
	r.emitContradiction(ctx, id, name, from, event)
}

// baseFolder returns the trailing path component (the folder name) of a path,
// sanitized for use in a display_name identifier.
func baseFolder(path string) string {
	end := len(path)
	for end > 0 && path[end-1] == '/' {
		end--
	}
	start := end
	for start > 0 && path[start-1] != '/' {
		start--
	}
	if start == end {
		return "peer"
	}
	return sanitizeFolder(path[start:end])
}

// sanitizeFolder makes a folder name safe for a display_name identifier: any
// char outside [a-zA-Z0-9._-] becomes '-', runs of '-' collapse to one, leading/
// trailing '-' are trimmed, empty -> "peer". Mirrors PeerRegistry.
// _sanitize_folder_name so a repo path with spaces or odd chars cannot produce
// an invalid display_name on the wire (clients/routes enforce the identifier rules).
func sanitizeFolder(name string) string {
	var b strings.Builder
	prevDash := false
	for _, c := range name {
		ok := c == '.' || c == '_' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			c = '-'
		}
		if c == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(c)
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "peer"
}
