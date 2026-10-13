package hub

// peer_restart.go owns `peer restart` (#470): a daemon-owned sequence that
// stops a runtime and relaunches it with resume in a new pane of the same tmux
// window. The launch is reconstructed (backend + cwd + permission mode +
// resume id), never replayed from a recorded argv, and missing inputs are
// defaulted and listed rather than refused.
//
// Stages, one attempt each, every wait bounded and ctx-cancellable:
//
//	prepare   resolve, destructive proof, plan, reserve the new pane (sync)
//	exit      SIGTERM the verified runtime pid (kill-pane when no pid is known)
//	wait      the pid, then the pane, is gone
//	launch    validate the resume id against the post-exit session file, start
//	register  the runtime registers on the new pane; orchestrator role re-claimed
//
// The sequence runs on the daemon context, not the request's, so a peer that
// restarts itself can die mid-way and the daemon still finishes. A failure is a
// typed result naming the stage and the exact command that would have run; the
// old binding is never rewritten and a launch is never retried.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

const (
	restartStagePrepare  = "prepare"
	restartStageExit     = "exit"
	restartStageWait     = "wait"
	restartStageLaunch   = "launch"
	restartStageRegister = "register"

	restartOutcomePlanned    = "planned"
	restartOutcomeInProgress = "in_progress"
	restartOutcomeRestarted  = "restarted"
	restartOutcomeFailed     = "failed"

	restartSender = "repowire"
)

var (
	restartExitTimeout     = 20 * time.Second
	restartRegisterTimeout = 90 * time.Second
	restartPollInterval    = 250 * time.Millisecond
)

// RestartPeerRequest is the POST /peers/{name}/restart body. Wait defaults to
// true; a caller that restarts itself should pass false (MCP does).
type RestartPeerRequest struct {
	Circle   *string `json:"circle"`
	FromPeer *string `json:"from_peer"`
	DryRun   bool    `json:"dry_run"`
	Fresh    bool    `json:"fresh"`
	Message  *string `json:"message"`
	Wait     *bool   `json:"wait"`
}

// RestartLaunch is the reconstructed launch: what ran, or what would have run.
// Env values are never included; EnvKeys names the variables prefixed at launch.
type RestartLaunch struct {
	Backend              proto.AgentType `json:"backend"`
	CWD                  string          `json:"cwd"`
	Command              string          `json:"command"`
	PermissionMode       string          `json:"permission_mode"`
	PermissionModeSource string          `json:"permission_mode_source"`
	ResumeID             string          `json:"resume_id,omitempty"`
	EnvKeys              []string        `json:"env_keys,omitempty"`
	// Manual is the paste-ready line a human runs to finish a failed restart.
	Manual string `json:"manual"`
}

// RestartLastKnown is the evidence captured when a stage fails.
type RestartLastKnown struct {
	PIDAlive      *bool  `json:"pid_alive"`
	OldPaneExists bool   `json:"old_pane_exists"`
	NewPaneExists bool   `json:"new_pane_exists"`
	PeerStatus    string `json:"peer_status,omitempty"`
	BindingStatus string `json:"binding_status,omitempty"`
}

// RestartFailure names the stage that failed and why.
type RestartFailure struct {
	Stage     string           `json:"stage"`
	Reason    string           `json:"reason"`
	Hint      string           `json:"hint,omitempty"`
	LastKnown RestartLastKnown `json:"last_known"`
}

// RestartResult is the typed outcome shared by HTTP, MCP, CLI, and dashboard.
type RestartResult struct {
	Outcome             string          `json:"outcome"`
	RestartID           string          `json:"restart_id"`
	Stage               string          `json:"stage"`
	OldPeerID           string          `json:"old_peer_id"`
	NewPeerID           string          `json:"new_peer_id,omitempty"`
	DisplayName         string          `json:"display_name"`
	Circle              string          `json:"circle"`
	Role                proto.PeerRole  `json:"role"`
	RuntimeSessionID    string          `json:"runtime_session_id,omitempty"`
	OldPaneID           string          `json:"old_pane_id,omitempty"`
	NewPaneID           string          `json:"new_pane_id,omitempty"`
	TmuxSession         string          `json:"tmux_session,omitempty"`
	ResumeMode          string          `json:"resume_mode"`
	ResumeReason        string          `json:"resume_reason,omitempty"`
	WouldRun            RestartLaunch   `json:"would_run"`
	Defaulted           []string        `json:"defaulted"`
	Warnings            []string        `json:"warnings"`
	OpenAsks            int             `json:"open_asks"`
	Failed              *RestartFailure `json:"failed,omitempty"`
	NeedsManualRelaunch bool            `json:"needs_manual_relaunch"`
	StartedAt           string          `json:"started_at"`
	FinishedAt          string          `json:"finished_at,omitempty"`
}

