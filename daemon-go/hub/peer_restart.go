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
	"errors"
	"fmt"
	"slices"
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
	restartCancelWait      = 500 * time.Millisecond
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
	// Command and Manual have argument values redacted (listed in Redacted);
	// fill those from daemon.spawn.commands before running Manual by hand.
	Manual   string   `json:"manual"`
	Redacted []string `json:"redacted,omitempty"`
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
	command       string // the real launch line; results only show it redacted
	models        map[string]bool
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
	mu         sync.Mutex
	runs       map[proto.PeerID]*restartRun
	superseded map[proto.PeerID][]supersededRuntime
	closed     bool // set at shutdown: no new runs
}

// supersededRuntime is an execution a restart replaced. Its pane and pid
// fence that execution's late offline posts, which share the reused peer_id.
type supersededRuntime struct {
	paneID    string
	pid       int
	restartID string
}

func (t *restartTracker) supersede(id proto.PeerID, rt supersededRuntime) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.superseded == nil {
		t.superseded = map[proto.PeerID][]supersededRuntime{}
	}
	t.superseded[id] = append(t.superseded[id], rt)
}

// unsupersede removes the fence restartID installed for id.
func (t *restartTracker) unsupersede(id proto.PeerID, restartID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.superseded == nil {
		return
	}
	t.superseded[id] = slices.DeleteFunc(t.superseded[id], func(rt supersededRuntime) bool { return rt.restartID == restartID })
}

// supersededBy returns the restart that replaced the execution identified by
// both paneID and pid. A post missing either, or matching only one (a reused
// pid in a new pane), is not fenced. Fences live in memory for the daemon's
// lifetime; a daemon restart clears them.
func (t *restartTracker) supersededBy(id proto.PeerID, paneID string, pid int) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rt := range t.superseded[id] {
		if paneID != "" && pid > 0 && paneID == rt.paneID && pid == rt.pid {
			return rt.restartID, true
		}
	}
	return "", false
}

func (t *restartTracker) get(id proto.PeerID) *restartRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runs[id]
}

// start installs run unless one is already in flight for id (returned) or
// the tracker is closed (nil, false).
func (t *restartTracker) start(id proto.PeerID, run *restartRun) (*restartRun, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, false
	}
	if current := t.runs[id]; current != nil && current.active() {
		return current, false
	}
	if t.runs == nil {
		t.runs = map[proto.PeerID]*restartRun{}
	}
	t.runs[id] = run
	return run, true
}

// close stops intake and returns the runs still in flight.
func (t *restartTracker) close() []*restartRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	var out []*restartRun
	for _, run := range t.runs {
		if run.active() {
			out = append(out, run)
		}
	}
	return out
}

