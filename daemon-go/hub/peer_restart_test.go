package hub

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// fakeRuntime is tmux + the OS process table for restart tests. It records
// the pane it reserved, the command it was handed, and every signal and kill.
type fakeRuntime struct {
	mu         sync.Mutex
	evidence   map[string]*service.TmuxPaneEvidence
	nextPane   string
	created    []service.SpawnConfig
	launched   []service.SpawnConfig
	killed     []string
	alive      map[int]bool
	args       map[int]string
	paneRoot   map[int]int // pid -> pane root pid
	terminated []int
	ignoreTerm bool
	argsLossy  bool          // Args mimics macOS ps (not exact)
	dieAfter   time.Duration // >0: the runtime exits this long after SIGTERM
	onTerm     func(pid int)
	onLaunch   func(cfg service.SpawnConfig)
}

func (f *fakeRuntime) Spawn(cfg service.SpawnConfig) (service.SpawnResult, error) {
	f.mu.Lock()
	f.launched = append(f.launched, cfg)
	onLaunch := f.onLaunch
	f.mu.Unlock()
	if onLaunch != nil {
		onLaunch(cfg)
	}
	return service.SpawnResult{PaneID: cfg.Reserved.PaneID, TmuxSession: cfg.Reserved.TmuxSession, DisplayName: cfg.Reserved.DisplayName}, nil
}

func (f *fakeRuntime) CreatePane(cfg service.SpawnConfig) (service.PanePlacement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, cfg)
	f.evidence[f.nextPane] = &service.TmuxPaneEvidence{PaneID: f.nextPane, SessionName: "mesh", WindowID: "@3", WindowPanes: 2, TmuxSession: "mesh:work", PanePID: "900"}
	return service.PanePlacement{PaneID: f.nextPane, TmuxSession: "mesh:work", DisplayName: "proj"}, nil
}

func (f *fakeRuntime) KillPane(paneID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, paneID)
	delete(f.evidence, paneID)
	return true
}

func (f *fakeRuntime) ProbePane(paneID string) *service.TmuxPaneEvidence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evidence[paneID]
}

func (f *fakeRuntime) Args(pid int) ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Fields(f.args[pid]), !f.argsLossy
}

func (f *fakeRuntime) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[pid]
}

func (f *fakeRuntime) InTree(pid, root int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paneRoot[pid] == root
}

// Terminate kills the pid and, like an exec'd runtime, its pane.
func (f *fakeRuntime) Terminate(pid int) error {
	f.mu.Lock()
	f.terminated = append(f.terminated, pid)
	switch {
	case f.dieAfter > 0:
		go func() {
			time.Sleep(f.dieAfter)
			f.mu.Lock()
			f.alive[pid] = false
			delete(f.evidence, "%10")
			f.mu.Unlock()
		}()
	case !f.ignoreTerm:
		f.alive[pid] = false
		delete(f.evidence, "%10")
	}
	onTerm := f.onTerm
	f.mu.Unlock()
	if onTerm != nil {
		onTerm(pid)
	}
	return nil
}

// restartRegistry is a goroutine-safe registry for the restart sequence.
type restartRegistry struct {
	fakeSpawnRegistry
	mu      sync.Mutex
	claims  []string
	events  []map[string]any
	offline []proto.PeerID
}

func (r *restartRegistry) GetPeerByPane(paneID string) (*proto.Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fakeSpawnRegistry.GetPeerByPane(paneID)
}

func (r *restartRegistry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fakeSpawnRegistry.GetPeer(id)
}

func (r *restartRegistry) MarkOffline(_ context.Context, id proto.PeerID, _ bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offline = append(r.offline, id)
	for _, p := range r.peers {
		if p.PeerID == id {
			p.Status = proto.StatusOffline
		}
	}
	return 0, nil
}

// register re-registers p under its peer_id, as the real registry reclaims.
func (r *restartRegistry) register(p *proto.Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.peers {
		if existing.PeerID == p.PeerID {
			r.peers[i] = p
			return
		}
	}
	r.peers = append(r.peers, p)
}

func (r *restartRegistry) ResolvePeerStrict(identifier string, circle *string) []*proto.Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fakeSpawnRegistry.ResolvePeerStrict(identifier, circle)
}

func (r *restartRegistry) ClaimSpecialRole(_ context.Context, name string, role proto.PeerRole, _ *string, _ bool) (*peer.ClaimResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims = append(r.claims, name+"="+string(role))
	return &peer.ClaimResult{}, nil
}

func (r *restartRegistry) AddEvent(_ context.Context, kind string, data map[string]any) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	data["type"] = kind
	r.events = append(r.events, data)
	return ""
}

type restartFixture struct {
	h       *Hub
	rt      *fakeRuntime
	reg     *restartRegistry
	peer    *proto.Peer
	project string
	home    string
	bin     string
	srv     *httptest.Server
	notes   chan service.NotifyParams
}

const restartOldPane = "%10"

