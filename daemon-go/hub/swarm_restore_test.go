package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

type swarmRig struct {
	t     *testing.T
	ctx   context.Context
	home  string
	reg   *peer.Registry
	store *state.Store
	tmux  *layoutTmux
	h     *Hub
}

func newSwarmRig(t *testing.T) *swarmRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg, err := peer.NewRegistry(ctx, memStore{}, liveSet{}, service.NewWebSocketTransport())
	if err != nil {
		t.Fatal(err)
	}
	tmux := &layoutTmux{fakeTmux: fakeTmux{evidence: map[string]*service.TmuxPaneEvidence{}}}
	h := NewHubWithTransport(reg, service.NewWebSocketTransport(), "")
	h.WithSpawn(service.NewSpawnService(tmux, service.NewFileOwnership("test-host", tmux.Probe), nil, nil), reg, nil, "test-host", proto.CircleBoundarySession)
	h.store = store
	return &swarmRig{t: t, ctx: ctx, home: home, reg: reg, store: store, tmux: tmux, h: h}
}

func (r *swarmRig) register(name, pane string, role proto.PeerRole, claim proto.PeerID) proto.PeerID {
	r.t.Helper()
	path := "/work/" + name
	params := peer.AllocateParams{Circle: "work", Backend: proto.AgentClaudeCode, Path: &path, Machine: "test-host", Role: role,
		Metadata: map[string]any{"hook_session_id": "sess-" + name}}
	if pane != "" {
		params.PaneID = &pane
	}
	if claim != "" {
		params.ClaimedPeerID = &claim
	}
	id, _, err := r.reg.AllocateAndRegister(r.ctx, params)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *swarmRig) offline(id proto.PeerID) {
	r.t.Helper()
	if _, err := r.reg.MarkOffline(r.ctx, id, false); err != nil {
		r.t.Fatal(err)
	}
}

// sessionFile makes name's Claude session resumable.
func (r *swarmRig) sessionFile(name string) {
	r.t.Helper()
	dir := service.ClaudeProjectDirs("/work/" + name)[0]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess-"+name+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func agentFor(id proto.PeerID, name string, role proto.PeerRole) *SwarmAgent {
	return &SwarmAgent{PeerID: id, DisplayName: name + "-claude-code", Circle: "work", Role: string(role), Backend: "claude-code",
		Path: "/work/" + name, Status: "offline", RuntimeSessionID: "sess-" + name, Running: true,
		Permission: permissionChoice{Mode: "plan", Source: "argv", Words: []string{"--permission-mode", "plan"}}}
}

