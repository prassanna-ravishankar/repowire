package service

// SessionControl acquires executors for durable jobs: assigned peer, reusable
// peer, backend resume, then spawn. Every acquisition records an operation.

import (
	"context"
	"path/filepath"
	"sort"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

const spawnRegistrationTimeout = 45 * time.Second

// controlRegistry is the narrow registry seam SessionControl + JobRunner call.
//
// ResolvePeerStrict returns 0/1/N candidates: an empty slice is "not found", a
// one-element slice is the unique resolution, N>1 is ambiguous. This matches the
// authoritative spawnRegistry seam shape (a list never raises for ambiguity).
type controlRegistry interface {
	ResolvePeerStrict(identifier string, circle *string) []*proto.Peer
	GetAllPeers() []*proto.Peer
	GetPeer(id proto.PeerID) (*proto.Peer, bool)
	GetPeerByPane(pane string) (*proto.Peer, bool)
	UnregisterPeer(ctx context.Context, identifier string, circle *string) (bool, error)
}

// spawnExecutor is the narrow SpawnService seam: the methods SessionControl
// drives. The concrete *SpawnService (spawn area) satisfies it directly — the
// methods + signatures match SpawnService.ResolveCommand / SpawnService.Spawn, so
// main injects the SAME shared *SpawnService instance with no adapter. A fake
// spawner keeps SessionControl tests hermetic (records calls, no tmux).
type spawnExecutor interface {
	// ResolveCommand resolves the backend (+ optional profile) launch line.
	ResolveCommand(b proto.AgentType, profile *string) (string, error)
	// Spawn performs the tmux exec + ownership record.
	Spawn(cfg SpawnConfig) (SpawnResult, error)
}

// ExecutorAcquisition is the result of AcquireExecutorForWork.
type ExecutorAcquisition struct {
	Peer           *proto.Peer
	Strategy       string // assigned_peer|reused_peer|backend_resume|spawned_peer
	OperationID    string
	ResumePlan     map[string]any
	RuntimeBinding map[string]any
	ReleaseHandle  map[string]any
}

// ExecutorAcquisitionUnavailableError carries the structured failure the runner
// records as an attempt outcome. Mirrors ExecutorAcquisitionUnavailableError.
type ExecutorAcquisitionUnavailableError struct {
	Reason         string
	OperationID    string
	Status         string // "unavailable" (default) | "failed"
	Phase          string // "resolve_peer" (default) | "spawn"
	Err            map[string]any
	AssignedPeerID string
}

func (e *ExecutorAcquisitionUnavailableError) Error() string { return e.Reason }

// SessionControl acquires a live executor for durable session/job work.
type SessionControl struct {
	reg    controlRegistry
	spawn  spawnExecutor
	store  *state.Store
	resume func(proto.AgentType, string, string, *string, map[string]any) (map[string]any, bool)
}

// NewSessionControl wires the executor-acquisition service. spawn may be nil
// (spawn-strategy acquisitions then fail loud with reason spawn_unavailable);
// resume may be nil (resume disabled → fresh spawns).
func NewSessionControl(reg controlRegistry, spawn spawnExecutor, store *state.Store) *SessionControl {
	return &SessionControl{reg: reg, spawn: spawn, store: store}
}

// WithResume attaches the shared resume-safety resolver. Returns the receiver.
func (c *SessionControl) WithResume(r func(proto.AgentType, string, string, *string, map[string]any) (map[string]any, bool)) *SessionControl {
	c.resume = r
	return c
}

// AcquireExecutorForWork walks the acquisition ladder and records a durable
// session.acquire_executor operation. target is the execution.target block;
// runnerOwnerID is recorded under provenance.requested_by. Mirrors
// acquire_executor_for_work.
func (c *SessionControl) AcquireExecutorForWork(ctx context.Context, work *state.TrackedWork, target map[string]any, runnerOwnerID string) (*ExecutorAcquisition, error) {
	policy := executionPolicy(work)
	op, err := c.store.CreateOperation(ctx, "session.acquire_executor", map[string]any{
		"work_id":             work.WorkID,
		"repowire_session_id": strOrNilAny(work.RepowireSessionID),
		"source_kind":         strOrNilAny(work.SourceKind),
		"source_id":           strOrNilAny(work.SourceID),
		"circle":              strOrNilAny(work.Circle),
		"process_scope":       policy.ProcessScope,
		"continuity":          policy.Continuity,
		"target":              target,
	}, map[string]any{"requested_by": runnerOwnerID})
	if err != nil {
		return nil, err
	}

	assigned := mapStr(target, "assigned_peer_id")
	if assigned == "" {
		assigned = strOrEmpty(work.AssignedPeerID)
	}
	if assigned != "" {
		return c.acquireAssignedPeer(ctx, work, op.OperationID, assigned)
	}

	path := mapStr(target, "path")
	backendRaw := mapStr(target, "backend")
	if path == "" || backendRaw == "" {
		c.failOp(ctx, op.OperationID, "", map[string]any{"reason": "missing_target"})
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "missing_target", OperationID: op.OperationID}
	}
	backend := proto.AgentType(backendRaw)
	if !backend.Valid() {
		c.failOp(ctx, op.OperationID, "", map[string]any{"reason": "invalid_backend"})
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "invalid_backend", OperationID: op.OperationID}
	}

	circle := strOrEmpty(work.Circle)
	if circle == "" {
		c.failOp(ctx, op.OperationID, "", map[string]any{"reason": "missing_circle"})
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "missing_circle", OperationID: op.OperationID}
	}
	if policy.ProcessScope != "per_fire" {
		if reusable := c.findLivePeer(path, backend, circle); reusable != nil {
			binding := c.recordRuntimeBinding(ctx, work, reusable, "reused_peer", "", "", "")
			_, _ = c.store.StartAttempt(ctx, op.OperationID, strPtr("reused_peer"), map[string]any{"peer_id": string(reusable.PeerID)})
			_, _ = c.store.CompleteOperation(ctx, op.OperationID, strPtr("reused_peer"), map[string]any{
				"peer_id":         string(reusable.PeerID),
				"runtime_binding": binding,
			})
			return &ExecutorAcquisition{Peer: reusable, Strategy: "reused_peer", OperationID: op.OperationID, RuntimeBinding: binding}, nil
		}
	}

	var resumePlan map[string]any
	if policy.Continuity == "resume" {
		resumePlan = c.resumePlanFor(work, path, backend)
	}
	strategy := "spawned_peer"
	if resumePlan != nil {
		strategy = "backend_resume"
	}
	_, _ = c.store.StartAttempt(ctx, op.OperationID, strPtr(strategy), map[string]any{
		"path":        path,
		"backend":     string(backend),
		"resume_plan": resumePlan,
	})

	if c.spawn == nil {
		errObj := map[string]any{"reason": "spawn_unavailable"}
		c.failOp(ctx, op.OperationID, strategy, errObj)
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "spawn_unavailable", OperationID: op.OperationID, Status: "failed", Phase: "spawn", Err: errObj}
	}

	warmup := "Repowire spawned this session for a durable job. " +
		"Please register with the mesh; the job request will arrive as an ask."
	command, cerr := c.spawn.ResolveCommand(backend, optStr(target, "profile"))
	if cerr != nil {
		errObj := map[string]any{"reason": "spawn_failed", "message": cerr.Error()}
		c.failOp(ctx, op.OperationID, strategy, errObj)
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "spawn_failed", OperationID: op.OperationID, Status: "failed", Phase: "spawn", Err: errObj}
	}
	if resumePlan != nil {
		command, cerr = BuildResumeCommand(command, backend, mapStr(resumePlan, "runtime_session_id"))
		if cerr != nil {
			errObj := map[string]any{"reason": "resume_command_unavailable", "message": cerr.Error()}
			c.failOp(ctx, op.OperationID, strategy, errObj)
			return nil, &ExecutorAcquisitionUnavailableError{Reason: "resume_command_unavailable", OperationID: op.OperationID, Status: "failed", Phase: "spawn", Err: errObj}
		}
	}
	out, err := c.spawn.Spawn(SpawnConfig{
		Path:    path,
		Circle:  circle,
		Backend: backend,
		Command: command,
		Message: &warmup,
		Role:    proto.RoleAgent,
	})
	if err != nil {
		errObj := map[string]any{"reason": "spawn_failed", "message": err.Error()}
		c.failOp(ctx, op.OperationID, strategy, errObj)
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "spawn_failed", OperationID: op.OperationID, Status: "failed", Phase: "spawn", Err: errObj}
	}

	resolved := c.awaitSpawnedPeer(ctx, out.DisplayName, circle, path, backend, out.PaneID)
	if resolved == nil {
		errObj := map[string]any{"reason": "spawned_peer_not_registered"}
		c.failOp(ctx, op.OperationID, strategy, errObj)
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "spawned_peer_not_registered", OperationID: op.OperationID, Err: errObj}
	}

	binding := c.recordRuntimeBinding(ctx, work, resolved, strategy, op.OperationID, policy.ProcessScope, policy.Continuity)
	release := releaseHandleForPeer(resolved, policy.ProcessScope, op.OperationID, strategy)
	if policy.ProcessScope == "per_fire" && release == nil {
		errObj := map[string]any{"reason": "release_handle_unavailable", "peer_id": string(resolved.PeerID)}
		c.failOp(ctx, op.OperationID, strategy, errObj)
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "release_handle_unavailable", OperationID: op.OperationID, Err: errObj}
	}
	_, _ = c.store.CompleteOperation(ctx, op.OperationID, strPtr(strategy), map[string]any{
		"peer_id":         string(resolved.PeerID),
		"tmux":            map[string]any{"tmux_session": strPtrOrNil(resolved.TmuxSession), "pane_id": strPtrOrNil(resolved.PaneID)},
		"runtime_binding": binding,
		"release_handle":  release,
	})
	return &ExecutorAcquisition{
		Peer:           resolved,
		Strategy:       strategy,
		OperationID:    op.OperationID,
		ResumePlan:     resumePlan,
		RuntimeBinding: binding,
		ReleaseHandle:  release,
	}, nil
}

