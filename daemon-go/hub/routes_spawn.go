package hub

// routes_spawn.go owns the spawn-kill-restart HTTP route group, ported from
// repowire/daemon/routes/spawn.py:
//
//	GET  /spawn/config                    spawn config for UI discovery
//	POST /spawn                           create an agent session in tmux
//	POST /kill-peer                       kill a registered peer by mesh identity
//	POST /peers/{name}/restart            strict kill + backend-resume
//	POST /peers/{name}/switch-backend     kill + respawn with a new backend
//	POST /peers/{name}/rehook             ws-hook recovery (dry-run/report subset)
//
// Identity discipline: every destructive proof keys on peer_id, never
// display_name/path. The _destructive_pane_proof truth table is ported verbatim
// (spawned-set → durable ownership → live pane metadata peer_id match; path alone
// is NEVER proof). Fail loud over silent degrade: no proof → unregister but
// report tmux_killed=null; a tmux kill failure leaves the peer registered and
// 500s so an operator can inspect a possibly-live runtime.

import (
	"context"
	"net/http"
	"os"
	"time"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// spawnRegistry is the narrow registry seam the spawn routes need. The concrete
// registry satisfies it; keeping the seam makes route tests hermetic.
type spawnRegistry interface {
	LazyRepair(ctx context.Context)
	// ResolvePeerStrict returns 0/1/N candidates: empty → 404, one → resolved,
	// many → 409 ambiguous (the route lists candidates). NEW on *peer.Registry.
	ResolvePeerStrict(identifier string, circle *string) []*proto.Peer
	AllocateAndRegister(ctx context.Context, p peer.AllocateParams) (proto.PeerID, proto.DisplayName, error)
	GetPeer(id proto.PeerID) (*proto.Peer, bool)
	UnregisterPeer(ctx context.Context, identifier string, circle *string) (bool, error)
	MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error)
}

// spawnDeps bundles the spawn route dependencies, wired onto the Hub via
// WithSpawn. nil → the spawn endpoints are not registered.
type spawnDeps struct {
	svc         *service.SpawnService
	reg         spawnRegistry
	asks        *service.AskTracker
	selfMachine string
	boundary    proto.CircleBoundary
}

// WithSpawn attaches the spawn-kill-restart route group. svc owns tmux + ownership;
// reg is the resolve/allocate/unregister seam; asks supplies the quiesce barrier
// for restart/switch; selfMachine is the daemon hostname for the same-host gate.
// Returns the hub for chaining; call before Routes.
func (h *Hub) WithSpawn(svc *service.SpawnService, reg spawnRegistry, asks *service.AskTracker, selfMachine string, boundary proto.CircleBoundary) *Hub {
	if boundary == "" {
		boundary = proto.CircleBoundarySession
	}
	if svc != nil {
		svc.WithCircleBoundary(boundary)
	}
	h.spawn = &spawnDeps{svc: svc, reg: reg, asks: asks, selfMachine: selfMachine, boundary: boundary}
	return h
}

// registerSpawnRoutes wires the spawn handlers behind the shared bearer gate.
// Method-prefixed patterns keep these POSTs distinct from the peer read/lifecycle
// groups on the same /peers/{name} paths.
func (h *Hub) registerSpawnRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /spawn/config", h.requireAuth(h.handleSpawnConfig))
	mux.HandleFunc("POST /spawn", h.requireAuth(h.handleSpawn))
	mux.HandleFunc("POST /kill-peer", h.requireAuth(h.handleKillPeer))
	mux.HandleFunc("POST /peers/{name}/restart", h.requireAuth(h.handleRestartPeer))
	mux.HandleFunc("POST /peers/{name}/switch-backend", h.requireAuth(h.handleSwitchBackend))
	mux.HandleFunc("POST /peers/{name}/rehook", h.requireAuth(h.handleRehookPeer))
}

// spawnReady reports whether the spawn deps are wired; 503 otherwise.
func (h *Hub) spawnReady(w http.ResponseWriter) bool {
	if h.spawn == nil || h.spawn.svc == nil || h.spawn.reg == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "spawn not configured")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// GET /spawn/config
// ---------------------------------------------------------------------------

// SpawnConfigResponse mirrors spawn.py SpawnConfigResponse.
type SpawnConfigResponse struct {
	Enabled         bool                       `json:"enabled"`
	CircleBoundary  proto.CircleBoundary       `json:"circle_boundary"`
	Commands        map[proto.AgentType]string `json:"commands"`
	Profiles        map[string]any             `json:"profiles"`
	AllowedCommands []string                   `json:"allowed_commands"`
	AllowedPaths    []string                   `json:"allowed_paths"`
}

func (h *Hub) handleSpawnConfig(w http.ResponseWriter, r *http.Request) {
	if !h.spawnReady(w) {
		return
	}
	svc := h.spawn.svc
	commands := svc.Commands()
	allowed := make([]string, 0, len(commands))
	for _, c := range commands {
		allowed = append(allowed, c)
	}
	writeJSON(w, http.StatusOK, SpawnConfigResponse{
		Enabled:         svc.Enabled(),
		CircleBoundary:  h.spawn.boundary,
		Commands:        commands,
		Profiles:        spawnProfiles(svc.Profiles()),
		AllowedCommands: allowed,
		AllowedPaths:    svc.AllowedPaths(),
	})
}