// restartPlan is everything prepare learned; the goroutine owns it afterwards.
type restartPlan struct {
	peer          *proto.Peer
	proof         destructiveProof
	exitMode      string // sigterm | kill_pane | none
	pid           int
	base          string
	resumeID      string
	repowireSID   *string
	capability    map[string]any
	bindingStatus string
	spawn         service.SpawnConfig
	message       *string
	from          string
}

// restartRun is one restart; result is guarded by mu, done closes on finish.
type restartRun struct {
	mu     sync.Mutex
	result RestartResult
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *restartRun) snapshot() RestartResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.result
	out.Defaulted = append([]string{}, r.result.Defaulted...)
	out.Warnings = append([]string{}, r.result.Warnings...)
	return out
}

func (r *restartRun) update(fn func(*RestartResult)) {
	r.mu.Lock()
	fn(&r.result)
	r.mu.Unlock()
}

func (r *restartRun) active() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// restartTracker keeps the latest run per peer: in flight or finished.
type restartTracker struct {
	mu   sync.Mutex
	runs map[proto.PeerID]*restartRun
}

func (t *restartTracker) get(id proto.PeerID) *restartRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs[id]
}

// start installs run unless one is already in flight for id.
func (t *restartTracker) start(id proto.PeerID, run *restartRun) (*restartRun, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current := t.runs[id]; current != nil && current.active() {
		return current, false
	}
	if t.runs == nil {
		t.runs = map[proto.PeerID]*restartRun{}
	}
	t.runs[id] = run
	return run, true
}

func newRestartID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "rst-" + hex.EncodeToString(b[:])
}

// restartPeer runs prepare synchronously, then hands the rest to the daemon.
// Request errors (unknown/ambiguous peer, spawn not configured) are errors;
// everything after resolution is a typed RestartResult.
func (h *Hub) restartPeer(ctx context.Context, name string, req RestartPeerRequest) (RestartResult, error) {
	if h.spawn == nil || h.spawn.svc == nil || h.spawn.reg == nil {
		return RestartResult{}, &service.SpawnError{Status: 503, Detail: "spawn not configured"}
	}
	h.spawn.reg.LazyRepair(ctx)
	resolved, err := h.resolveStrict(name, req.Circle)
	if err != nil {
		return RestartResult{}, err
	}
	p := h.peerWithAdoptedOwnership(resolved)
	if run := h.spawn.restarts.get(p.PeerID); run != nil && run.active() {
		return run.snapshot(), nil
	}

	plan, result := h.prepareRestart(ctx, p, req)
	if req.DryRun {
		return result, nil
	}
	if result.Failed == nil {
		reserved, rerr := h.spawn.svc.Reserve(plan.spawn)
		if rerr != nil {
			h.failPrepare(plan, &result, &RestartFailure{Reason: "pane_reserve_failed", Hint: rerr.Error()})
		}
		plan.spawn = reserved
	}
	if result.Failed != nil {
		h.recordFinishedRestart(p.PeerID, result)
		return result, nil
	}
	result.NewPaneID = plan.spawn.Reserved.PaneID
	result.TmuxSession = plan.spawn.Reserved.TmuxSession
	result.Outcome, result.Stage = restartOutcomeInProgress, restartStageExit

	runCtx, cancel := context.WithCancel(h.spawn.ctx)
	run := &restartRun{result: result, cancel: cancel, done: make(chan struct{})}
	if current, ok := h.spawn.restarts.start(p.PeerID, run); !ok {
		cancel()
		h.spawn.svc.Tmux().KillPane(plan.spawn.Reserved.PaneID)
		return current.snapshot(), nil
	}
	h.restartEvent(ctx, "peer_restart_started", run.snapshot(), nil)
	go h.runRestart(runCtx, run, plan)

	if req.Wait != nil && !*req.Wait {
		return run.snapshot(), nil
	}
	select {
	case <-run.done:
	case <-ctx.Done(): // the caller went away (possibly by restarting itself)
	}
	return run.snapshot(), nil
}