func (c *SessionControl) acquireAssignedPeer(ctx context.Context, work *state.TrackedWork, operationID, assigned string) (*ExecutorAcquisition, error) {
	resolved := c.reg.ResolvePeerStrict(assigned, work.Circle)
	if len(resolved) != 1 {
		reason := "assigned_peer_not_found"
		if len(resolved) > 1 {
			reason = "ambiguous_assigned_peer"
		}
		c.failOp(ctx, operationID, "", map[string]any{"reason": reason})
		return nil, &ExecutorAcquisitionUnavailableError{Reason: reason, OperationID: operationID}
	}
	peer := resolved[0]
	if peer.Status == proto.StatusOffline {
		c.failOp(ctx, operationID, "", map[string]any{"reason": "assigned_peer_offline"})
		return nil, &ExecutorAcquisitionUnavailableError{Reason: "assigned_peer_offline", OperationID: operationID, AssignedPeerID: string(peer.PeerID)}
	}
	binding := c.recordRuntimeBinding(ctx, work, peer, "assigned_peer", "", "", "")
	_, _ = c.store.StartAttempt(ctx, operationID, strPtr("assigned_peer"), map[string]any{"peer_id": string(peer.PeerID), "release_handle": nil})
	_, _ = c.store.CompleteOperation(ctx, operationID, strPtr("assigned_peer"), map[string]any{
		"peer_id":         string(peer.PeerID),
		"runtime_binding": binding,
		"release_handle":  nil,
	})
	return &ExecutorAcquisition{Peer: peer, Strategy: "assigned_peer", OperationID: operationID, RuntimeBinding: binding}, nil
}