func spawnProfiles(profiles map[proto.AgentType]map[string][]string) map[string]any {
	out := map[string]any{}
	for backend, items := range profiles {
		out[string(backend)] = items
	}
	return out
}

// ---------------------------------------------------------------------------
// POST /spawn
// ---------------------------------------------------------------------------

// SpawnRequest mirrors spawn.py SpawnRequest. Exactly one of backend/command must
// be set (validated below → 422). profile requires backend.
type SpawnRequest struct {
	Path       string           `json:"path"`
	Backend    *proto.AgentType `json:"backend"`
	Profile    *string          `json:"profile"`
	Command    *string          `json:"command"`
	Circle     string           `json:"circle"`
	Message    *string          `json:"message"`
	Role       proto.PeerRole   `json:"role"`
	SourcePane string           `json:"source_pane,omitempty"`
}

// SpawnResponse mirrors spawn.py SpawnResponse.
type SpawnResponse struct {
	OK                bool     `json:"ok"`
	DisplayName       string   `json:"display_name"`
	TmuxSession       string   `json:"tmux_session"`
	PeerID            *string  `json:"peer_id"`
	RegistrationState string   `json:"registration_state"`
	Warnings          []string `json:"warnings"`
}

// selfRegistersOnSpawn ports the per-backend self_registers_on_spawn flag. Default
// true (hook-backed runtimes self-register via SessionStart); only antigravity
// overrides to false in agent_backends.py.
func selfRegistersOnSpawn(b proto.AgentType) bool {
	return b != proto.AgentAntigravity
}