// StopRestarts closes restart intake, cancels every in-flight restart
// (waiting or not), and waits up to timeout for them to finish. It then
// returns even if some have not: a run blocked in a tmux call (itself bounded
// by service.TmuxCommand) may still be running, may write its final journal
// event after the store has closed, and so may lose that event.
func (h *Hub) StopRestarts(timeout time.Duration) {
	if h.spawn == nil || h.spawn.restarts == nil {
		return
	}
	runs := h.spawn.restarts.close()
	for _, run := range runs {
		run.cancel()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, run := range runs {
		select {
		case <-run.done:
		case <-deadline.C:
			return
		}
	}
}

// restartFencedOffline reports the restart that superseded the execution
// (pane and pid) an offline post for id comes from.
func (h *Hub) restartFencedOffline(id proto.PeerID, paneID string, pid int) (string, bool) {
	if h.spawn == nil || h.spawn.restarts == nil {
		return "", false
	}
	return h.spawn.restarts.supersededBy(id, paneID, pid)
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
	if result.Failed != nil {
		h.recordFinishedRestart(p.PeerID, result)
		return result, nil
	}
	result.Outcome = restartOutcomeInProgress

	// Installed before the pane is reserved, so a cancel during prepare finds it.
	runCtx, cancel := context.WithCancel(h.spawn.ctx)
	run := &restartRun{result: result, cancel: cancel, done: make(chan struct{})}
	if current, ok := h.spawn.restarts.start(p.PeerID, run); !ok {
		cancel()
		if current != nil {
			return current.snapshot(), nil
		}
		h.failPrepare(plan, &result, &RestartFailure{Reason: "daemon_shutting_down", Hint: "the daemon is stopping; nothing was touched"})
		return result, nil
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
	select { // a run inside a (bounded) tmux call reports cancelled when it returns
	case <-run.done:
	case <-ctx.Done():
	case <-time.After(restartCancelWait):
	}
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
	case oldPane == nil && (plan.proof.ok || paneGone):
		// Nothing visible to stop: only safe when the runtime is provably gone,
		// else a tmux probe blip would launch a second runtime on the same id.
		if p.Status != proto.StatusOffline || (plan.pid > 0 && proc.Alive(plan.pid)) {
			return plan, &RestartFailure{Reason: "runtime_alive_pane_unseen", Hint: "the peer is not offline (or its pid is alive) but its pane is not visible in tmux; refusing to launch a second runtime"}
		}
		plan.exitMode, plan.pid = "none", 0
	default:
		detail := h.paneControlErrorDetail(p, plan.proof)
		return plan, &RestartFailure{Reason: fmt.Sprint(detail["error"]), Hint: fmt.Sprint(detail["hint"])}
	}

	// Fencing needs the old execution's hook to name its pane and pid. The
	// version is the one its live socket connected with; anything not
	// provably current (no socket, no version, older) is refused.
	if plan.exitMode != "none" {
		if v, known := h.connectedHookVersion(p); !known || v < proto.CurrentHookVersion {
			shown := "unknown"
			if known {
				shown = strconv.Itoa(v)
			}
			return plan, &RestartFailure{Reason: "hook_too_old", Hint: fmt.Sprintf("this peer's connected hook (version %s) is not provably version %d, which names its pane and pid when it reports an exit; without that its exit could retire the restarted peer. Relaunch it once by hand on the current binary, then restart works.", shown, proto.CurrentHookVersion)}
		}
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

	// Permissions: only modeled argument shapes are rebuilt; anything else in
	// the permission family relaunches the observed argv verbatim.
	hookMode := ""
	for _, meta := range []map[string]any{p.Metadata, bindingMeta(binding)} {
		if m, _ := meta["permission_mode"].(string); m != "" {
			if _, ok := service.PermissionWords(p.Backend, m); ok {
				hookMode = m
				break
			}
		}
	}
	var argv []string
	exact := false
	if plan.exitMode == "sigterm" {
		argv, exact = proc.Args(plan.pid)
	}
	mode, source, words := "", "", []string(nil)
	if len(argv) > 0 {
		obs := service.ObservePermission(p.Backend, argv)
		mode, source, words = obs.Mode, "argv", obs.Words
		if obs.Mode == service.ModeArgvVerbatim && !exact && hookMode != "" {
			mode, source = hookMode, "hook"
			words, _ = service.PermissionWords(p.Backend, hookMode)
			result.Warnings = append(result.Warnings, "argv read via ps is not exact; used the recorded permission_mode")
		}
	} else if hookMode != "" {
		mode, source = hookMode, "hook"
		words, _ = service.PermissionWords(p.Backend, hookMode)
	} else {
		mode, source = service.ModeDefault, "backend_default"
		result.Defaulted = append(result.Defaulted, "permission_mode")
	}
	if mode != service.ModeArgvVerbatim {
		applied, aerr := service.ApplyPermission(base, p.Backend, words)
		switch {
		case aerr == nil:
			base = applied
		case errors.Is(aerr, service.ErrUnmodeledPermissions) && len(argv) > 0:
			mode, source = service.ModeArgvVerbatim, "argv"
		default:
			return plan, &RestartFailure{Reason: "template_permissions_unmodeled", Hint: "daemon.spawn.commands carries permission arguments restart cannot rewrite safely and no argv was observed: " + aerr.Error()}
		}
	}
	if mode == service.ModeArgvVerbatim {
		base = service.ArgvCommand(argv)
		if !exact {
			result.Warnings = append(result.Warnings, "argv read via ps loses quoting; arguments containing spaces may be split in the relaunch")
		}
	}
	plan.base, plan.models = base, svc.ConfiguredModels()

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
		Backend: p.Backend, CWD: service.NormPath(cwd), PermissionMode: mode,
		PermissionModeSource: source, EnvKeys: svc.EnvKeys(),
	}
	if err := applyCommand(result, plan); err != nil {
		return plan, &RestartFailure{Reason: "unparseable_command", Hint: err.Error()}
	}

	if h.spawn.asks != nil {
		open, _ := h.spawn.asks.PendingForPeer(ctx, p.PeerID, -1, "both")
		result.OpenAsks = len(open)
	}
	if p.Circle == "" {
		return plan, &RestartFailure{Reason: "missing_circle", Hint: "Peer has no circle to relaunch into."}
	}
	plan.spawn = service.SpawnConfig{
		Path: cwd, Circle: p.Circle, Backend: p.Backend, Role: p.Role, PeerID: &p.PeerID,
		TargetPane: plan.proof.paneID, SameWindow: oldPane != nil, Message: req.Message, Profile: derefString(profile),
	}
	if oldPane == nil {
		plan.spawn.TargetPane = ""
	}
	return plan, nil
}

// applyCommand builds plan.command from the base and resume id (any session
// args in the template are replaced; a fresh launch carries none) and shows
// it redacted in would_run.
func applyCommand(r *RestartResult, plan *restartPlan) error {
	var err error
	r.WouldRun.ResumeID = ""
	if plan.resumeID != "" {
		plan.command, err = service.BuildResumeCommand(plan.base, r.WouldRun.Backend, plan.resumeID)
		r.WouldRun.ResumeID = plan.resumeID
	} else {
		plan.command, err = service.StripResume(plan.base, r.WouldRun.Backend)
	}
	if err != nil {
		return err
	}
	r.WouldRun.Command, r.WouldRun.Redacted = service.RedactCommand(plan.command, service.RedactShown{ResumeID: plan.resumeID, Models: plan.models})
	r.WouldRun.Manual = "cd " + service.ShellQuote(r.WouldRun.CWD) + " && " + r.WouldRun.Command
	return nil
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
	reserved, restartID, launched := "", run.snapshot().RestartID, false
	fail := func(stage, reason, hint string, manual bool) {
		if !launched {
			// No replacement is pending any more: the old execution's own
			// future offline posts must count again.
			h.spawn.restarts.unsupersede(plan.peer.PeerID, restartID)
		}
		current := run.snapshot()
		evidence := h.restartEvidence(plan, &current) // tmux/ps probes run outside run.mu
		run.update(func(r *RestartResult) {
			r.Outcome, r.Stage, r.FinishedAt, r.NeedsManualRelaunch = restartOutcomeFailed, stage, nowISO(), manual
			r.Failed = &RestartFailure{Stage: stage, Reason: reason, Hint: hint, LastKnown: evidence}
		})
		h.restartEvent(ctx, "peer_restart_failed", run.snapshot(), nil)
	}
	abandonReservation := func() {
		if reserved != "" {
			svc.Tmux().KillPane(reserved)
		}
	}
	stage := func(name string) { run.update(func(r *RestartResult) { r.Stage = name }) }

	// prepare (tail): reserve the new pane beside the old one.
	reservedCfg, err := svc.Reserve(plan.spawn)
	if err != nil {
		fail(restartStagePrepare, "pane_reserve_failed", err.Error(), false)
		return
	}
	plan.spawn, reserved = reservedCfg, reservedCfg.Reserved.PaneID
	run.update(func(r *RestartResult) { r.NewPaneID, r.TmuxSession = reserved, reservedCfg.Reserved.TmuxSession })
	if ctx.Err() != nil {
		abandonReservation()
		fail(restartStagePrepare, "cancelled", "cancelled before the old runtime was touched", false)
		return
	}

	// exit. The fence on the old execution goes up BEFORE the signal (a fast
	// runtime can post its exit before Terminate returns) and comes down on any
	// failure before the replacement launches.
	stage(restartStageExit)
	if ctx.Err() != nil {
		abandonReservation()
		fail(restartStageExit, "cancelled", "cancelled before the old runtime was signalled; nothing was touched", false)
		return
	}
	if plan.exitMode != "none" {
		h.spawn.restarts.supersede(plan.peer.PeerID, supersededRuntime{paneID: plan.proof.paneID, pid: derefInt(plan.peer.AgentPID), restartID: restartID})
	}
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
			if gone() { // cancelled after the runtime went: it needs a hand relaunch
				fail(restartStageWait, waitReason(err, "runtime_still_alive"), "cancelled after the old runtime exited; nothing was launched", true)
			} else {
				fail(restartStageWait, waitReason(err, "runtime_still_alive"), fmt.Sprintf("%s was signalled but had not exited when the wait ended (bound %s); it may still exit. Nothing was relaunched: once it is gone, run would_run.manual", exitTarget(plan), restartExitTimeout), false)
			}
			return
		}
		if plan.proof.paneID != "" {
			svc.Tmux().KillPane(plan.proof.paneID) // the runtime is gone; clear its leftover shell
			svc.Ownership().Forget(plan.proof.paneID)
			clienthooks.ClearPaneRuntimeState(plan.proof.paneID)
		}
	}
	// Offline with no socket: deliveries in the gap queue durably and replay
	// when the replacement's ws-hook connects, instead of reaching the dying hook.
	_, _ = h.spawn.reg.MarkOffline(ctx, plan.peer.PeerID, false)
	if h.transport != nil {
		if n := h.transport.PendingDeliveryAcks(plan.peer.PeerID); n > 0 {
			run.update(func(r *RestartResult) {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%d deliveries were in flight at exit; they may be reported delivered without having been read", n))
			})
		}
		_ = h.transport.Close(plan.peer.PeerID)
	}

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
		_ = applyCommand(r, plan) // parsed once already in prepare
	})
	if ctx.Err() != nil {
		abandonReservation()
		fail(restartStageLaunch, "cancelled", "cancelled after the old runtime exited; nothing was launched", true)
		return
	}
	cfg := plan.spawn
	cfg.Command = plan.command
	spawned, err := svc.Launch(cfg)
	if err != nil {
		abandonReservation()
		fail(restartStageLaunch, "launch_failed", err.Error(), true)
		return
	}
	launched = true
	h.restartEvent(ctx, "peer_restart_launched", run.snapshot(), map[string]any{"pane_id": spawned.PaneID})

	// register: the registry sees the new pane AND its ws-hook is the peer's
	// transport (HTTP registration precedes the ws-hook connect).
	stage(restartStageRegister)
	var registered *proto.Peer
	err = pollUntil(ctx, restartRegisterTimeout, func() bool {
		p, ok := h.spawn.reg.GetPeerByPane(spawned.PaneID)
		if !ok || p.Status == proto.StatusOffline {
			return false
		}
		if pane, known := h.transportPane(p.PeerID); !known || pane != spawned.PaneID {
			return false
		}
		registered = p
		return true
	})
	if err != nil {
		// Launched but unverified: a hand relaunch now would run two runtimes on one session.
		fail(restartStageRegister, waitReason(err, "registration_timeout"), fmt.Sprintf("the runtime was launched in pane %s but had not registered when the wait ended (bound %s); inspect that pane and do not relaunch while it runs", spawned.PaneID, restartRegisterTimeout), false)
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
		h.deliverSpawnSeed(ctx, spawned.PaneID, result.DisplayName, plan.from, *plan.message)
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

func exitTarget(plan *restartPlan) string {
	if plan.pid > 0 {
		return fmt.Sprintf("pid %d", plan.pid)
	}
	return "pane " + plan.proof.paneID
}

// transportPane reports which pane holds id's live socket; with no transport
// wired (hermetic tests) every registration counts as connected.
func (h *Hub) transportPane(id proto.PeerID) (string, bool) {
	if h.spawn.transportPane != nil {
		return h.spawn.transportPane(id)
	}
	if h.transport == nil {
		if p, ok := h.spawn.reg.GetPeer(id); ok && p.PaneID != nil {
			return *p.PaneID, true
		}
		return "", false
	}
	return h.transport.ConnectionPaneID(id)
}

// connectedHookVersion is the hook version p's live socket connected with.
// Hermetic tests without a transport fall back to the numeric
// metadata.hook_version, which is self-reported.
func (h *Hub) connectedHookVersion(p *proto.Peer) (int, bool) {
	if h.transport != nil {
		v, ok := h.transport.ConnectionHookVersion(p.PeerID)
		return v, ok && v > 0
	}
	switch v := p.Metadata["hook_version"].(type) {
	case int:
		return v, true
	case float64:
		return int(v), true
	}
	return 0, false
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
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