// newRestartFixture builds a live, daemon-spawned claude-code peer (pid 101
// inside pane %10 whose root is 100) and a hub wired over the fake runtime.
func newRestartFixture(t *testing.T, commands map[proto.AgentType]string) *restartFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	setRestartTimings(t, time.Second, time.Second)

	project := service.NormPath(t.TempDir())
	rt := &fakeRuntime{
		evidence: map[string]*service.TmuxPaneEvidence{restartOldPane: {PaneID: restartOldPane, SessionName: "mesh", WindowID: "@3", WindowPanes: 1, TmuxSession: "mesh:work", CurrentPath: project, PanePID: "100"}},
		nextPane: "%11",
		alive:    map[int]bool{101: true},
		args:     map[int]string{101: "claude --dangerously-skip-permissions"},
		paneRoot: map[int]int{101: 100},
	}
	own := service.NewFileOwnership("test-host", rt.ProbePane)
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	svc := service.NewSpawnService(rt, own, commands, []string{t.TempDir()}).
		WithRuntimeConfig(nil, map[string]string{"PATH": bin, "SECRET_TOKEN": "s3cr3t"})
	own.MarkSpawned(restartOldPane)

	pane, pid := restartOldPane, 101
	p := &proto.Peer{
		PeerID: "repow-mesh-old", DisplayName: "proj", Path: project, Machine: "test-host",
		Backend: proto.AgentClaudeCode, Circle: "mesh", Status: proto.StatusOnline, Role: proto.RoleAgent,
		PaneID: &pane, AgentPID: &pid, Metadata: map[string]any{"hook_session_id": "5e1f0c2a-0000-4000-8000-000000000001", "hook_version": float64(proto.CurrentHookVersion)},
	}
	reg := &restartRegistry{fakeSpawnRegistry: fakeSpawnRegistry{peers: []*proto.Peer{p}}}
	f := &restartFixture{rt: rt, reg: reg, peer: p, project: project, home: home, bin: bin, notes: make(chan service.NotifyParams, 4)}
	// The relaunched runtime registers on its new pane under the claimed peer_id.
	rt.onLaunch = func(cfg service.SpawnConfig) {
		newPane := cfg.Reserved.PaneID
		reg.register(&proto.Peer{
			PeerID: *cfg.PeerID, DisplayName: "proj", Path: cfg.Path, Backend: cfg.Backend, Circle: cfg.Circle,
			Status: proto.StatusOnline, Role: proto.RoleAgent, PaneID: &newPane, Metadata: map[string]any{"hook_session_id": "5e1f0c2a-0000-4000-8000-000000000001"},
		})
	}
	h := &Hub{}
	h.WithSpawn(svc, reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	h.spawn.proc = rt
	h.spawn.notify = func(_ context.Context, n service.NotifyParams) error {
		f.notes <- n
		return nil
	}
	f.h = h
	mux := http.NewServeMux()
	h.registerSpawnRoutes(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func setRestartTimings(t *testing.T, exit, register time.Duration) {
	t.Helper()
	prevExit, prevReg, prevPoll, prevFile := restartExitTimeout, restartRegisterTimeout, restartPollInterval, restartResumeFileWait
	restartExitTimeout, restartRegisterTimeout, restartPollInterval, restartResumeFileWait = exit, register, 5*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		restartExitTimeout, restartRegisterTimeout, restartPollInterval, restartResumeFileWait = prevExit, prevReg, prevPoll, prevFile
	})
}