// ReleaseExecutorForWork releases the executor acquired for the current attempt.
// Idempotent: a missing/dead pane is recorded as already-released, a live
// mismatched peer fails closed. Mirrors release_executor_for_work but defers the
// actual pane kill to the SpawnService (KillPane is the spawn area's concern).
//
// ponytail: the pane-kill + ownership-forget half lives in the spawn area's
// TmuxController/PaneOwnership. Until that's injected here, release records the
// terminal operation and unregisters the peer, returning a structured result;
// the physical kill is wired in main once the spawn area exposes KillPane. The
// upgrade path: inject a paneReaper seam (KillPane + ProbePane + Forget) and call
// it between the mismatch check and unregister.
func (c *SessionControl) ReleaseExecutorForWork(ctx context.Context, work *state.TrackedWork, terminalReason string) (map[string]any, error) {
	runner := mapAtAny(work.Provenance, "runner")
	attemptID, _ := runner["current_attempt_id"].(string)
	attempts := anySlice(runner["attempts"])
	var attempt map[string]any
	for _, raw := range attempts {
		if m, ok := raw.(map[string]any); ok {
			if id, _ := m["attempt_id"].(string); id == attemptID {
				attempt = m
				break
			}
		}
	}
	acquisition := mapAtAny(attempt, "acquisition")
	release, ok := acquisition["release_handle"].(map[string]any)
	if !ok || release == nil {
		return map[string]any{"status": "skipped", "reason": "no_release_handle", "terminal_reason": terminalReason}, nil
	}
	if ra, _ := release["released_at"].(string); ra != "" {
		out := cloneAny(release)
		out["status"] = "already_released"
		out["terminal_reason"] = terminalReason
		return out, nil
	}

	op, err := c.store.CreateOperation(ctx, "session.release_executor", map[string]any{
		"work_id":         work.WorkID,
		"attempt_id":      attemptID,
		"terminal_reason": terminalReason,
		"release_handle":  release,
	}, map[string]any{"acquire_operation_id": release["operation_id"]})
	if err != nil {
		return nil, err
	}
	_, _ = c.store.StartAttempt(ctx, op.OperationID, strPtr("kill_pane"), map[string]any{"release_handle": release})

	paneID, _ := release["pane_id"].(string)
	peerID, _ := release["peer_id"].(string)
	if paneID == "" {
		errObj := map[string]any{"reason": "missing_pane_id", "peer_id": peerID}
		c.failOp(ctx, op.OperationID, "kill_pane", errObj)
		return map[string]any{"status": "failed", "reason": "missing_pane_id", "operation_id": op.OperationID, "terminal_reason": terminalReason, "reap_error": errObj}, nil
	}
	if peerID != "" {
		if live, ok := c.reg.GetPeer(proto.PeerID(peerID)); ok && live.PaneID != nil && *live.PaneID != paneID {
			errObj := map[string]any{"reason": "release_handle_mismatch", "peer_id": peerID, "expected_pane_id": paneID, "actual_pane_id": *live.PaneID}
			c.failOp(ctx, op.OperationID, "kill_pane", errObj)
			return map[string]any{"status": "failed", "reason": "release_handle_mismatch", "operation_id": op.OperationID, "terminal_reason": terminalReason, "reap_error": errObj}, nil
		}
	}

	// ponytail: physical kill deferred to the spawn area; unregister + record.
	// peerID is already resolved (from the release handle), so the ambiguity
	// error can't fire.
	if peerID != "" {
		_, _ = c.reg.UnregisterPeer(ctx, peerID, nil)
	}
	result := map[string]any{
		"status":          "released",
		"reason":          "pane_released",
		"operation_id":    op.OperationID,
		"terminal_reason": terminalReason,
		"peer_id":         peerID,
		"pane_id":         paneID,
		"released_at":     opReleaseNow(),
	}
	_, _ = c.store.CompleteOperation(ctx, op.OperationID, strPtr("kill_pane"), result)
	return result, nil
}