// prepareRestart builds the plan and the planned (or prepare-failed) result.
// A dry run also previews whether the resume id validates now.
func (h *Hub) prepareRestart(ctx context.Context, p *proto.Peer, req RestartPeerRequest) (*restartPlan, RestartResult) {
	result := RestartResult{
		Outcome: restartOutcomePlanned, RestartID: newRestartID(), Stage: restartStagePrepare,
		OldPeerID: string(p.PeerID), DisplayName: string(p.DisplayName), Circle: p.Circle, Role: p.Role,
		OldPaneID: derefString(p.PaneID), Defaulted: []string{}, Warnings: []string{}, StartedAt: nowISO(),
	}
	plan, failure := h.planRestart(ctx, p, req, &result)
	if failure != nil {
		h.failPrepare(plan, &result, failure)
	} else if req.DryRun {
		h.previewResume(plan, &result)
	}
	return plan, result
}

func (h *Hub) failPrepare(plan *restartPlan, result *RestartResult, failure *RestartFailure) {
	failure.Stage = restartStagePrepare
	failure.LastKnown = h.restartEvidence(plan, result)
	result.Outcome, result.Failed, result.FinishedAt = restartOutcomeFailed, failure, nowISO()
}

// PeerRestartInfo is the restartability view `peer describe` and `peer doctor`
// show: a dry-run preview of the reconstructed launch and the last result.
type PeerRestartInfo struct {
	Restartable bool           `json:"restartable"`
	Preview     RestartResult  `json:"preview"`
	Last        *RestartResult `json:"last,omitempty"`
}

func (h *Hub) restartInfo(ctx context.Context, p *proto.Peer) *PeerRestartInfo {
	if h.spawn == nil || h.spawn.svc == nil || h.spawn.reg == nil {
		return nil
	}
	p = h.peerWithAdoptedOwnership(p)
	_, preview := h.prepareRestart(ctx, p, RestartPeerRequest{DryRun: true})
	info := &PeerRestartInfo{Restartable: preview.Failed == nil, Preview: preview}
	if run := h.spawn.restarts.get(p.PeerID); run != nil {
		last := run.snapshot()
		info.Last = &last
	}
	return info
}

// cancelRestart cancels an in-flight restart for the named peer.
func (h *Hub) cancelRestart(ctx context.Context, name string, circle *string) (RestartResult, bool, error) {
	run, err := h.restartRunFor(name, circle)
	if err != nil || run == nil {
		return RestartResult{}, false, err
	}
	if !run.active() {
		return run.snapshot(), false, nil
	}
	run.cancel()
	<-run.done
	return run.snapshot(), true, nil
}

// lastRestart returns the latest restart result for the named peer, if any.
func (h *Hub) lastRestart(name string, circle *string) (*RestartResult, error) {
	run, err := h.restartRunFor(name, circle)
	if err != nil || run == nil {
		return nil, err
	}
	snap := run.snapshot()
	return &snap, nil
}

func (h *Hub) restartRunFor(name string, circle *string) (*restartRun, error) {
	if h.spawn == nil || h.spawn.reg == nil {
		return nil, &service.SpawnError{Status: 503, Detail: "spawn not configured"}
	}
	p, err := h.resolveStrict(name, circle)
	if err != nil {
		return nil, err
	}
	return h.spawn.restarts.get(p.PeerID), nil
}