func (f *restartFixture) writeClaudeSession(t *testing.T, id string) {
	t.Helper()
	dir := service.ClaudeProjectDirs(f.project)[0]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *restartFixture) writeCodexSession(t *testing.T, id string) {
	t.Helper()
	dir := filepath.Join(f.home, ".codex", "sessions", "2026", "10", "13")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ := json.Marshal(f.project)
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+id+".jsonl"), []byte(`{"type":"session_meta","payload":{"cwd":`+string(cwd)+`}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *restartFixture) restart(t *testing.T, req RestartPeerRequest) RestartResult {
	t.Helper()
	resp := postSpawnJSON(t, f.srv, "/peers/proj/restart", req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart status = %d", resp.StatusCode)
	}
	var out RestartResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *restartFixture) launchedCommands() []string {
	f.rt.mu.Lock()
	defer f.rt.mu.Unlock()
	var out []string
	for _, cfg := range f.rt.launched {
		out = append(out, cfg.Command)
	}
	return out
}

func TestRestartRelaunchesWithResumeInSameWindow(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude --dangerously-skip-permissions"})
	f.writeClaudeSession(t, "5e1f0c2a-0000-4000-8000-000000000001")
	f.rt.args[101] = "node /opt/claude/cli.js --permission-mode acceptEdits"
	f.peer.Role = proto.RoleOrchestrator

	out := f.restart(t, RestartPeerRequest{})

	if out.Outcome != restartOutcomeRestarted || out.Failed != nil {
		t.Fatalf("outcome = %s failed=%+v", out.Outcome, out.Failed)
	}
	want := "claude --permission-mode acceptEdits --resume 5e1f0c2a-0000-4000-8000-000000000001"
	if got := f.launchedCommands(); len(got) != 1 || got[0] != want {
		t.Fatalf("launched %q, want [%q]", got, want)
	}
	if out.WouldRun.Command != want || out.WouldRun.PermissionModeSource != "argv" || out.ResumeMode != "resumed" {
		t.Fatalf("would_run = %+v resume_mode=%s", out.WouldRun, out.ResumeMode)
	}
	if strings.Contains(out.WouldRun.Command, "s3cr3t") || !slices.Contains(out.WouldRun.EnvKeys, "SECRET_TOKEN") {
		t.Fatalf("env must be named, not printed: %+v", out.WouldRun)
	}
	created := f.rt.created[0]
	if !created.SameWindow || created.TargetPane != restartOldPane || created.Path != f.project {
		t.Fatalf("reserved pane cfg = %+v, want a split of %s", created, restartOldPane)
	}
	if !slices.Equal(f.rt.terminated, []int{101}) {
		t.Fatalf("terminated = %v, want SIGTERM to 101 only", f.rt.terminated)
	}
	if out.OldPeerID != "repow-mesh-old" || out.NewPeerID != "repow-mesh-old" || out.NewPaneID != "%11" || out.RuntimeSessionID != "5e1f0c2a-0000-4000-8000-000000000001" {
		t.Fatalf("ids = %+v", out)
	}
	if !slices.Equal(f.reg.claims, []string{"repow-mesh-old=orchestrator"}) || out.Role != proto.RoleOrchestrator {
		t.Fatalf("orchestrator role not re-claimed: claims=%v role=%s", f.reg.claims, out.Role)
	}
	for _, e := range f.reg.events {
		if e["restart_id"] != out.RestartID {
			t.Fatalf("journal event without restart_id: %v", e)
		}
	}
	note := <-f.notes
	if note.ToPeer != "repow-mesh-old" || !strings.Contains(note.Text, "you were restarted") || !strings.Contains(note.Text, "resume_mode resumed") {
		t.Fatalf("restart report = %+v", note)
	}
	last, err := f.h.lastRestart("proj", nil)
	if err != nil || last == nil || last.RestartID != out.RestartID {
		t.Fatalf("last restart = %+v, %v", last, err)
	}
}

// The orchestrator restarting itself: its request dies at the exit stage and
// the daemon must still finish and tell the resumed session what happened.
func TestRestartSelfRestartCompletesAfterCallerDies(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude --dangerously-skip-permissions"})
	f.writeClaudeSession(t, "5e1f0c2a-0000-4000-8000-000000000001")
	callerCtx, callerDies := context.WithCancel(context.Background())
	f.rt.onTerm = func(int) { callerDies() }

	out, err := f.h.restartPeer(callerCtx, "proj", RestartPeerRequest{FromPeer: strPtr("proj")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Outcome == restartOutcomeFailed {
		t.Fatalf("self-restart failed: %+v", out.Failed)
	}
	select {
	case note := <-f.notes:
		if !strings.Contains(note.Text, "old repow-mesh-old, new repow-mesh-old") {
			t.Fatalf("report = %q", note.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed session never received the restart report")
	}
	last, _ := f.h.lastRestart("proj", nil)
	if last.Outcome != restartOutcomeRestarted {
		t.Fatalf("final outcome = %s (%+v)", last.Outcome, last.Failed)
	}
	if got := f.launchedCommands(); len(got) != 1 || got[0] != "claude --dangerously-skip-permissions --resume 5e1f0c2a-0000-4000-8000-000000000001" {
		t.Fatalf("launched %q", got)
	}
}

func TestRestartGoesFreshWhenSessionFileAbsentAfterExit(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude --dangerously-skip-permissions"})

	out := f.restart(t, RestartPeerRequest{})

	if out.Outcome != restartOutcomeRestarted || out.ResumeMode != "fresh" || !strings.HasPrefix(out.ResumeReason, "session file absent after exit (waited ") {
		t.Fatalf("outcome=%s resume=%s (%s)", out.Outcome, out.ResumeMode, out.ResumeReason)
	}
	if got := f.launchedCommands(); len(got) != 1 || got[0] != "claude --dangerously-skip-permissions" {
		t.Fatalf("launched %q, want a fresh launch", got)
	}
}

func TestRestartWaitTimeoutLeavesOldRuntimeAndRelaunchesNothing(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	setRestartTimings(t, 30*time.Millisecond, time.Second)
	f.rt.ignoreTerm = true

	out := f.restart(t, RestartPeerRequest{})

	if out.Failed == nil || out.Failed.Stage != restartStageWait || out.Failed.Reason != "runtime_still_alive" {
		t.Fatalf("failed = %+v", out.Failed)
	}
	if out.NeedsManualRelaunch || len(f.launchedCommands()) != 0 {
		t.Fatalf("a still-alive runtime must not be relaunched: manual=%v launched=%v", out.NeedsManualRelaunch, f.launchedCommands())
	}
	if !slices.Equal(f.rt.killed, []string{"%11"}) {
		t.Fatalf("killed = %v, want only the reserved pane", f.rt.killed)
	}
	if lk := out.Failed.LastKnown; lk.PIDAlive == nil || !*lk.PIDAlive || !lk.OldPaneExists || lk.NewPaneExists {
		t.Fatalf("last_known = %+v", lk)
	}
}

func TestRestartRegisterTimeoutIsNotAManualRelaunch(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	setRestartTimings(t, time.Second, 30*time.Millisecond)
	f.rt.onLaunch = nil

	out := f.restart(t, RestartPeerRequest{})

	// Launched but unverified: a hand relaunch would duplicate the session.
	if out.Failed == nil || out.Failed.Stage != restartStageRegister || out.Failed.Reason != "registration_timeout" || out.NeedsManualRelaunch {
		t.Fatalf("failed = %+v manual=%v", out.Failed, out.NeedsManualRelaunch)
	}
	if out.WouldRun.Command == "" || out.WouldRun.CWD != f.project || !out.Failed.LastKnown.NewPaneExists {
		t.Fatalf("failure must carry the exact command and evidence: %+v", out)
	}
	if len(f.launchedCommands()) != 1 {
		t.Fatalf("launch must not be retried: %v", f.launchedCommands())
	}
}

func TestRestartRefusesUnprovenLivePane(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.h.spawn.svc.Ownership().Forget(restartOldPane)

	out := f.restart(t, RestartPeerRequest{})

	if out.Failed == nil || out.Failed.Stage != restartStagePrepare || out.Failed.Reason != "missing_pane_metadata" {
		t.Fatalf("failed = %+v", out.Failed)
	}
	if len(f.rt.created) != 0 || len(f.rt.killed) != 0 || len(f.rt.terminated) != 0 {
		t.Fatalf("prepare failure touched tmux: created=%d killed=%v term=%v", len(f.rt.created), f.rt.killed, f.rt.terminated)
	}
	last, _ := f.h.lastRestart("proj", nil)
	if last == nil || last.Failed == nil {
		t.Fatal("a failed restart must be visible to peer describe")
	}
}

func TestRestartBestGuessesMissingFields(t *testing.T) {
	// No command for claude-code, no recorded mode, no resume id, pane gone.
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentCodex: "codex"})
	f.peer.Status, f.peer.AgentPID, f.peer.Metadata = proto.StatusOffline, nil, nil
	delete(f.rt.evidence, restartOldPane)

	out := f.restart(t, RestartPeerRequest{})

	if out.Outcome != restartOutcomeRestarted {
		t.Fatalf("outcome = %s failed=%+v", out.Outcome, out.Failed)
	}
	if !slices.Equal(out.Defaulted, []string{"command", "permission_mode", "resume_id"}) {
		t.Fatalf("defaulted = %v", out.Defaulted)
	}
	// Nothing observed or recorded: no permission arguments at all, never the bypass template.
	if got := f.launchedCommands(); len(got) != 1 || got[0] != "claude" {
		t.Fatalf("launched %q", got)
	}
	if f.rt.created[0].SameWindow || len(f.rt.terminated) != 0 {
		t.Fatalf("an offline peer with no pane has nothing to stop or split: %+v term=%v", f.rt.created[0], f.rt.terminated)
	}
}

func TestRestartCancelStopsTheWait(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	setRestartTimings(t, time.Second, time.Minute)
	f.rt.onLaunch = nil

	out := f.restart(t, RestartPeerRequest{Wait: new(bool)})
	if out.Outcome != restartOutcomeInProgress {
		t.Fatalf("no-wait outcome = %s", out.Outcome)
	}
	again := f.restart(t, RestartPeerRequest{})
	if again.RestartID != out.RestartID {
		t.Fatalf("a second restart must report the one in flight, got %s vs %s", again.RestartID, out.RestartID)
	}
	req, _ := http.NewRequest(http.MethodDelete, f.srv.URL+"/peers/proj/restart", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Cancelled bool          `json:"cancelled"`
		Restart   RestartResult `json:"restart"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if !body.Cancelled || body.Restart.Failed == nil || body.Restart.Failed.Reason != "cancelled" {
		t.Fatalf("cancel = %+v", body)
	}
}