func (c *SessionControl) findLivePeer(path string, backend proto.AgentType, circle string) *proto.Peer {
	target := normalizePath(path)
	var candidates []*proto.Peer
	for _, p := range c.reg.GetAllPeers() {
		if p.Status != proto.StatusOnline && p.Status != proto.StatusBusy {
			continue
		}
		if p.Circle != circle || p.Backend != backend {
			continue
		}
		if normalizePath(p.Path) != target {
			continue
		}
		candidates = append(candidates, p)
	}
	return preferLivePeer(candidates)
}

func (c *SessionControl) awaitSpawnedPeer(ctx context.Context, displayName, circle, path string, backend proto.AgentType, paneID string) *proto.Peer {
	target := normalizePath(path)
	deadline := time.Now().Add(spawnRegistrationTimeout)
	for {
		if paneID != "" {
			if p, ok := c.reg.GetPeerByPane(paneID); ok && p.Status != proto.StatusOffline {
				return p
			}
			if time.Now().After(deadline) {
				return nil
			}
			if !sleepCtx(ctx, 250*time.Millisecond) {
				return nil
			}
			continue
		}
		if resolved := c.reg.ResolvePeerStrict(displayName, &circle); len(resolved) == 1 && resolved[0].Status != proto.StatusOffline {
			return resolved[0]
		}
		if p := c.findRegisteredSpawnedPeer(target, backend, circle); p != nil {
			return p
		}
		if time.Now().After(deadline) {
			return nil
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return nil
		}
	}
}

func (c *SessionControl) findRegisteredSpawnedPeer(normalizedPath string, backend proto.AgentType, circle string) *proto.Peer {
	var candidates []*proto.Peer
	for _, p := range c.reg.GetAllPeers() {
		if p.Status != proto.StatusOnline && p.Status != proto.StatusBusy {
			continue
		}
		if p.Circle != circle || p.Backend != backend {
			continue
		}
		if normalizePath(p.Path) != normalizedPath {
			continue
		}
		candidates = append(candidates, p)
	}
	// Prefer a peer that owns a pane, then most-recently-seen.
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i].PaneID != nil, candidates[j].PaneID != nil
		if ai != aj {
			return ai && !aj
		}
		return lastSeen(candidates[i]).After(lastSeen(candidates[j]))
	})
	if len(candidates) > 0 {
		return candidates[0]
	}
	return nil
}