// The plan (#505 part 2) decides each pane against the daemon's state at
// restore time. Only an agent that is offline, not closed, not a live job's
// executor, whose session no live peer holds and whose session file exists,
// is relaunched: in its registered path, with its modeled permissions and
// its backend's resume. Every other pane stays in its place as a shell, so
// the window layout still applies, and says why.
func TestSwarmPlanRelaunchesOnlyWhatCanComeBackAsItself(t *testing.T) {
	r := newSwarmRig(t)
	ok := r.register("ok", "", proto.RoleAgent, "")
	r.offline(ok)
	r.sessionFile("ok")
	online := r.register("online", "", proto.RoleAgent, "")
	r.sessionFile("online")
	closed := r.register("closed", "", proto.RoleAgent, "")
	if _, err := r.reg.ClosePeer(r.ctx, closed, "operator"); err != nil {
		t.Fatal(err)
	}
	r.sessionFile("closed")
	executor := r.register("executor", "", proto.RoleAgent, "")
	r.offline(executor)
	r.sessionFile("executor")
	assigned := string(executor)
	if _, err := r.store.CreateWork(r.ctx, state.WorkCreate{Title: "job", AssignedPeerID: &assigned}); err != nil {
		t.Fatal(err)
	}
	fork := r.register("fork", "", proto.RoleAgent, "")
	r.offline(fork)
	r.sessionFile("fork")
	holder := r.register("holder", "", proto.RoleAgent, "")
	holderID, forkPath, forkSession := string(holder), "/work/fork", "sess-fork"
	if _, err := r.store.UpsertObservation(r.ctx, state.Observation{PeerID: &holderID, Backend: "claude-code", ProjectPath: &forkPath, RuntimeSessionID: &forkSession}); err != nil {
		t.Fatal(err)
	}
	nofile := r.register("nofile", "", proto.RoleAgent, "")
	r.offline(nofile)
	verbatim := r.register("verbatim", "", proto.RoleAgent, "")
	r.offline(verbatim)
	r.sessionFile("verbatim")
	orch := r.register("orch", "", proto.RoleOrchestrator, "")

	geo := func(i int, pane string) service.PaneGeometry {
		return service.PaneGeometry{Session: "work", WindowIndex: 2, WindowName: "agents", WindowLayout: "lay", PaneIndex: i, PaneID: pane, CWD: "/pane/cwd"}
	}
	vb := agentFor(verbatim, "verbatim", proto.RoleAgent)
	vb.Permission = permissionChoice{Mode: service.ModeArgvVerbatim, Source: "argv"}
	ex := agentFor(orch, "orch", proto.RoleOrchestrator)
	ex.Exclude = "orchestrator: it has its own start path"
	m := SwarmManifest{Schema: SwarmSchema, Panes: []SwarmPane{
		{PaneGeometry: geo(0, "%1"), Agent: agentFor(ok, "ok", proto.RoleAgent)},
		{PaneGeometry: geo(1, "%2"), Agent: agentFor(online, "online", proto.RoleAgent)},
		{PaneGeometry: geo(2, "%3"), Agent: agentFor(closed, "closed", proto.RoleAgent)},
		{PaneGeometry: geo(3, "%4"), Agent: agentFor(executor, "executor", proto.RoleAgent)},
		{PaneGeometry: geo(4, "%5"), Agent: agentFor(fork, "fork", proto.RoleAgent)},
		{PaneGeometry: geo(5, "%6"), Agent: agentFor(nofile, "nofile", proto.RoleAgent)},
		{PaneGeometry: geo(6, "%7"), Agent: vb},
		{PaneGeometry: geo(7, "%8"), Agent: ex},
		{PaneGeometry: geo(8, "%9")}, // a plain shell
	}}

	plan, err := r.h.swarmPlan(r.ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Panes) != len(m.Panes) {
		t.Fatalf("plan dropped panes: %d of %d", len(plan.Panes), len(m.Panes))
	}
	launch := plan.Panes[0]
	if launch.Action != SwarmLaunch || launch.CWD != "/work/ok" || launch.PeerID != ok || launch.AwaitsFirstTurn ||
		!strings.Contains(launch.Command, "sess-ok") || !strings.Contains(launch.Command, "--permission-mode plan") {
		t.Fatalf("launch pane = %+v", launch)
	}
	wantSkip := map[int]string{1: "online now", 2: "closed by an operator", 3: "executor of live job", 4: "held by live peer " + string(holder),
		5: "no resumable session file", 6: "not modeled", 7: "orchestrator"}
	for i, want := range wantSkip {
		p := plan.Panes[i]
		if p.Action != SwarmShell || p.Command != "" || !strings.Contains(p.Skip, want) {
			t.Errorf("pane %d = action %s skip %q, want a shell skipped for %q", i, p.Action, p.Skip, want)
		}
	}
	if shell := plan.Panes[8]; shell.Action != SwarmShell || shell.CWD != "/pane/cwd" || shell.Skip != "" || shell.PeerID != "" {
		t.Fatalf("shell pane = %+v", shell)
	}
	for i, p := range plan.Panes {
		if p.PaneIndex != i || p.WindowLayout != "lay" || p.WindowIndex != 2 {
			t.Fatalf("pane %d lost its place: %+v", i, p)
		}
	}
	if _, err := r.h.swarmPlan(r.ctx, SwarmManifest{Schema: 99}); err == nil {
		t.Fatal("planned an unknown manifest schema")
	}
}