func TestRestartDryRunBuildsCodexResumeFromBinding(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentCodex: "codex --dangerously-bypass-approvals-and-sandbox"})
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.h.store = store
	f.peer.Backend, f.peer.Metadata = proto.AgentCodex, map[string]any{"hook_version": float64(2)}
	f.rt.args[101] = "codex --full-auto"
	pid, runtimeID := string(f.peer.PeerID), "019a0000-0000-7000-8000-00000000c0de"
	if _, err := store.UpsertObservation(context.Background(), state.Observation{
		PeerID: &pid, Backend: "codex", ProjectPath: &f.project, RuntimeSessionID: &runtimeID,
		ResumeCapability: map[string]any{"supported": true, "strategy": "codex_resume", "runtime_session_id_arg": runtimeID},
		Status:           state.BindingActive,
	}); err != nil {
		t.Fatal(err)
	}

	out := f.restart(t, RestartPeerRequest{DryRun: true})

	if out.Outcome != restartOutcomePlanned || out.WouldRun.Command != "codex --full-auto resume 019a0000-0000-7000-8000-00000000c0de" {
		t.Fatalf("dry run = %s %q", out.Outcome, out.WouldRun.Command)
	}
	if len(f.rt.created) != 0 || len(f.rt.terminated) != 0 {
		t.Fatal("dry run must not touch tmux or processes")
	}
}

// The resume id is validated after exit: a runtime that writes its session
// file while handling SIGTERM still resumes.
func TestRestartValidatesResumeAfterExit(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.rt.onTerm = func(int) { f.writeClaudeSession(t, "5e1f0c2a-0000-4000-8000-000000000001") }

	out := f.restart(t, RestartPeerRequest{})

	if out.ResumeMode != "resumed" || out.WouldRun.Command != "claude --dangerously-skip-permissions --resume 5e1f0c2a-0000-4000-8000-000000000001" {
		t.Fatalf("resume_mode=%s (%s) command=%q", out.ResumeMode, out.ResumeReason, out.WouldRun.Command)
	}
}

func TestRestartInfoPreviewsWithoutTouchingTheRuntime(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude --dangerously-skip-permissions"})
	f.writeClaudeSession(t, "5e1f0c2a-0000-4000-8000-000000000001")

	info := f.h.restartInfo(context.Background(), f.peer)

	if info == nil || !info.Restartable || info.Last != nil {
		t.Fatalf("info = %+v", info)
	}
	if info.Preview.WouldRun.Command != "claude --dangerously-skip-permissions --resume 5e1f0c2a-0000-4000-8000-000000000001" || info.Preview.WouldRun.PermissionModeSource != "argv" {
		t.Fatalf("preview = %+v", info.Preview.WouldRun)
	}
	if len(f.rt.created) != 0 || len(f.rt.terminated) != 0 {
		t.Fatal("describe must not reserve panes or signal processes")
	}
}

// A peer that is not provably gone but whose pane tmux cannot see must not get
// a second runtime launched beside it.
func TestRestartRefusesWhenRuntimeMayBeAliveButPaneUnseen(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	delete(f.rt.evidence, restartOldPane) // spawned-set proof stays ok; probe blips

	out := f.restart(t, RestartPeerRequest{})

	if out.Failed == nil || out.Failed.Stage != restartStagePrepare || out.Failed.Reason != "runtime_alive_pane_unseen" {
		t.Fatalf("failed = %+v", out.Failed)
	}
	if len(f.rt.created) != 0 || len(f.launchedCommands()) != 0 {
		t.Fatal("nothing may be reserved or launched")
	}
}