func (h *Hub) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := h.spawnPeer(r.Context(), req)
	if err != nil {
		h.writeSpawnError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// spawnPeer is the typed spawn entry point shared by HTTP and MCP callers.
// It preserves the route's validation, configured-command policy, Antigravity
// polling fallback, and ownership recording without requiring JSON plumbing.
func (h *Hub) spawnPeer(ctx context.Context, req SpawnRequest) (SpawnResponse, error) {
	if h.spawn == nil || h.spawn.svc == nil || h.spawn.reg == nil {
		return SpawnResponse{}, &service.SpawnError{Status: http.StatusServiceUnavailable, Detail: "spawn not configured"}
	}
	boundary := h.spawn.boundary
	if boundary == "" {
		boundary = proto.CircleBoundarySession
	}
	if boundary == proto.CircleBoundaryWindow {
		if req.SourcePane == "" && req.Circle == "" {
			return SpawnResponse{}, &service.SpawnError{Status: http.StatusUnprocessableEntity, Detail: "circle or source_pane is required when daemon.circle_boundary is window"}
		}
	} else if req.Circle == "" {
		return SpawnResponse{}, &service.SpawnError{Status: http.StatusUnprocessableEntity, Detail: "circle is required; run inside a tmux session or pass --circle"}
	}
	if req.Role == "" {
		req.Role = proto.RoleAgent
	}

	// Single runtime selector (mirrors SpawnRequest._single_runtime_selector).
	if req.Backend != nil && req.Command != nil {
		return SpawnResponse{}, &service.SpawnError{Status: http.StatusUnprocessableEntity, Detail: "Pass backend or command, not both"}
	}
	if req.Backend == nil && req.Command == nil {
		return SpawnResponse{}, &service.SpawnError{Status: http.StatusUnprocessableEntity, Detail: "Pass backend or command"}
	}
	if req.Profile != nil && *req.Profile != "" && req.Backend == nil {
		return SpawnResponse{}, &service.SpawnError{Status: http.StatusUnprocessableEntity, Detail: "Pass backend with profile"}
	}

	var backend proto.AgentType
	if req.Backend != nil {
		backend = *req.Backend
	} else {
		// Legacy command selector: resolve to a backend whose configured command
		// matches. ponytail: the legacy command->backend alias path is the rare
		// compatibility case; we match against configured commands directly.
		resolved, ok := h.resolveLegacyCommand(*req.Command)
		if !ok {
			return SpawnResponse{}, &service.SpawnError{Status: http.StatusForbidden, Detail: map[string]any{
				"error":   "command_unavailable",
				"hint":    "Command/profile not configured. Use daemon.spawn.commands keyed by backend.",
				"command": *req.Command,
			}}
		}
		backend = resolved
	}

	svc := h.spawn.svc
	// Resolve command BEFORE spawn so a 422/403 surfaces without a pane.
	command, err := svc.ResolveCommand(backend, req.Profile)
	if err != nil {
		return SpawnResponse{}, err
	}
	result, err := svc.Spawn(service.SpawnConfig{
		Path: req.Path, Circle: req.Circle, TargetPane: req.SourcePane,
		Backend: backend, Command: command, Message: req.Message, Role: req.Role,
	})
	if err != nil {
		return SpawnResponse{}, err
	}

	resp := SpawnResponse{
		OK:                true,
		DisplayName:       result.DisplayName,
		TmuxSession:       result.TmuxSession,
		RegistrationState: "pending_hook",
		Warnings:          []string{},
	}

	// Non-self-registering backends are daemon-pre-registered for CLI polling.
	if result.PaneID != "" && !selfRegistersOnSpawn(backend) {
		resolvedPath := service.NormPath(req.Path)
		warning := string(backend) + " hooks do not currently fire reliably; Repowire pre-registered this peer for CLI polling."
		metadata := map[string]any{
			"repowire_cli_fallback": true,
			"spawn_registration":    "daemon_pre_registered",
			"spawn_warning":         warning,
		}
		paneID := result.PaneID
		tmux := result.TmuxSession
		peerID, displayName, aerr := h.spawn.reg.AllocateAndRegister(ctx, peer.AllocateParams{
			Circle:      result.Circle,
			Backend:     backend,
			Path:        &resolvedPath,
			PaneID:      &paneID,
			TmuxSession: &tmux,
			Machine:     h.spawn.selfMachine,
			Role:        req.Role,
			Metadata:    metadata,
		})
		if aerr != nil {
			return SpawnResponse{}, &service.SpawnError{Status: http.StatusConflict, Detail: aerr.Error()}
		}
		// turn_state pending_first_turn when a seed message is in flight.
		// AllocateParams carries no turn_state, so set it post-register through the
		// registry FSM-orthogonal field (mirrors the Python turn_state arg).
		if req.Message != nil {
			if reg, ok := h.spawn.reg.(turnStateRegistry); ok {
				reg.UpdateTurnState(ctx, peerID, proto.TurnPendingFirstTurn)
			}
		}
		// Re-record ownership now that we know the assigned peer_id (so the durable
		// proof carries the strong disambiguator).
		pid := string(peerID)
		svc.Ownership().Record(service.OwnershipRecord{
			PaneID:      paneID,
			Path:        resolvedPath,
			Backend:     string(backend),
			Circle:      req.Circle,
			Role:        string(req.Role),
			DisplayName: string(displayName),
			TmuxSession: tmux,
			Machine:     h.spawn.selfMachine,
			PeerID:      &pid,
		})
		idStr := string(peerID)
		resp.PeerID = &idStr
		resp.DisplayName = string(displayName)
		resp.RegistrationState = "cli_fallback"
		resp.Warnings = append(resp.Warnings,
			string(backend)+" plugin hooks are pending upstream; peer was pre-registered for CLI polling, "+
				"so ask/notify delivery queues until the peer drains it with `repowire peer asks` / `repowire peer deliveries`.")
	}

	return resp, nil
}

// turnStateRegistry is the optional seam for seeding pending_first_turn after a
// CLI-fallback pre-registration; *peer.Registry satisfies it (UpdateTurnState).
type turnStateRegistry interface {
	UpdateTurnState(ctx context.Context, id proto.PeerID, ts proto.TurnState)
}

// resolveLegacyCommand maps a legacy command string to a backend whose configured
// command equals it (or whose value the string matches). Returns false when none.
func (h *Hub) resolveLegacyCommand(command string) (proto.AgentType, bool) {
	for backend, configured := range h.spawn.svc.Commands() {
		if command == configured || command == string(backend) {
			return backend, true
		}
	}
	return "", false
}

// writeSpawnError surfaces a *service.SpawnError as its carried HTTP status + detail; any
// other error is a 500.
func (h *Hub) writeSpawnError(w http.ResponseWriter, err error) {
	if se, ok := service.AsSpawnError(err); ok {
		writeJSONError(w, se.Status, se.Detail)
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}

// ---------------------------------------------------------------------------
// POST /kill-peer
// ---------------------------------------------------------------------------

// KillPeerRequest mirrors spawn.py KillPeerRequest.
type KillPeerRequest struct {
	PeerIdentifier string  `json:"peer_identifier"`
	Circle         *string `json:"circle"`
	FromPeer       *string `json:"from_peer"`
}

// KillResponse mirrors spawn.py KillResponse. tmux_killed: true (killed), false
// (kill attempted but failed), null (kill skipped — ownership unproven).
type KillResponse struct {
	OK         bool  `json:"ok"`
	TmuxKilled *bool `json:"tmux_killed"`
}

func (h *Hub) handleKillPeer(w http.ResponseWriter, r *http.Request) {
	var req KillPeerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := h.killPeer(r.Context(), req)
	if err != nil {
		h.writeSpawnError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// killPeer is the typed destructive-control entry point shared by HTTP and MCP.
// It retains strict identity resolution and the existing pane-proof truth table.
func (h *Hub) killPeer(ctx context.Context, req KillPeerRequest) (KillResponse, error) {
	if h.spawn == nil || h.spawn.svc == nil || h.spawn.reg == nil {
		return KillResponse{}, &service.SpawnError{Status: http.StatusServiceUnavailable, Detail: "spawn not configured"}
	}
	h.spawn.reg.LazyRepair(ctx)

	resolved, err := h.resolveStrict(req.PeerIdentifier, req.Circle)
	if err != nil {
		return KillResponse{}, err
	}

	peerCopy := h.peerWithAdoptedOwnership(resolved)
	proof := h.destructivePaneProof(peerCopy)
	if !proof.ok || proof.paneID == "" {
		// No proof: unregister identity but DO NOT touch any pane. tmux_killed=null.
		// id is already resolved (resolveStrict), so the ambiguity error can't fire.
		_, _ = h.spawn.reg.UnregisterPeer(ctx, string(peerCopy.PeerID), nil)
		return KillResponse{OK: true, TmuxKilled: nil}, nil
	}

	killed := h.spawn.svc.Tmux().KillPane(proof.paneID)
	if !killed {
		// Fail loud: verified pane could not be killed; leave the peer registered.
		return KillResponse{}, &service.SpawnError{Status: http.StatusInternalServerError, Detail: map[string]any{
			"error":   "kill_failed",
			"hint":    "tmux kill-pane failed for the peer's verified pane; the peer remains registered so the operator can inspect it.",
			"pane_id": proof.paneID,
		}}
	}
	h.spawn.svc.Ownership().Forget(proof.paneID)
	clienthooks.ClearPaneRuntimeState(proof.paneID) // stale meta must not re-prove a reused pane
	// id is already resolved (resolveStrict), so the ambiguity error can't fire.
	_, _ = h.spawn.reg.UnregisterPeer(ctx, string(peerCopy.PeerID), nil)
	t := true
	return KillResponse{OK: true, TmuxKilled: &t}, nil
}

// ---------------------------------------------------------------------------
// POST /peers/{name}/restart
// ---------------------------------------------------------------------------

// RestartPeerRequest mirrors spawn.py RestartPeerRequest.
type RestartPeerRequest struct {
	Circle   *string `json:"circle"`
	FromPeer *string `json:"from_peer"`
	DryRun   bool    `json:"dry_run"`
	Message  *string `json:"message"`
}

// RestartPeerResponse mirrors spawn.py RestartPeerResponse.
type RestartPeerResponse struct {
	OK                bool            `json:"ok"`
	Status            string          `json:"status"`
	Restarted         bool            `json:"restarted"`
	PeerID            string          `json:"peer_id"`
	DisplayName       string          `json:"display_name"`
	Backend           proto.AgentType `json:"backend"`
	Path              string          `json:"path"`
	Circle            string          `json:"circle"`
	TmuxSession       *string         `json:"tmux_session"`
	ResumeMode        string          `json:"resume_mode"`
	ResumeWarning     *string         `json:"resume_warning"`
	UnsupportedReason *string         `json:"unsupported_reason"`
	Command           *string         `json:"command"`
}

func (h *Hub) handleRestartPeer(w http.ResponseWriter, r *http.Request) {
	if !h.spawnReady(w) {
		return
	}
	name := r.PathValue("name")
	var req RestartPeerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	h.spawn.reg.LazyRepair(ctx)

	resolved, err := h.resolveStrict(name, req.Circle)
	if err != nil {
		h.writeSpawnError(w, err)
		return
	}
	peerCopy := h.peerWithAdoptedOwnership(resolved)

	if !h.sameHostOK(w, peerCopy, "Peer restart is same-host only in this slice.") {
		return
	}
	if peerCopy.Path == "" {
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error": "missing_path",
			"hint":  "Peer has no recorded working directory; cannot restart.",
		})
		return
	}

	svc := h.spawn.svc
	resolvedPath, perr := svc.ValidatePath(peerCopy.Path)
	if perr != nil {
		h.writeSpawnError(w, perr)
		return
	}
	spawnCircle := peerCopy.Circle
	if spawnCircle == "" {
		writeJSONError(w, http.StatusConflict, "peer has no circle; cannot restart")
		return
	}
	resumeCommand, _, rerr := h.restartResumeCommand(ctx, peerCopy, resolvedPath)
	if rerr != nil {
		writeJSONError(w, http.StatusConflict, rerr)
		return
	}
	resumeMode := "resumed"

	proof := h.destructivePaneProof(peerCopy)
	canSkipKill := peerCopy.Status == proto.StatusOffline &&
		(proof.errCode == "missing_pane" || proof.errCode == "pane_not_live")
	if !proof.ok && !canSkipKill {
		writeJSONError(w, http.StatusConflict, h.paneControlErrorDetail(peerCopy, proof))
		return
	}
	spawnConfig := service.SpawnConfig{
		Path: resolvedPath, Circle: spawnCircle, Backend: peerCopy.Backend,
		Role: peerCopy.Role, PeerID: &peerCopy.PeerID,
	}
	if proof.ok {
		spawnConfig.TargetPane = proof.paneID
		spawnConfig, perr = svc.PrepareReplacement(spawnConfig)
	} else {
		spawnConfig, perr = svc.PrepareSpawn(spawnConfig)
	}
	if perr != nil {
		h.writeSpawnError(w, perr)
		return
	}

	tmuxSession := proof.tmuxSession
	if tmuxSession == "" && peerCopy.TmuxSession != nil {
		tmuxSession = *peerCopy.TmuxSession
	}

	if req.DryRun {
		ts := tmuxSession
		writeJSON(w, http.StatusOK, RestartPeerResponse{
			OK:          true,
			Status:      "restart_available",
			Restarted:   false,
			PeerID:      string(peerCopy.PeerID),
			DisplayName: string(peerCopy.DisplayName),
			Backend:     peerCopy.Backend,
			Path:        resolvedPath,
			Circle:      spawnCircle,
			TmuxSession: &ts,
			Command:     &resumeCommand,
			ResumeMode:  resumeMode,
		})
		return
	}

	// Quiesce barrier: blocks new asks in either direction + verifies none are
	// open. service.ErrQuiesceHasOpen → 409 in_flight_asks; service.ErrQuiesced (concurrent
	// restart) → 409 restart_in_progress.
	if h.spawn.asks != nil {
		if qerr := h.spawn.asks.BeginQuiesce(ctx, peerCopy.PeerID); qerr != nil {
			h.writeQuiesceError(w, qerr, ctx, peerCopy.PeerID, "restart_in_progress",
				"Another restart/switch is in progress for this peer. Retry shortly.")
			return
		}
		defer h.spawn.asks.EndQuiesce(ctx, peerCopy.PeerID)
	}

	if proof.ok {
		if !h.spawn.svc.Tmux().KillPane(proof.paneID) {
			writeJSONError(w, http.StatusInternalServerError, map[string]any{
				"error":   "kill_failed",
				"hint":    "tmux kill-pane failed for the peer's verified pane; the old runtime may still be alive. Aborting restart to avoid duplicates.",
				"pane_id": proof.paneID,
			})
			return
		}
		h.spawn.svc.Ownership().Forget(proof.paneID)
		clienthooks.ClearPaneRuntimeState(proof.paneID) // stale meta must not re-prove the reused pane
		_, _ = h.spawn.reg.MarkOffline(ctx, peerCopy.PeerID, false)
	}

	spawnConfig.Command = resumeCommand
	spawnConfig.Message = req.Message
	result, serr := svc.Spawn(spawnConfig)
	if serr != nil {
		h.writeSpawnError(w, serr)
		return
	}

	ts := result.TmuxSession
	cmd := resumeCommand
	writeJSON(w, http.StatusOK, RestartPeerResponse{
		OK:                true,
		Status:            "restarted",
		Restarted:         true,
		PeerID:            string(peerCopy.PeerID),
		DisplayName:       string(peerCopy.DisplayName),
		Backend:           peerCopy.Backend,
		Path:              resolvedPath,
		Circle:            spawnCircle,
		TmuxSession:       &ts,
		Command:           &cmd,
		ResumeMode:        resumeMode,
		ResumeWarning:     nil,
		UnsupportedReason: nil,
	})
}

func (h *Hub) restartResumeCommand(ctx context.Context, peer *proto.Peer, resolvedPath string) (string, map[string]any, map[string]any) {
	base, err := h.spawn.svc.ResolveCommand(peer.Backend, nil)
	if err != nil {
		return "", nil, map[string]any{"error": "command_unavailable", "hint": err.Error()}
	}
	runtimeID, repowireSessionID, capability := h.restartResumeTarget(ctx, peer, resolvedPath)
	if runtimeID == "" {
		return "", nil, restartResumeUnavailable(peer, "missing_id")
	}
	plan, ok := service.ResolveLocalResume(peer.Backend, resolvedPath, runtimeID, repowireSessionID, capability)
	if !ok {
		return "", nil, restartResumeUnavailable(peer, "resume_unavailable")
	}
	command, err := service.BuildResumeCommand(base, peer.Backend, runtimeID)
	if err != nil {
		return "", nil, map[string]any{"error": "resume_unavailable", "hint": err.Error(), "peer_id": string(peer.PeerID)}
	}
	return command, plan, nil
}

func (h *Hub) restartResumeTarget(ctx context.Context, peer *proto.Peer, resolvedPath string) (runtimeID string, repowireSessionID *string, capability map[string]any) {
	capability = map[string]any{}
	if h.store != nil {
		bindings, err := h.store.ListBindingsByPeer(ctx, string(peer.PeerID))
		if err == nil {
			for _, b := range bindings {
				if b.RuntimeSessionID == nil || *b.RuntimeSessionID == "" || b.Backend != string(peer.Backend) {
					continue
				}
				if service.NormPath(b.ProjectPath) != service.NormPath(resolvedPath) {
					continue
				}
				return *b.RuntimeSessionID, &b.RepowireSessionID, b.ResumeCapability
			}
		}
	}
	if id := runtimeSessionIDFromMetadata(peer.Metadata); id != nil {
		return *id, nil, capability
	}
	return "", nil, capability
}

func restartResumeUnavailable(peer *proto.Peer, reason string) map[string]any {
	return map[string]any{
		"error":              "resume_unavailable",
		"hint":               "Restart is strict kill+resume: Repowire will not kill this peer unless it can first build a validated backend-native resume command.",
		"peer_id":            string(peer.PeerID),
		"display_name":       string(peer.DisplayName),
		"backend":            string(peer.Backend),
		"unsupported_reason": reason,
	}
}

// ---------------------------------------------------------------------------
// POST /peers/{name}/switch-backend
// ---------------------------------------------------------------------------

// SwitchBackendRequest mirrors spawn.py SwitchBackendRequest.
type SwitchBackendRequest struct {
	NewBackend proto.AgentType `json:"new_backend"`
}

// SwitchBackendResponse mirrors spawn.py SwitchBackendResponse.
type SwitchBackendResponse struct {
	OK          bool            `json:"ok"`
	DisplayName string          `json:"display_name"`
	TmuxSession string          `json:"tmux_session"`
	OldBackend  proto.AgentType `json:"old_backend"`
	NewBackend  proto.AgentType `json:"new_backend"`
	Command     string          `json:"command"`
}

func (h *Hub) handleSwitchBackend(w http.ResponseWriter, r *http.Request) {
	if !h.spawnReady(w) {
		return
	}
	name := r.PathValue("name")
	var req SwitchBackendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var circle *string
	if c := r.URL.Query().Get("circle"); c != "" {
		circle = &c
	}
	ctx := r.Context()
	h.spawn.reg.LazyRepair(ctx)

	resolved, err := h.resolveStrict(name, circle)
	if err != nil {
		h.writeSpawnError(w, err)
		return
	}
	peerCopy := resolved // switch uses identity + spawned-set ownership, not adoption

	if !h.sameHostOK(w, peerCopy, "Backend switch is same-host only in v1; ACP transport required for remote peers.") {
		return
	}
	if peerCopy.Backend == req.NewBackend {
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error":   "same_backend",
			"hint":    "Peer is already running " + string(peerCopy.Backend),
			"backend": string(peerCopy.Backend),
		})
		return
	}
	if peerCopy.Path == "" {
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error": "missing_path",
			"hint":  "Peer has no recorded working directory; cannot respawn.",
		})
		return
	}

	svc := h.spawn.svc
	resolvedPath, perr := svc.ValidatePath(peerCopy.Path)
	if perr != nil {
		h.writeSpawnError(w, perr)
		return
	}
	command, cerr := svc.ResolveCommand(req.NewBackend, nil)
	if cerr != nil {
		// Decorate command_unavailable with new_backend (parity with Python).
		if se, ok := service.AsSpawnError(cerr); ok {
			if m, ok := se.Detail.(map[string]any); ok && m["error"] == "command_unavailable" {
				m["new_backend"] = string(req.NewBackend)
			}
		}
		h.writeSpawnError(w, cerr)
		return
	}
	proof := h.destructivePaneProof(peerCopy)
	if !proof.ok {
		writeJSONError(w, http.StatusConflict, h.paneControlErrorDetail(peerCopy, proof))
		return
	}

	if h.spawn.asks != nil {
		if qerr := h.spawn.asks.BeginQuiesce(ctx, peerCopy.PeerID); qerr != nil {
			h.writeQuiesceError(w, qerr, ctx, peerCopy.PeerID, "switch_in_progress",
				"Another switch is in progress for this peer. Retry shortly.")
			return
		}
		defer h.spawn.asks.EndQuiesce(ctx, peerCopy.PeerID)
	}

	spawnCircle := peerCopy.Circle
	if spawnCircle == "" {
		writeJSONError(w, http.StatusConflict, "peer has no circle; cannot switch backend")
		return
	}
	spawnConfig, perr := svc.PrepareReplacement(service.SpawnConfig{
		Path: resolvedPath, Circle: spawnCircle, TargetPane: proof.paneID,
		Backend: req.NewBackend, Command: command, Role: peerCopy.Role,
	})
	if perr != nil {
		h.writeSpawnError(w, perr)
		return
	}

	if !svc.Tmux().KillPane(proof.paneID) {
		writeJSONError(w, http.StatusInternalServerError, map[string]any{
			"error":   "kill_failed",
			"hint":    "tmux kill-pane failed for the peer's verified pane; the old agent may still be alive. Aborting switch to avoid a zombie runtime. Check `tmux list-panes -a`.",
			"pane_id": proof.paneID,
		})
		return
	}
	svc.Ownership().Forget(proof.paneID)
	clienthooks.ClearPaneRuntimeState(proof.paneID)
	// id is already resolved (resolveStrict), so the ambiguity error can't fire.
	_, _ = h.spawn.reg.UnregisterPeer(ctx, string(peerCopy.PeerID), nil)

	result, serr := svc.Spawn(spawnConfig)
	if serr != nil {
		h.writeSpawnError(w, serr)
		return
	}
	writeJSON(w, http.StatusOK, SwitchBackendResponse{
		OK:          true,
		DisplayName: result.DisplayName,
		TmuxSession: result.TmuxSession,
		OldBackend:  peerCopy.Backend,
		NewBackend:  req.NewBackend,
		Command:     command,
	})
}