func (c *SessionControl) resumePlanFor(work *state.TrackedWork, path string, backend proto.AgentType) map[string]any {
	if c.resume == nil || work.SourceKind == nil || *work.SourceKind != "calendar" || work.SourceID == nil || *work.SourceID == "" {
		return nil
	}
	entry, err := c.store.GetCalendarEntry(context.Background(), *work.SourceID)
	if err != nil || entry == nil {
		return nil
	}
	binding := mapAtAny(entry.Provenance, "runtime_binding")
	if bk, _ := binding["backend"].(string); bk != string(backend) {
		return nil
	}
	if normalizePath(mapStr(binding, "path")) != normalizePath(path) {
		return nil
	}
	circle := strOrEmpty(work.Circle)
	if circle == "" {
		return nil
	}
	if bc, _ := binding["circle"].(string); bc != "" && bc != circle {
		return nil
	}
	runtimeSessionID, _ := binding["runtime_session_id"].(string)
	if runtimeSessionID == "" {
		return nil
	}
	capability := mapAtAny(binding, "resume_capability")
	var repowireSessionID *string
	if rs, ok := binding["repowire_session_id"].(string); ok && rs != "" {
		repowireSessionID = &rs
	}
	plan, resumable := c.resume(backend, path, runtimeSessionID, repowireSessionID, capability)
	if !resumable {
		return nil
	}
	return plan
}

func (c *SessionControl) recordRuntimeBinding(ctx context.Context, work *state.TrackedWork, peer *proto.Peer, source, operationID, processScope, continuity string) map[string]any {
	binding := runtimeBindingForPeer(ctx, c.store, peer, work)
	binding["source"] = source
	binding["recorded_at"] = opReleaseNow()
	if work.SourceID != nil {
		binding["source_id"] = *work.SourceID
	}
	if work.SourceKind != nil {
		binding["source_kind"] = *work.SourceKind
	}
	if operationID != "" {
		binding["acquired_by_operation_id"] = operationID
	}
	if processScope != "" {
		binding["process_scope"] = processScope
	}
	if continuity != "" {
		binding["continuity"] = continuity
	}
	if work.SourceKind != nil && *work.SourceKind == "calendar" && work.SourceID != nil && *work.SourceID != "" {
		_, _ = c.store.UpdateCalendarRuntimeBinding(ctx, *work.SourceID, binding)
	}
	return binding
}

func (c *SessionControl) failOp(ctx context.Context, operationID, strategy string, errObj map[string]any) {
	var sp *string
	if strategy != "" {
		sp = &strategy
	}
	_, _ = c.store.FailOperation(ctx, operationID, "unavailable", sp, errObj)
}

// --- pure helpers (mirrors of the static methods in session_control.py) ---

type execPolicy struct {
	ProcessScope string
	Continuity   string
}

// executionPolicy derives process_scope + continuity from the work's execution
// block, applying the per_fire/resume defaults. Mirrors _execution_policy.
func executionPolicy(work *state.TrackedWork) execPolicy {
	execution := mapAtAny(work.Request, "execution")
	processScope, _ := execution["process_scope"].(string)
	target := mapAtAny(execution, "target")
	assigned := mapStr(target, "assigned_peer_id")
	if assigned == "" {
		assigned = strOrEmpty(work.AssignedPeerID)
	}
	if processScope == "" && assigned == "" && mapStr(target, "path") != "" && mapStr(target, "backend") != "" {
		processScope = "per_fire"
	}
	if processScope == "" {
		processScope = "persistent"
	}
	if processScope == "per-fire" {
		processScope = "per_fire"
	}
	continuity, _ := execution["continuity"].(string)
	if continuity == "" && processScope == "per_fire" {
		if work.SourceKind != nil && *work.SourceKind == "calendar" {
			continuity = "resume"
		} else {
			continuity = "fresh"
		}
	}
	if continuity == "" {
		continuity = "resume"
	}
	return execPolicy{ProcessScope: processScope, Continuity: continuity}
}