// planRestart is the prepare stage minus the pane reservation.
func (h *Hub) planRestart(ctx context.Context, p *proto.Peer, req RestartPeerRequest, result *RestartResult) (*restartPlan, *RestartFailure) {
	svc := h.spawn.svc
	plan := &restartPlan{peer: p, message: req.Message, from: restartSender}
	if req.FromPeer != nil && *req.FromPeer != "" {
		plan.from = *req.FromPeer
	}
	if p.Machine != "" && p.Machine != "unknown" && p.Machine != h.spawn.selfMachine {
		return plan, &RestartFailure{Reason: "cross_host", Hint: "Peer restart is same-host only; the peer runs on " + p.Machine + "."}
	}
	if !supportedSpawnBackend(p.Backend) {
		return plan, &RestartFailure{Reason: "unsupported_backend", Hint: string(p.Backend) + " has no launch template."}
	}

	binding := h.restartBinding(ctx, p)
	cwd := p.Path
	if binding != nil {
		plan.bindingStatus = string(binding.Status)
		if binding.ProjectPath != "" {
			cwd = binding.ProjectPath
		}
		plan.capability = binding.ResumeCapability
		plan.repowireSID = &binding.RepowireSessionID
		if id, _ := binding.ResumeCapability["runtime_session_id_arg"].(string); id != "" {
			plan.resumeID = id
		} else if binding.RuntimeSessionID != nil {
			plan.resumeID = *binding.RuntimeSessionID
		}
	}
	if plan.resumeID == "" {
		if id := runtimeSessionIDFromMetadata(p.Metadata); id != nil {
			plan.resumeID = *id
		}
	}
	if cwd == "" {
		return plan, &RestartFailure{Reason: "missing_cwd", Hint: "Neither the session binding nor the peer records a working directory."}
	}

	// Destructive authority is not guessed: a live runtime needs pane proof.
	plan.proof = h.destructivePaneProof(p)
	var oldPane *service.TmuxPaneEvidence
	if plan.proof.paneID != "" {
		oldPane = svc.Tmux().ProbePane(plan.proof.paneID)
	}
	if p.AgentPID != nil {
		plan.pid = *p.AgentPID
	}
	proc := h.spawn.proc
	pidVerified := plan.proof.ok && oldPane != nil && plan.pid > 0 && proc.Alive(plan.pid) &&
		proc.InTree(plan.pid, atoiOrZero(oldPane.PanePID))
	paneGone := plan.proof.errCode == "missing_pane" || plan.proof.errCode == "pane_not_live"
	switch {
	case pidVerified:
		plan.exitMode = "sigterm"
	case plan.proof.ok && oldPane != nil:
		plan.exitMode, plan.pid = "kill_pane", 0
	case plan.proof.ok || (paneGone && p.Status == proto.StatusOffline):
		plan.exitMode, plan.pid = "none", 0
	default:
		detail := h.paneControlErrorDetail(p, plan.proof)
		return plan, &RestartFailure{Reason: fmt.Sprint(detail["error"]), Hint: fmt.Sprint(detail["hint"])}
	}

	// Launch template: configured command (+ the spawn profile), else default.
	var profile *string
	if rec := svc.Ownership().ValidateForPeer(p); rec.OK && rec.Record != nil && rec.Record.Profile != "" {
		profile = &rec.Record.Profile
	}
	base, cerr := svc.ResolveCommand(p.Backend, profile)
	if cerr != nil {
		base = service.DefaultSpawnCommands[p.Backend]
		result.Defaulted = append(result.Defaulted, "command")
		result.Warnings = append(result.Warnings, "no usable daemon.spawn.commands entry ("+cerr.Error()+"); using the built-in "+string(p.Backend)+" template")
	}

	mode, source := "", ""
	if plan.exitMode == "sigterm" {
		if mode = service.PermissionModeFromArgs(p.Backend, proc.Args(plan.pid)); mode != "" {
			source = "argv"
		}
	}
	if mode == "" {
		for _, meta := range []map[string]any{p.Metadata, bindingMeta(binding)} {
			if m, _ := meta["permission_mode"].(string); service.ValidPermissionMode(p.Backend, m) {
				mode, source = m, "hook"
				break
			}
		}
	}
	if mode != "" {
		base = service.WithPermissionMode(base, p.Backend, mode)
	} else if mode = service.PermissionModeFromArgs(p.Backend, base); mode != "" {
		source = "template"
		result.Defaulted = append(result.Defaulted, "permission_mode")
	} else {
		mode, source = "default", "backend_default"
		result.Defaulted = append(result.Defaulted, "permission_mode")
	}
	plan.base = base

	switch {
	case req.Fresh:
		result.ResumeMode, result.ResumeReason = "fresh", "fresh requested"
	case plan.resumeID == "":
		result.ResumeMode, result.ResumeReason = "fresh", "no runtime session id recorded"
		result.Defaulted = append(result.Defaulted, "resume_id")
	default:
		result.ResumeMode = "resumed"
	}
	if req.Fresh {
		plan.resumeID = ""
	}
	result.WouldRun = RestartLaunch{
		Backend: p.Backend, CWD: service.NormPath(cwd), Command: base, PermissionMode: mode,
		PermissionModeSource: source, ResumeID: plan.resumeID, EnvKeys: svc.EnvKeys(),
	}
	applyCommand(result, plan)

	if h.spawn.asks != nil {
		open, _ := h.spawn.asks.PendingForPeer(ctx, p.PeerID, -1, "both")
		result.OpenAsks = len(open)
	}
	if p.Circle == "" {
		return plan, &RestartFailure{Reason: "missing_circle", Hint: "Peer has no circle to relaunch into."}
	}
	plan.spawn = service.SpawnConfig{
		Path: cwd, Circle: p.Circle, Backend: p.Backend, Role: p.Role, PeerID: &p.PeerID,
		TargetPane: plan.proof.paneID, SameWindow: oldPane != nil, Message: req.Message, Command: base,
	}
	if oldPane == nil {
		plan.spawn.TargetPane = ""
	}
	return plan, nil
}