// ---------------------------------------------------------------------------
// POST /peers/{name}/rehook — dry-run/report subset.
// ---------------------------------------------------------------------------

// RehookPeerRequest mirrors spawn.py RehookPeerRequest.
type RehookPeerRequest struct {
	Circle   *string `json:"circle"`
	FromPeer *string `json:"from_peer"`
	Apply    bool    `json:"apply"`
}

// RehookPeerResponse mirrors spawn.py RehookPeerResponse.
type RehookPeerResponse struct {
	OK              bool    `json:"ok"`
	Acted           bool    `json:"acted"`
	PeerID          string  `json:"peer_id"`
	DisplayName     string  `json:"display_name"`
	PaneID          *string `json:"pane_id"`
	WSWasConnected  bool    `json:"ws_was_connected"`
	PingOK          *bool   `json:"ping_ok"`
	PaneVerified    bool    `json:"pane_verified"`
	WSHookRespawned bool    `json:"ws_hook_respawned"`
	Reason          string  `json:"reason"`
}

func (h *Hub) handleRehookPeer(w http.ResponseWriter, r *http.Request) {
	if !h.spawnReady(w) {
		return
	}
	name := r.PathValue("name")
	var req RehookPeerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	h.spawn.reg.LazyRepair(ctx)

	resolved, err := h.resolveStrict(name, req.Circle)
	if err != nil {
		h.writeSpawnError(w, err)
		return
	}
	peerCopy := h.peerWithAdoptedOwnership(resolved)
	wasConnected := h.transport.IsConnected(peerCopy.PeerID)
	if wasConnected {
		if _, err := h.transport.Ping(ctx, peerCopy.PeerID, 2*time.Second); err == nil {
			ok := true
			writeJSON(w, http.StatusOK, RehookPeerResponse{OK: true, Acted: false, PeerID: string(peerCopy.PeerID), DisplayName: string(peerCopy.DisplayName), PaneID: peerCopy.PaneID, WSWasConnected: true, PingOK: &ok, PaneVerified: true, Reason: "already_healthy"})
			return
		}
	}

	if !h.sameHostOK(w, peerCopy, "Peer rehook is same-host only.") {
		return
	}
	if peerCopy.PaneID == nil || *peerCopy.PaneID == "" {
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error": "missing_pane",
			"hint":  "Peer has no recorded pane; nothing to rehook.",
		})
		return
	}

	// Ownership gate: prove the pane is real AND belongs to THIS peer. Accept
	// durable spawn-ownership proof OR live tmux evidence whose current_path
	// matches the peer's path.
	paneVerified := h.spawn.svc.Ownership().IsSpawned(*peerCopy.PaneID) ||
		h.spawn.svc.Ownership().ValidateForPeer(peerCopy).OK
	if !paneVerified {
		if ev := h.spawn.svc.Tmux().ProbePane(*peerCopy.PaneID); ev != nil &&
			peerCopy.Path != "" && service.NormPath(ev.CurrentPath) == service.NormPath(peerCopy.Path) {
			paneVerified = true
		}
	}
	if !paneVerified {
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error":   "pane_unverified",
			"hint":    "Could not prove the pane belongs to this peer (no spawn ownership, pane path mismatch).",
			"pane_id": *peerCopy.PaneID,
		})
		return
	}

	if !req.Apply {
		writeJSON(w, http.StatusOK, RehookPeerResponse{
			OK:           true,
			Acted:        false,
			PeerID:       string(peerCopy.PeerID),
			DisplayName:  string(peerCopy.DisplayName),
			PaneID:       peerCopy.PaneID,
			PaneVerified: true,
			Reason:       "dry_run",
		})
		return
	}

	agentPID := 0
	if peerCopy.AgentPID != nil {
		agentPID = *peerCopy.AgentPID
	}
	respawned, err := clienthooks.ReconcileWSHook(*peerCopy.PaneID, string(peerCopy.PeerID), string(peerCopy.DisplayName), string(peerCopy.Backend), peerCopy.Path, agentPID)
	if err != nil {
		writeJSONError(w, http.StatusConflict, map[string]any{"error": "rehook_failed", "hint": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, RehookPeerResponse{
		OK:              true,
		Acted:           respawned,
		PeerID:          string(peerCopy.PeerID),
		DisplayName:     string(peerCopy.DisplayName),
		PaneID:          peerCopy.PaneID,
		WSWasConnected:  wasConnected,
		PaneVerified:    true,
		WSHookRespawned: respawned,
		Reason:          "ws_hook_respawned",
	})
}

// ---------------------------------------------------------------------------
// Shared resolution + destructive-proof helpers.
// ---------------------------------------------------------------------------

// resolveStrict resolves an identifier without coupling the operation to HTTP.
func (h *Hub) resolveStrict(identifier string, circle *string) (*proto.Peer, error) {
	candidates := h.spawn.reg.ResolvePeerStrict(identifier, circle)
	if len(candidates) == 0 {
		return nil, &service.SpawnError{Status: http.StatusNotFound, Detail: "Peer not found: " + identifier}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	items := make([]map[string]any, 0, len(candidates))
	for _, p := range candidates {
		items = append(items, map[string]any{
			"peer_id": string(p.PeerID), "display_name": string(p.DisplayName),
			"circle": p.Circle, "tmux_session": p.TmuxSession,
		})
	}
	return nil, &service.SpawnError{Status: http.StatusConflict, Detail: map[string]any{
		"error": "Ambiguous peer identifier: " + identifier, "candidates": items,
	}}
}

// sameHostOK enforces the same-host gate: a peer on another machine 409s
// cross_host. machine "unknown"/"" passes (no host claim to contradict).
func (h *Hub) sameHostOK(w http.ResponseWriter, p *proto.Peer, hint string) bool {
	if p.Machine != "" && p.Machine != "unknown" && p.Machine != h.spawn.selfMachine {
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error":        "cross_host",
			"hint":         hint,
			"peer_machine": p.Machine,
			"self_machine": h.spawn.selfMachine,
		})
		return false
	}
	return true
}