// releaseHandleForPeer builds the per-fire release handle, or nil when the scope/
// strategy/pane don't qualify. Mirrors _release_handle_for_peer.
func releaseHandleForPeer(peer *proto.Peer, processScope, operationID, strategy string) map[string]any {
	if processScope != "per_fire" {
		return nil
	}
	if strategy != "spawned_peer" && strategy != "backend_resume" {
		return nil
	}
	if peer.PaneID == nil || *peer.PaneID == "" {
		return nil
	}
	return map[string]any{
		"kind":         "tmux_pane",
		"peer_id":      string(peer.PeerID),
		"display_name": string(peer.DisplayName),
		"pane_id":      *peer.PaneID,
		"tmux_session": strPtrOrNil(peer.TmuxSession),
		"operation_id": operationID,
		"strategy":     strategy,
		"created_at":   opReleaseNow(),
	}
}

// runtimeBindingForPeer builds the runtime binding snapshot, enriched from
// session_bindings when the SQLite store already knows this runtime.
func runtimeBindingForPeer(ctx context.Context, store *state.Store, peer *proto.Peer, work *state.TrackedWork) map[string]any {
	runtimeSessionID := runtimeSessionIDForPeer(peer)
	sessionBinding := sessionBindingForPeer(ctx, store, peer, runtimeSessionID)
	binding := map[string]any{
		"peer_id":      string(peer.PeerID),
		"display_name": string(peer.DisplayName),
		"backend":      string(peer.Backend),
		"path":         peer.Path,
		"circle":       peer.Circle,
		"status":       string(peer.Status),
		"tmux":         map[string]any{"tmux_session": strPtrOrNil(peer.TmuxSession), "pane_id": strPtrOrNil(peer.PaneID)},
	}
	if work != nil {
		binding["work_id"] = work.WorkID
	}
	if runtimeSessionID != "" {
		binding["runtime_session_id"] = runtimeSessionID
	}
	if len(peer.Metadata) > 0 {
		binding["metadata"] = peer.Metadata
	}
	if sessionBinding != nil {
		binding["repowire_session_id"] = sessionBinding.RepowireSessionID
		if sessionBinding.RuntimeSessionID != nil && *sessionBinding.RuntimeSessionID != "" {
			binding["runtime_session_id"] = *sessionBinding.RuntimeSessionID
		}
		binding["runtime_source_uri"] = strPtrOrNil(sessionBinding.RuntimeSourceURI)
		binding["source_cursor"] = sessionBinding.SourceCursor
		binding["resume_capability"] = sessionBinding.ResumeCapability
		binding["binding_status"] = string(sessionBinding.Status)
	}
	return binding
}

func sessionBindingForPeer(ctx context.Context, store *state.Store, peer *proto.Peer, runtimeSessionID string) *state.SessionBinding {
	if store == nil {
		return nil
	}
	backend := string(peer.Backend)
	projectPath := peer.Path
	if runtimeSessionID != "" {
		b, err := store.GetByRuntimeSession(ctx, runtimeSessionID, &backend, &projectPath)
		if err == nil && b != nil {
			return b
		}
	}
	bindings, err := store.ListBindingsByPeer(ctx, string(peer.PeerID))
	if err == nil && len(bindings) > 0 {
		return bindings[0]
	}
	return nil
}

func runtimeSessionIDForPeer(peer *proto.Peer) string {
	for _, key := range []string{"runtime_session_id", "hook_session_id", "session_id"} {
		if v, ok := peer.Metadata[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// preferLivePeer picks the best reuse candidate: online over busy, then most
// recently seen. Mirrors the max(...) key in _find_live_peer.
func preferLivePeer(candidates []*proto.Peer) *proto.Peer {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, p := range candidates[1:] {
		bOnline, pOnline := best.Status == proto.StatusOnline, p.Status == proto.StatusOnline
		if pOnline != bOnline {
			if pOnline {
				best = p
			}
			continue
		}
		if lastSeen(p).After(lastSeen(best)) {
			best = p
		}
	}
	return best
}

func lastSeen(p *proto.Peer) time.Time {
	if p.LastSeen != nil {
		return *p.LastSeen
	}
	return time.Time{}
}

func normalizePath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func opReleaseNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

// --- generic map/ptr helpers shared with job_runner.go / routes_work.go ---

func mapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func optStr(m map[string]any, key string) *string {
	if v, ok := m[key].(string); ok && v != "" {
		return &v
	}
	return nil
}

func mapAtAny(m map[string]any, key string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func anySlice(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func cloneAny(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func strOrNilAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func strPtrOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// strPtr returns nil for an empty string, else a pointer to s. Shared by
// session_control.go and job_runner.go; routes_messaging.go keeps its own
// 3-line copy on the route side.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