// applyCommand sets would_run.command from the plan's base and resume id.
func applyCommand(r *RestartResult, plan *restartPlan) {
	r.WouldRun.Command, r.WouldRun.ResumeID = plan.base, ""
	if plan.resumeID != "" {
		if command, err := service.BuildResumeCommand(plan.base, r.WouldRun.Backend, plan.resumeID); err == nil {
			r.WouldRun.Command, r.WouldRun.ResumeID = command, plan.resumeID
		}
	}
	r.WouldRun.Manual = "cd " + service.ShellQuote(r.WouldRun.CWD) + " && " + r.WouldRun.Command
}

// previewResume reports, for a dry run, whether the resume id validates now.
func (h *Hub) previewResume(plan *restartPlan, result *RestartResult) {
	if plan.resumeID == "" {
		return
	}
	if _, ok := service.ResolveLocalResume(plan.peer.Backend, result.WouldRun.CWD, plan.resumeID, plan.repowireSID, plan.capability); !ok {
		result.Warnings = append(result.Warnings, "session file for "+plan.resumeID+" is not present yet; it is re-checked after exit and the launch goes fresh if it is still absent")
	}
}

// runRestart executes exit → wait → launch → register on the daemon context.
func (h *Hub) runRestart(ctx context.Context, run *restartRun, plan *restartPlan) {
	defer close(run.done)
	defer run.cancel()
	svc := h.spawn.svc
	reserved := plan.spawn.Reserved.PaneID
	fail := func(stage, reason, hint string, manual bool) {
		run.update(func(r *RestartResult) {
			r.Outcome, r.Stage, r.FinishedAt, r.NeedsManualRelaunch = restartOutcomeFailed, stage, nowISO(), manual
			r.Failed = &RestartFailure{Stage: stage, Reason: reason, Hint: hint, LastKnown: h.restartEvidence(plan, r)}
		})
		h.restartEvent(ctx, "peer_restart_failed", run.snapshot(), nil)
	}
	abandonReservation := func() { svc.Tmux().KillPane(reserved) }
	stage := func(name string) { run.update(func(r *RestartResult) { r.Stage = name }) }

	// exit
	switch plan.exitMode {
	case "sigterm":
		if err := h.spawn.proc.Terminate(plan.pid); err != nil {
			abandonReservation()
			fail(restartStageExit, "sigterm_failed", err.Error(), false)
			return
		}
	case "kill_pane":
		if !svc.Tmux().KillPane(plan.proof.paneID) {
			abandonReservation()
			fail(restartStageExit, "kill_pane_failed", "tmux kill-pane failed for the verified pane "+plan.proof.paneID, false)
			return
		}
	}

	// wait: the pid first (SIGTERM lets the runtime flush its session file), then the pane.
	stage(restartStageWait)
	if plan.exitMode != "none" {
		gone := func() bool {
			if plan.pid > 0 {
				return !h.spawn.proc.Alive(plan.pid)
			}
			return svc.Tmux().ProbePane(plan.proof.paneID) == nil
		}
		if err := pollUntil(ctx, restartExitTimeout, gone); err != nil {
			abandonReservation()
			if exited := gone(); exited { // cancelled after the runtime went: it needs a hand relaunch
				fail(restartStageWait, waitReason(err, "runtime_still_alive"), "cancelled after the old runtime exited; nothing was launched", true)
			} else {
				fail(restartStageWait, waitReason(err, "runtime_still_alive"), fmt.Sprintf("the old runtime had not exited after SIGTERM (bound %s); it was left as it is and nothing was relaunched", restartExitTimeout), false)
			}
			return
		}
		if plan.proof.paneID != "" {
			svc.Tmux().KillPane(plan.proof.paneID) // the runtime is gone; clear its leftover shell
			svc.Ownership().Forget(plan.proof.paneID)
			clienthooks.ClearPaneRuntimeState(plan.proof.paneID)
		}
	}
	_, _ = h.spawn.reg.MarkOffline(ctx, plan.peer.PeerID, false)

	// launch: the resume id is validated against the post-exit session file.
	stage(restartStageLaunch)
	run.update(func(r *RestartResult) {
		if plan.resumeID == "" {
			return
		}
		if _, ok := service.ResolveLocalResume(plan.peer.Backend, r.WouldRun.CWD, plan.resumeID, plan.repowireSID, plan.capability); !ok {
			r.ResumeMode, r.ResumeReason = "fresh", "session file absent after exit"
			plan.resumeID = ""
		}
		applyCommand(r, plan)
	})
	if ctx.Err() != nil {
		abandonReservation()
		fail(restartStageLaunch, "cancelled", "cancelled after the old runtime exited; nothing was launched", true)
		return
	}
	cfg := plan.spawn
	cfg.Command = run.snapshot().WouldRun.Command
	launched, err := svc.Launch(cfg)
	if err != nil {
		abandonReservation()
		fail(restartStageLaunch, "launch_failed", err.Error(), true)
		return
	}
	h.restartEvent(ctx, "peer_restart_launched", run.snapshot(), map[string]any{"pane_id": launched.PaneID})

	// register
	stage(restartStageRegister)
	var registered *proto.Peer
	err = pollUntil(ctx, restartRegisterTimeout, func() bool {
		p, ok := h.spawn.reg.GetPeerByPane(launched.PaneID)
		if ok && p.Status != proto.StatusOffline {
			registered = p
			return true
		}
		return false
	})
	if err != nil {
		fail(restartStageRegister, waitReason(err, "registration_timeout"), fmt.Sprintf("the runtime was launched in %s but did not register within %s", launched.PaneID, restartRegisterTimeout), true)
		return
	}
	var warnings []string
	if plan.peer.Role == proto.RoleOrchestrator && registered.Role != proto.RoleOrchestrator {
		if claimer, ok := h.spawn.reg.(roleClaimer); ok {
			if _, cerr := claimer.ClaimSpecialRole(ctx, string(registered.PeerID), proto.RoleOrchestrator, &registered.Circle, false); cerr != nil {
				warnings = append(warnings, "orchestrator role was not re-claimed: "+cerr.Error())
			} else {
				registered.Role = proto.RoleOrchestrator
			}
		}
	}
	run.update(func(r *RestartResult) {
		r.Outcome, r.FinishedAt = restartOutcomeRestarted, nowISO()
		r.NewPeerID, r.Role = string(registered.PeerID), registered.Role
		if id := runtimeSessionIDFromMetadata(registered.Metadata); id != nil {
			r.RuntimeSessionID = *id
		}
		r.Warnings = append(r.Warnings, warnings...)
		if r.OpenAsks > 0 {
			r.Warnings = append(r.Warnings, openAsksNote(r))
		}
	})
	result := run.snapshot()
	h.restartEvent(ctx, "peer_restart_restarted", result, nil)
	if plan.message != nil && strings.TrimSpace(*plan.message) != "" && h.ask != nil && h.ask.delivery != nil && h.ask.reg != nil {
		h.deliverSpawnSeed(ctx, launched.PaneID, result.DisplayName, plan.from, *plan.message)
	}
	h.notifyRestarted(ctx, result)
}