// The restart fences the execution it replaced: both its pane and its pid must
// match. A reused pid in another pane, or a post without both, is not fenced.
func TestRestartFencesOnlyTheReplacedExecution(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	if _, fenced := f.h.restartFencedOffline(f.peer.PeerID, restartOldPane, 101); fenced {
		t.Fatal("nothing is fenced before a restart")
	}
	out := f.restart(t, RestartPeerRequest{})
	if out.Outcome != restartOutcomeRestarted {
		t.Fatalf("outcome = %s", out.Outcome)
	}
	for _, c := range []struct {
		pane string
		pid  int
		want bool
	}{
		{restartOldPane, 101, true},
		{restartOldPane, 0, false}, {"", 101, false}, {"%11", 101, false}, {restartOldPane, 102, false},
		{"%11", 0, false}, {"", 0, false},
	} {
		id, fenced := f.h.restartFencedOffline(proto.PeerID(out.NewPeerID), c.pane, c.pid)
		if fenced != c.want || (fenced && id != out.RestartID) {
			t.Errorf("pane=%q pid=%d fenced=%v (%s), want %v", c.pane, c.pid, fenced, id, c.want)
		}
	}
}

func TestRestartKeepsTheSpawnProfile(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.h.spawn.svc.WithRuntimeConfig(map[proto.AgentType]map[string][]string{proto.AgentClaudeCode: {"deep": {"--model", "opus"}}}, map[string]string{"PATH": f.bin})
	id := string(f.peer.PeerID)
	f.h.spawn.svc.Ownership().Record(service.OwnershipRecord{PaneID: restartOldPane, Path: f.project, Backend: "claude-code", Circle: "mesh", Role: "agent", TmuxSession: "mesh:work", Machine: "test-host", PeerID: &id, Profile: "deep"})

	out := f.restart(t, RestartPeerRequest{})

	if got := f.launchedCommands(); len(got) != 1 || got[0] != "claude --model opus --dangerously-skip-permissions" {
		t.Fatalf("launched %q (%+v)", got, out.Failed)
	}
	if cfg := f.rt.launched[0]; cfg.Profile != "deep" {
		t.Fatalf("the relaunched pane must keep the profile for the next restart, got %q", cfg.Profile)
	}
}

func TestStopRestartsCancelsInFlightRuns(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	setRestartTimings(t, time.Second, time.Minute)
	f.rt.onLaunch = nil
	f.restart(t, RestartPeerRequest{Wait: new(bool)})

	f.h.StopRestarts(time.Second)

	last, _ := f.h.lastRestart("proj", nil)
	if last.Outcome != restartOutcomeFailed || last.Failed.Reason != "cancelled" {
		t.Fatalf("after shutdown: %+v", last)
	}
}

func TestRestartCancelledDaemonNeverSignals(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.h.WithSpawnContext(ctx)

	out := f.restart(t, RestartPeerRequest{})

	if len(f.rt.terminated) != 0 || len(f.launchedCommands()) != 0 {
		t.Fatalf("a cancelled restart signalled %v / launched %v", f.rt.terminated, f.launchedCommands())
	}
	if out.Failed == nil || out.Failed.Reason != "cancelled" || out.NeedsManualRelaunch {
		t.Fatalf("failed = %+v manual=%v", out.Failed, out.NeedsManualRelaunch)
	}
	if !slices.Equal(f.rt.killed, []string{"%11"}) {
		t.Fatalf("killed = %v, want only the reserved pane", f.rt.killed)
	}
}

func TestRestartFreshStripsTemplateResume(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude --resume old-session --continue"})
	f.writeClaudeSession(t, "5e1f0c2a-0000-4000-8000-000000000001")

	if out := f.restart(t, RestartPeerRequest{Fresh: true}); out.ResumeMode != "fresh" {
		t.Fatalf("resume_mode = %s", out.ResumeMode)
	}
	if got := f.launchedCommands(); len(got) != 1 || got[0] != "claude --dangerously-skip-permissions" {
		t.Fatalf("fresh launch %q must carry no session args", got)
	}
}

// Any permission argument outside the model relaunches the observed argv
// verbatim; it never falls back to the (bypass) template.
func TestRestartRelaunchesUnmodeledPermissionsArgvVerbatim(t *testing.T) {
	for _, argv := range []string{
		"codex --sandbox read-only --ask-for-approval untrusted",
		"codex --full-auto",
		"codex -c approval_policy=never -m gpt-5",
	} {
		f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentCodex: "codex --dangerously-bypass-approvals-and-sandbox resume old"})
		f.peer.Backend, f.peer.Metadata = proto.AgentCodex, map[string]any{"hook_session_id": "019a0000-0000-7000-8000-00000000c0de", "hook_version": float64(2)}
		f.rt.args[101] = argv
		f.rt.onLaunch = nil
		f.writeCodexSession(t, "019a0000-0000-7000-8000-00000000c0de")
		setRestartTimings(t, time.Second, 10*time.Millisecond)

		out := f.restart(t, RestartPeerRequest{})

		if out.WouldRun.PermissionMode != service.ModeArgvVerbatim || out.WouldRun.PermissionModeSource != "argv" {
			t.Fatalf("%q: mode = %s (%s)", argv, out.WouldRun.PermissionMode, out.WouldRun.PermissionModeSource)
		}
		if got := f.launchedCommands(); len(got) != 1 || got[0] != service.ArgvCommand(strings.Fields(argv))+" resume 019a0000-0000-7000-8000-00000000c0de" {
			t.Fatalf("%q: launched %q", argv, got)
		}
	}
}