// peerWithAdoptedOwnership ports _peer_with_adopted_ownership: when durable
// ownership uniquely resolves to a different pane than the peer's recorded one,
// return a copy carrying the record's pane/tmux/machine (so a rehydrated peer that
// lost its pane fields across restart still has a kill handle).
func (h *Hub) peerWithAdoptedOwnership(p *proto.Peer) *proto.Peer {
	v := h.spawn.svc.Ownership().ValidateForPeer(p)
	if !v.OK || v.Record == nil {
		return p
	}
	if p.PaneID != nil && *p.PaneID == v.Record.PaneID {
		return p
	}
	cp := *p
	paneID := v.Record.PaneID
	tmux := v.Record.TmuxSession
	cp.PaneID = &paneID
	cp.TmuxSession = &tmux
	cp.Machine = v.Record.Machine
	return &cp
}

// destructiveProof is the kill-authorization result (port of _DestructivePaneProof).
type destructiveProof struct {
	ok          bool
	paneID      string
	tmuxSession string
	mode        string
	errCode     string
	hint        string
}

// destructivePaneProof ports _destructive_pane_proof: the only accepted proofs
// are (1) the pane is in the in-process spawned set, (2) durable spawn ownership
// validates, or (3) live pane metadata (the ws-hook meta.json) names THIS
// peer_id. Path match alone is NEVER proof. A live pane whose metadata names a
// different peer is a mismatch (distinct from no metadata); both refuse.
func (h *Hub) destructivePaneProof(p *proto.Peer) destructiveProof {
	own := h.spawn.svc.Ownership()

	if p.PaneID != nil && *p.PaneID != "" && own.IsSpawned(*p.PaneID) {
		return destructiveProof{ok: true, paneID: *p.PaneID, mode: "repowire_spawned_pane",
			tmuxSession: derefString(p.TmuxSession)}
	}

	ownership := own.ValidateForPeer(p)
	if ownership.OK && ownership.Record != nil {
		return destructiveProof{ok: true, paneID: ownership.Record.PaneID,
			tmuxSession: ownership.Record.TmuxSession, mode: "durable_spawn_ownership"}
	}
	if ownership.Error != "" && ownership.Error != "missing_ownership" {
		pane := derefString(p.PaneID)
		if ownership.Record != nil {
			pane = ownership.Record.PaneID
		}
		ts := derefString(p.TmuxSession)
		if ownership.Evidence != nil {
			ts = ownership.Evidence.TmuxSession
		} else if ownership.Record != nil {
			ts = ownership.Record.TmuxSession
		}
		return destructiveProof{paneID: pane, tmuxSession: ts, errCode: ownership.Error, hint: ownership.Hint}
	}

	if p.PaneID == nil || *p.PaneID == "" {
		return destructiveProof{errCode: "missing_pane",
			hint: "Peer has no pane id, so Repowire has no pane to kill."}
	}

	_ = own
	ev := h.spawn.svc.Tmux().ProbePane(*p.PaneID)
	if ev == nil {
		return destructiveProof{paneID: *p.PaneID, errCode: "pane_not_live",
			hint: "The peer's recorded pane is not visible in tmux."}
	}

	// (3) Live pane metadata names THIS peer_id. The ws-hook writes the pane's
	// owning peer_id into ws-hook-<pane>.meta.json; a match proves this peer really
	// occupies the live pane (not merely shares its path). Parity with
	// spawn.py destructive proof mode 3. Path match alone is still NOT proof.
	if meta := clienthooks.ReadPaneRuntimeMetadata(*p.PaneID); meta != nil {
		if mpid, _ := meta["peer_id"].(string); mpid != "" {
			if mpid == string(p.PeerID) {
				return destructiveProof{ok: true, paneID: *p.PaneID, tmuxSession: ev.TmuxSession,
					mode: "verified_pane_metadata"}
			}
			// Meta names a DIFFERENT peer — the live pane is another peer's, not this
			// one. Distinguish from "no metadata" for diagnostics (parity with Python
			// pane_metadata_mismatch). Same blocking behavior either way.
			return destructiveProof{paneID: *p.PaneID, tmuxSession: ev.TmuxSession,
				errCode: "pane_metadata_mismatch",
				hint:    "Live pane's peer_id metadata names a different peer. Refusing destructive control."}
		}
	}

	return destructiveProof{paneID: *p.PaneID, tmuxSession: ev.TmuxSession,
		errCode: "missing_pane_metadata",
		hint:    "Live pane has no peer_id metadata. Path match alone is not enough for destructive controls."}
}