// notifyRestarted tells the resumed session what happened to it.
func (h *Hub) notifyRestarted(ctx context.Context, r RestartResult) {
	notify := h.spawn.notify
	if notify == nil && h.ask != nil && h.ask.delivery != nil {
		notify = func(ctx context.Context, p service.NotifyParams) error {
			_, err := h.ask.delivery.Notify(ctx, p)
			return err
		}
	}
	if notify == nil {
		return
	}
	text := fmt.Sprintf("[repowire] you were restarted (%s): old %s, new %s, resume_mode %s", r.RestartID, r.OldPeerID, r.NewPeerID, r.ResumeMode)
	if r.ResumeReason != "" {
		text += " (" + r.ResumeReason + ")"
	}
	if len(r.Defaulted) > 0 {
		text += ", defaulted [" + strings.Join(r.Defaulted, ", ") + "]"
	}
	if len(r.Warnings) > 0 {
		text += ". Warnings: " + strings.Join(r.Warnings, "; ")
	}
	if err := notify(ctx, service.NotifyParams{FromPeer: restartSender, ToPeer: r.NewPeerID, Text: text, BypassCircle: true}); err != nil {
		h.restartEvent(ctx, "peer_restart_notify_failed", r, map[string]any{"error": err.Error()})
	}
}