func TestRestartRebuildsModeledCodexPermissions(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentCodex: "codex --dangerously-bypass-approvals-and-sandbox -m gpt-5"})
	f.peer.Backend, f.peer.Metadata = proto.AgentCodex, map[string]any{"hook_session_id": "019a0000-0000-7000-8000-00000000c0de", "hook_version": float64(2)}
	f.rt.args[101] = "codex -s read-only -a never"

	out := f.restart(t, RestartPeerRequest{DryRun: true})

	if out.WouldRun.PermissionMode != "sandbox=read-only,approval=never" || out.WouldRun.Command != "codex -m gpt-5 -s read-only -a never resume 019a0000-0000-7000-8000-00000000c0de" {
		t.Fatalf("mode=%s command=%q", out.WouldRun.PermissionMode, out.WouldRun.Command)
	}
}

func TestRestartCodexWithoutObservationLaunchesNoPermissionFlags(t *testing.T) {
	for _, meta := range []map[string]any{nil, {"permission_mode": "default"}} {
		f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentCodex: service.DefaultSpawnCommands[proto.AgentCodex]})
		f.peer.Backend, f.peer.Status, f.peer.AgentPID, f.peer.Metadata = proto.AgentCodex, proto.StatusOffline, nil, meta
		delete(f.rt.evidence, restartOldPane)

		out := f.restart(t, RestartPeerRequest{DryRun: true})

		if out.WouldRun.Command != "codex" || out.WouldRun.PermissionMode != service.ModeDefault {
			t.Fatalf("meta=%v: command=%q mode=%s", meta, out.WouldRun.Command, out.WouldRun.PermissionMode)
		}
	}
}

// With lossy (ps) argv and a recorded mode, an unmodeled set prefers the mode.
func TestRestartPrefersRecordedModeOverLossyVerbatimArgv(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.rt.args[101] = "claude --allowedTools Bash(git log) Edit"
	f.rt.argsLossy = true
	f.peer.Metadata["permission_mode"] = "plan"

	out := f.restart(t, RestartPeerRequest{DryRun: true})

	if out.WouldRun.PermissionMode != "plan" || out.WouldRun.PermissionModeSource != "hook" {
		t.Fatalf("mode=%s source=%s", out.WouldRun.PermissionMode, out.WouldRun.PermissionModeSource)
	}
	if !slices.Contains(out.Warnings, "argv read via ps is not exact; used the recorded permission_mode") {
		t.Fatalf("warnings = %q", out.Warnings)
	}
}

// The hook version gate refuses anything not provably current: missing,
// non-numeric, or older.
func TestRestartRefusesHookTooOldToFence(t *testing.T) {
	for _, v := range []any{nil, "2", float64(1)} {
		f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
		f.peer.Metadata["hook_version"] = v

		out := f.restart(t, RestartPeerRequest{})

		if out.Failed == nil || out.Failed.Stage != restartStagePrepare || out.Failed.Reason != "hook_too_old" || !strings.Contains(out.Failed.Hint, "by hand") {
			t.Fatalf("hook_version=%v: failed = %+v", v, out.Failed)
		}
		if len(f.rt.terminated) != 0 || len(f.rt.created) != 0 {
			t.Fatalf("hook_version=%v: nothing may be touched", v)
		}
	}
}

// With a transport, the version is the one the live socket connected with,
// not editable peer metadata.
func TestRestartHookVersionComesFromTheSocket(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.h.transport = service.NewWebSocketTransport() // the peer has no live socket
	f.peer.Metadata["hook_version"] = float64(99)

	out := f.restart(t, RestartPeerRequest{DryRun: true})

	if out.Failed == nil || out.Failed.Reason != "hook_too_old" {
		t.Fatalf("metadata alone must not pass the gate: %+v", out.Failed)
	}
}

func TestRestartFencesOnlyAfterASuccessfulSignal(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.h.spawn.proc = failingTerminate{f.rt}

	out := f.restart(t, RestartPeerRequest{})

	if out.Failed == nil || out.Failed.Reason != "sigterm_failed" {
		t.Fatalf("failed = %+v", out.Failed)
	}
	if _, fenced := f.h.restartFencedOffline(f.peer.PeerID, restartOldPane, 101); fenced {
		t.Fatal("a runtime that was never signalled must not be fenced; its genuine death must count")
	}
}

type failingTerminate struct{ *fakeRuntime }

func (failingTerminate) Terminate(int) error { return os.ErrPermission }

func TestRestartRedactsCommandValuesInResultAndJournal(t *testing.T) {
	const marker = "SYNTHETIC_REVIEW_SECRET"
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude --settings '{\"env\":{\"TOKEN\":\"" + marker + "\"}}'"})
	f.writeClaudeSession(t, "5e1f0c2a-0000-4000-8000-000000000001")

	out := f.restart(t, RestartPeerRequest{})

	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), marker) || !slices.Contains(out.WouldRun.Redacted, "--settings") {
		t.Fatalf("result leaks or misses redaction: %s", raw)
	}
	for _, e := range f.reg.events {
		if b, _ := json.Marshal(e); strings.Contains(string(b), marker) {
			t.Fatalf("journal leaks: %s", b)
		}
	}
	if got := f.launchedCommands(); len(got) != 1 || !strings.Contains(got[0], marker) {
		t.Fatalf("the real launch must keep the value: %q", got)
	}
}