// paneControlErrorDetail mirrors _pane_control_error_detail.
func (h *Hub) paneControlErrorDetail(p *proto.Peer, proof destructiveProof) map[string]any {
	errCode := proof.errCode
	if errCode == "" {
		errCode = "pane_unverified"
	}
	hint := proof.hint
	if hint == "" {
		hint = "Destructive peer controls require a Repowire spawn proof or pane metadata whose peer_id matches the target peer."
	}
	return map[string]any{
		"error":   errCode,
		"hint":    hint,
		"pane_id": derefString(p.PaneID),
	}
}

// writeQuiesceError maps a BeginQuiesce error to the right 409 shape. An open-ask
// failure surfaces in_flight_asks; a concurrent-quiesce failure uses the supplied
// inProgress error/hint. Releases nothing (the caller never acquired the barrier).
func (h *Hub) writeQuiesceError(w http.ResponseWriter, qerr error, ctx context.Context, id proto.PeerID, inProgressErr, inProgressHint string) {
	if qerr == service.ErrQuiesceHasOpen {
		open, _ := h.spawn.asks.PendingForPeer(ctx, id, -1, "both")
		cids := make([]string, 0, len(open))
		for _, a := range open {
			cids = append(cids, a.CorrelationID)
		}
		writeJSONError(w, http.StatusConflict, map[string]any{
			"error":     "in_flight_asks",
			"hint":      "Peer has open asks; the operation would orphan them. Retry after they're acked or evicted.",
			"open_asks": cids,
		})
		return
	}
	writeJSONError(w, http.StatusConflict, map[string]any{
		"error": inProgressErr,
		"hint":  inProgressHint,
	})
}

// selfHostname returns the daemon hostname for the same-host gate (used by main
// wiring; here for the package's convenience).
func selfHostname() string {
	if hn, err := os.Hostname(); err == nil && hn != "" {
		return hn
	}
	return "unknown"
}