// A placed pane is recorded as spawn records one, from the daemon's own
// mapping. Verify then reports each agent: back under its own peer_id in its
// new pane, a twin (another identity in its pane), awaiting its first turn
// (Codex), or missing. It never rebinds anything.
func TestSwarmPlacedAndVerify(t *testing.T) {
	r := newSwarmRig(t)
	back := r.register("back", "", proto.RoleAgent, "")
	r.offline(back)
	twin := r.register("twin", "", proto.RoleAgent, "")
	r.offline(twin)
	codex := r.register("codex", "", proto.RoleAgent, "")
	r.offline(codex)
	missing := r.register("missing", "", proto.RoleAgent, "")
	r.offline(missing)
	for _, pane := range []string{"%20", "%21", "%22", "%23"} {
		r.tmux.evidence[pane] = &service.TmuxPaneEvidence{PaneID: pane, PanePID: "1"}
	}

	if err := r.h.swarmPlaced(SwarmPlaced{PaneID: "%20", PeerID: back, TmuxSession: "work"}); err != nil {
		t.Fatal(err)
	}
	if !r.h.spawn.svc.Ownership().IsSpawned("%20") {
		t.Fatal("the placed pane is not recorded as spawned")
	}
	if err := r.h.swarmPlaced(SwarmPlaced{PaneID: "%20", PeerID: "repow-work-nobody"}); err == nil {
		t.Fatal("recorded a pane for an unknown peer")
	}
	if err := r.h.swarmPlaced(SwarmPlaced{PaneID: "%99", PeerID: back}); err == nil {
		t.Fatal("recorded a pane that is not live")
	}

	// back re-registers in its new pane through its claim, as its binding
	// would bring it; a stranger registers in twin's pane.
	r.register("back", "%20", proto.RoleAgent, back)
	stranger := r.register("stranger", "%21", proto.RoleAgent, "")

	got := r.h.swarmVerify([]SwarmPlaced{
		{PaneID: "%20", PeerID: back}, {PaneID: "%21", PeerID: twin}, {PaneID: "%22", PeerID: codex}, {PaneID: "%23", PeerID: missing},
	}, map[proto.PeerID]bool{codex: true})
	want := []string{SwarmBack, SwarmTwin, SwarmAwaitsFirstTurn, SwarmMissing}
	for i, v := range got {
		if v.Outcome != want[i] {
			t.Errorf("%s: outcome %s, want %s", v.PeerID, v.Outcome, want[i])
		}
	}
	if got[1].Got != stranger {
		t.Fatalf("twin names %s, want %s", got[1].Got, stranger)
	}
}

// The restore routes share the snapshot's experiment: off, each answers 404
// naming the setting; on, a manifest posted to /swarm/plan comes back planned.
func TestSwarmRestoreRoutesFollowTheExperiment(t *testing.T) {
	r := newSwarmRig(t)
	post := func(enabled bool, path, body string) *httptest.ResponseRecorder {
		r.h.WithSwarmSnapshot(enabled)
		mux := http.NewServeMux()
		r.h.registerSwarmRoutes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec
	}
	for _, path := range []string{"/swarm/plan", "/swarm/placed", "/swarm/verify"} {
		if rec := post(false, path, "{}"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "experiments.swarm_snapshot") {
			t.Fatalf("%s with the experiment off: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := post(true, "/swarm/plan", `{"schema":1,"panes":[{"session":"work","window_index":1,"pane_index":0,"pane_id":"%1","cwd":"/tmp"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"action":"shell"`) {
		t.Fatalf("plan: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(true, "/swarm/plan", `{"schema":2}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown schema: %d", rec.Code)
	}
}