func TestRestartCancelIsBoundedWhileLaunchBlocks(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	prev := restartCancelWait
	restartCancelWait = 50 * time.Millisecond
	t.Cleanup(func() { restartCancelWait = prev })
	started, release := make(chan struct{}), make(chan struct{})
	f.rt.onLaunch = func(service.SpawnConfig) { close(started); <-release }
	f.restart(t, RestartPeerRequest{Wait: new(bool)})
	<-started

	begin := time.Now()
	_, cancelled, err := f.h.cancelRestart(context.Background(), "proj", nil)
	if err != nil || !cancelled || time.Since(begin) > restartCancelWait+100*time.Millisecond {
		t.Fatalf("cancel = %v, %v after %s", cancelled, err, time.Since(begin))
	}
	close(release)
	run := f.h.spawn.restarts.get(f.peer.PeerID)
	<-run.done
	if last := run.snapshot(); last.Failed == nil || last.Failed.Stage != restartStageRegister || last.NeedsManualRelaunch {
		t.Fatalf("a cancel that lands mid-launch reports launched-unverified: %+v", last.Failed)
	}
}

// HTTP registration precedes the ws-hook connect: the restart is not done (and
// the report is not sent) until the new pane holds the peer's socket.
func TestRestartWaitsForTheNewTransport(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	setRestartTimings(t, time.Second, 40*time.Millisecond)
	f.h.spawn.transportPane = func(proto.PeerID) (string, bool) { return restartOldPane, true }

	out := f.restart(t, RestartPeerRequest{})

	if out.Failed == nil || out.Failed.Stage != restartStageRegister {
		t.Fatalf("registered without the new socket: %+v", out)
	}
	select {
	case n := <-f.notes:
		t.Fatalf("report sent before the new transport connected: %+v", n)
	default:
	}
}

// A fast runtime can post its exit before Terminate returns: the fence must
// already be up when the signal is sent.
func TestRestartFencesBeforeTheSignalLands(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	during := false
	f.rt.onTerm = func(int) { _, during = f.h.restartFencedOffline(f.peer.PeerID, restartOldPane, 101) }

	if out := f.restart(t, RestartPeerRequest{}); out.Outcome != restartOutcomeRestarted {
		t.Fatalf("outcome = %s", out.Outcome)
	}
	_, after := f.h.restartFencedOffline(f.peer.PeerID, restartOldPane, 101)
	if !during || !after {
		t.Fatalf("fenced during signal=%v after=%v, want both", during, after)
	}
}

// A runtime that survived SIGTERM and was not replaced is not fenced: its
// genuine later death must count.
func TestRestartFailedWaitRemovesTheFence(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	setRestartTimings(t, 20*time.Millisecond, time.Second)
	f.rt.ignoreTerm = true

	out := f.restart(t, RestartPeerRequest{})

	if out.Failed == nil || out.Failed.Stage != restartStageWait || len(f.launchedCommands()) != 0 {
		t.Fatalf("failed = %+v launched=%v", out.Failed, f.launchedCommands())
	}
	if _, fenced := f.h.restartFencedOffline(f.peer.PeerID, restartOldPane, 101); fenced {
		t.Fatal("a failed wait must remove the fence")
	}
}

func TestRestartPromptAfterEndOfOptionsIsNotAPermission(t *testing.T) {
	for backend, argv := range map[proto.AgentType]string{
		proto.AgentClaudeCode: "claude -- --dangerously-skip-permissions",
		proto.AgentCodex:      "codex -- --dangerously-bypass-approvals-and-sandbox",
	} {
		prog := strings.Fields(argv)[0]
		f := newRestartFixture(t, map[proto.AgentType]string{backend: prog})
		f.peer.Backend = backend
		f.rt.args[101] = argv

		out := f.restart(t, RestartPeerRequest{DryRun: true})

		if out.WouldRun.PermissionMode != service.ModeDefault || strings.Contains(out.WouldRun.Command, "dangerously") {
			t.Fatalf("%q: mode=%s command=%q", argv, out.WouldRun.PermissionMode, out.WouldRun.Command)
		}
	}
}