// openAsksNote says what happens to asks that were open across the restart.
// Asks are keyed by peer_id and non-terminal offline does not close them.
func openAsksNote(r *RestartResult) string {
	if r.NewPeerID == r.OldPeerID {
		return fmt.Sprintf("%d open asks stay open under the same peer_id; the resumed session's Stop reminder resurfaces inbound ones", r.OpenAsks)
	}
	return fmt.Sprintf("%d open asks stay open on the old peer_id %s", r.OpenAsks, r.OldPeerID)
}

func (h *Hub) recordFinishedRestart(id proto.PeerID, result RestartResult) {
	run := &restartRun{result: result, cancel: func() {}, done: make(chan struct{})}
	close(run.done)
	if _, ok := h.spawn.restarts.start(id, run); ok {
		h.restartEvent(context.Background(), "peer_restart_failed", result, nil)
	}
}

// restartEvidence captures last_known for a failure.
func (h *Hub) restartEvidence(plan *restartPlan, r *RestartResult) RestartLastKnown {
	lk := RestartLastKnown{}
	if plan == nil {
		return lk
	}
	lk.BindingStatus = plan.bindingStatus
	tmux := h.spawn.svc.Tmux()
	if r.OldPaneID != "" {
		lk.OldPaneExists = tmux.ProbePane(r.OldPaneID) != nil
	}
	if r.NewPaneID != "" {
		lk.NewPaneExists = tmux.ProbePane(r.NewPaneID) != nil
	}
	if plan.peer.AgentPID != nil && *plan.peer.AgentPID > 0 {
		alive := h.spawn.proc.Alive(*plan.peer.AgentPID)
		lk.PIDAlive = &alive
	}
	if p, ok := h.spawn.reg.GetPeer(plan.peer.PeerID); ok {
		lk.PeerStatus = string(p.Status)
	}
	return lk
}

func (h *Hub) restartEvent(ctx context.Context, kind string, r RestartResult, extra map[string]any) {
	journal, ok := h.spawn.reg.(eventRegistry)
	if !ok {
		return
	}
	data := map[string]any{
		"restart_id": r.RestartID, "peer_id": r.OldPeerID, "peer_name": r.DisplayName, "stage": r.Stage,
		"outcome": r.Outcome, "resume_mode": r.ResumeMode, "new_peer_id": r.NewPeerID, "command": r.WouldRun.Command,
	}
	if r.Failed != nil {
		data["reason"] = r.Failed.Reason
	}
	for k, v := range extra {
		data[k] = v
	}
	journal.AddEvent(context.WithoutCancel(ctx), kind, data)
}

func (h *Hub) restartBinding(ctx context.Context, p *proto.Peer) *state.SessionBinding {
	if h.store == nil {
		return nil
	}
	bindings, err := h.store.ListBindingsByPeer(ctx, string(p.PeerID))
	if err != nil {
		return nil
	}
	for _, b := range bindings {
		if b.Backend == string(p.Backend) {
			return b
		}
	}
	return nil
}

func bindingMeta(b *state.SessionBinding) map[string]any {
	if b == nil {
		return nil
	}
	return b.Metadata
}

// pollUntil checks cond every restartPollInterval until it holds, the timeout
// passes, or ctx ends. Holds no locks or connections between checks.
func pollUntil(ctx context.Context, timeout time.Duration, cond func() bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(restartPollInterval)
	defer tick.Stop()
	for {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if cond() {
				return nil
			}
			return context.DeadlineExceeded
		case <-tick.C:
		}
	}
}

func waitReason(err error, timeout string) string {
	if err == context.Canceled {
		return "cancelled"
	}
	return timeout
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// roleClaimer and eventRegistry are optional registry seams (*peer.Registry
// satisfies both); hermetic test registries may omit them.
type roleClaimer interface {
	ClaimSpecialRole(ctx context.Context, name string, role proto.PeerRole, circle *string, force bool) (*peer.ClaimResult, error)
}

type eventRegistry interface {
	AddEvent(ctx context.Context, eventType string, data map[string]any) string
}