func TestRestartWarnsAboutDeliveriesInFlightAtExit(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	tr := service.NewWebSocketTransport()
	id := f.peer.PeerID
	connected := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		tr.Connect(context.Background(), &service.ConnectionInfo{SessionID: id, WS: c, HookVersion: proto.CurrentHookVersion})
		close(connected)
		for {
			var data map[string]any
			if wsjson.Read(context.Background(), c, &data) != nil {
				return
			}
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	<-connected
	sent := make(chan struct{})
	go func() {
		_, _ = tr.SendAndWaitDeliveryAck(ctx, id, map[string]any{"delivery_id": "in-flight", "type": "notify"}, time.Second)
		close(sent)
	}()
	var frame map[string]any
	if err := wsjson.Read(ctx, client, &frame); err != nil {
		t.Fatal(err)
	}
	client.CloseRead(ctx)
	f.h.transport = tr
	f.h.spawn.transportPane = func(proto.PeerID) (string, bool) { return "%11", true }

	out := f.restart(t, RestartPeerRequest{})

	if !slices.Contains(out.Warnings, "1 deliveries were in flight at exit; they may be reported delivered without having been read") {
		t.Fatalf("warnings = %q", out.Warnings)
	}
	<-sent
}

// Smoke regression (rst-9508dc315aca): the cwd contains a dot, which Claude
// Code's project slug turns into "-", and the runtime writes its session file
// a second after SIGTERM. Restart must find it and resume.
func TestRestartResumesWhenSessionFileAppearsAfterExitUnderClaudeSlug(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	dotted := filepath.Join(f.project, "repowire.smoke-restart")
	if err := os.Mkdir(dotted, 0o755); err != nil {
		t.Fatal(err)
	}
	f.project, f.peer.Path = dotted, dotted
	f.rt.evidence[restartOldPane].CurrentPath = dotted
	prev := restartResumeFileWait
	restartResumeFileWait = 3 * time.Second
	t.Cleanup(func() { restartResumeFileWait = prev })
	id := "5e1f0c2a-0000-4000-8000-000000000001"
	f.rt.onTerm = func(int) {
		go func() {
			time.Sleep(time.Second)
			f.writeClaudeSession(t, id)
		}()
	}

	out := f.restart(t, RestartPeerRequest{})

	if out.ResumeMode != "resumed" || out.ResumeCheck == nil || out.ResumeCheck.WaitedMS < 900 {
		t.Fatalf("resume_mode=%s (%s) check=%+v", out.ResumeMode, out.ResumeReason, out.ResumeCheck)
	}
	wantDir := filepath.Join(f.home, ".claude", "projects", strings.ReplaceAll(strings.ReplaceAll(dotted, "/", "-"), ".", "-"))
	if out.ResumeCheck.FoundPath != filepath.Join(wantDir, id+".jsonl") || !strings.Contains(out.ResumeCheck.CheckedPaths[0], "repowire-smoke-restart") {
		t.Fatalf("check = %+v, want the dot slugged to -", out.ResumeCheck)
	}
	if got := f.launchedCommands(); len(got) != 1 || !strings.HasSuffix(got[0], "--resume "+id) {
		t.Fatalf("launched %q", got)
	}
}

// Smoke regression (rst-b8046b02ad48): a cancel right after SIGTERM must not
// kill the reserved pane (the window's survivor once the old runtime exits),
// must report the state after the exit, and must not let the old runtime's
// own exit posts retire the peer.
func TestRestartCancelAfterSignalKeepsTheWindowAndReportsTheEndState(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	f.rt.dieAfter = 150 * time.Millisecond
	f.rt.onTerm = func(int) { go f.h.cancelRestart(context.Background(), "proj", nil) }

	f.restart(t, RestartPeerRequest{Wait: new(bool)})
	run := f.h.spawn.restarts.get(f.peer.PeerID)
	<-run.done
	out := run.snapshot()

	if out.Failed == nil || out.Failed.Stage != restartStageWait || out.Failed.Reason != "cancelled" || !out.NeedsManualRelaunch || !out.CancelRequested {
		t.Fatalf("result = %+v manual=%v", out.Failed, out.NeedsManualRelaunch)
	}
	if f.rt.ProbePane("%11") == nil || slices.Contains(f.rt.killed, "%11") {
		t.Fatalf("the reserved pane must survive as the window's shell; killed=%v", f.rt.killed)
	}
	lk := out.Failed.LastKnown
	if lk.PIDAlive == nil || *lk.PIDAlive != f.rt.Alive(101) || lk.OldPaneExists != (f.rt.ProbePane(restartOldPane) != nil) || !lk.NewPaneExists {
		t.Fatalf("last_known %+v does not match the end state (pid alive=%v)", lk, f.rt.Alive(101))
	}
	if *lk.PIDAlive || lk.OldPaneExists {
		t.Fatalf("the old runtime had exited: %+v", lk)
	}
	if _, fenced := f.h.restartFencedOffline(f.peer.PeerID, restartOldPane, 101); !fenced {
		t.Fatal("the stopped runtime's own exit posts must stay fenced so they cannot retire the peer")
	}
	if !slices.Contains(f.reg.offline, f.peer.PeerID) || len(f.launchedCommands()) != 0 {
		t.Fatalf("offline=%v launched=%v", f.reg.offline, f.launchedCommands())
	}
}

// Smoke step 9: a codex app-server peer has no hook and no agent pid. It must
// pass the execution-identity gate, take the runtime pid from the pane root,
// and get a full preview.
func TestRestartCodexAppServerPeerPassesTheGateWithoutAHook(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentCodex: "codex --dangerously-bypass-approvals-and-sandbox"})
	f.peer.Backend, f.peer.AgentPID = proto.AgentCodex, nil
	f.peer.Provenance = proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true}
	f.peer.Metadata = map[string]any{"runtime_session_id": "019a0000-0000-7000-8000-00000000c0de", "transport": "codex-app-server"}
	f.rt.alive[100] = true
	f.rt.args[100] = "codex --dangerously-bypass-approvals-and-sandbox"
	f.rt.paneRoot[100] = 100

	out := f.restart(t, RestartPeerRequest{DryRun: true})

	if out.Outcome != restartOutcomePlanned || out.Failed != nil {
		t.Fatalf("outcome = %s failed=%+v", out.Outcome, out.Failed)
	}
	if out.WouldRun.PermissionModeSource != "argv" || out.WouldRun.Command != "codex --dangerously-bypass-approvals-and-sandbox resume 019a0000-0000-7000-8000-00000000c0de" {
		t.Fatalf("would_run = %+v", out.WouldRun)
	}
}

// A refusal keeps the reconstructed launch beside it.
func TestRestartRefusalStillShowsThePreview(t *testing.T) {
	f := newRestartFixture(t, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"})
	delete(f.peer.Metadata, "hook_version")

	out := f.restart(t, RestartPeerRequest{DryRun: true})

	if out.Failed == nil || out.Failed.Reason != "hook_too_old" {
		t.Fatalf("failed = %+v", out.Failed)
	}
	if out.WouldRun.Command == "" || out.WouldRun.PermissionMode == "" || out.WouldRun.PermissionModeSource == "" {
		t.Fatalf("preview missing beside the refusal: %+v", out.WouldRun)
	}
}
