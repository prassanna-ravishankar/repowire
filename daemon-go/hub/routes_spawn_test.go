package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// ---------------------------------------------------------------------------
// Fakes for the spawn route deps.
// ---------------------------------------------------------------------------

// fakeSpawnRegistry satisfies spawnRegistry + turnStateRegistry over an in-memory
// peer slice. ResolvePeerStrict returns all display-name/peer-id matches so the
// route can apply its 0/1/N policy.
type fakeSpawnRegistry struct {
	peers        []*proto.Peer
	unregistered []string
	allocID      proto.PeerID
	allocName    proto.DisplayName
	turnStates   map[proto.PeerID]proto.TurnState
}

func (r *fakeSpawnRegistry) LazyRepair(context.Context) {}

func (r *fakeSpawnRegistry) ResolvePeerStrict(identifier string, circle *string) []*proto.Peer {
	var out []*proto.Peer
	for _, p := range r.peers {
		if string(p.PeerID) == identifier || string(p.DisplayName) == identifier {
			if circle != nil && p.Circle != *circle {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

func (r *fakeSpawnRegistry) AllocateAndRegister(_ context.Context, _ peer.AllocateParams) (proto.PeerID, proto.DisplayName, error) {
	return r.allocID, r.allocName, nil
}

func (r *fakeSpawnRegistry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	for _, p := range r.peers {
		if p.PeerID == id {
			return p, true
		}
	}
	return nil, false
}

func (r *fakeSpawnRegistry) UnregisterPeer(_ context.Context, identifier string, _ *string) (bool, error) {
	r.unregistered = append(r.unregistered, identifier)
	return true, nil
}

func (r *fakeSpawnRegistry) MarkOffline(context.Context, proto.PeerID, bool) (int, error) {
	return 0, nil
}

func (r *fakeSpawnRegistry) UpdateTurnState(_ context.Context, id proto.PeerID, ts proto.TurnState) {
	if r.turnStates == nil {
		r.turnStates = map[proto.PeerID]proto.TurnState{}
	}
	r.turnStates[id] = ts
}

// fakeTmux records kill calls and returns a scripted Spawn result / pane evidence.
type fakeTmux struct {
	spawnResult service.SpawnResult
	spawnErr    error
	spawnConfig service.SpawnConfig
	killed      []string
	killOK      bool
	evidence    map[string]*service.TmuxPaneEvidence
}

func (f *fakeTmux) Spawn(cfg service.SpawnConfig) (service.SpawnResult, error) {
	f.spawnConfig = cfg
	return f.spawnResult, f.spawnErr
}
func (f *fakeTmux) KillPane(paneID string) bool {
	f.killed = append(f.killed, paneID)
	return f.killOK
}
func (f *fakeTmux) ProbePane(paneID string) *service.TmuxPaneEvidence { return f.evidence[paneID] }

func newHermeticSpawnService(t *testing.T, tmux *fakeTmux, own service.PaneOwnership, commands map[proto.AgentType]string, allowedPaths []string) *service.SpawnService {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return service.NewSpawnService(tmux, own, commands, allowedPaths).WithRuntimeConfig(nil, map[string]string{"PATH": bin})
}

// newSpawnTestHub wires the spawn deps over fakes + an httptest server. The
// service.SpawnService uses an in-memory PaneOwnership (REPOWIRE_CONFIG_DIR sandboxed to a
// temp dir so the on-disk JSON never touches the real ~/.repowire).
func newSpawnTestHub(t *testing.T, reg *fakeSpawnRegistry, tmux *fakeTmux) (*httptest.Server, *service.SpawnService) {
	t.Helper()
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())

	own := service.NewFileOwnership("test-host", tmux.ProbePane)
	svc := newHermeticSpawnService(t, tmux, own,
		map[proto.AgentType]string{proto.AgentClaudeCode: "claude", proto.AgentCodex: "codex"},
		[]string{t.TempDir()}, // an allowed root; spawn-enabled
	)
	asks := service.NewAskTracker(0)
	h := &Hub{authToken: ""}
	h.WithSpawn(svc, reg, asks, "test-host", proto.CircleBoundarySession)

	mux := http.NewServeMux()
	h.registerSpawnRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, svc
}

func postSpawnJSON(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	resp, err := http.Post(srv.URL+path, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func TestWindowBoundarySpawnUsesSourcePaneEvidence(t *testing.T) {
	root, pane := t.TempDir(), "%12"
	tmux := &fakeTmux{
		spawnResult: service.SpawnResult{DisplayName: "worker", TmuxSession: "mesh:work", PaneID: "%13"},
		evidence: map[string]*service.TmuxPaneEvidence{
			pane: {PaneID: pane, SessionName: "mesh", WindowID: "@7", TmuxSession: "mesh:work", CurrentPath: root},
			"@7": {PaneID: pane, SessionName: "mesh", WindowID: "@7", TmuxSession: "mesh:work", CurrentPath: root},
		},
	}
	own := service.NewFileOwnership("test-host", tmux.ProbePane)
	svc := newHermeticSpawnService(t, tmux, own, map[proto.AgentType]string{proto.AgentClaudeCode: "claude"}, []string{root})
	h := &Hub{}
	h.WithSpawn(svc, &fakeSpawnRegistry{}, service.NewAskTracker(0), "test-host", proto.CircleBoundaryWindow)

	result, err := h.spawnPeer(context.Background(), SpawnRequest{Path: root, Backend: agentTypePtr(proto.AgentClaudeCode), SourcePane: pane})
	if err != nil || !result.OK {
		t.Fatalf("window spawn failed: result=%+v err=%v", result, err)
	}
	if tmux.spawnConfig.Circle != "window-7" || tmux.spawnConfig.TargetPane != "@7" || tmux.spawnConfig.CircleBoundary != proto.CircleBoundaryWindow {
		t.Fatalf("spawn config = %+v", tmux.spawnConfig)
	}

	_, err = h.spawnPeer(context.Background(), SpawnRequest{Path: root, Backend: agentTypePtr(proto.AgentClaudeCode), SourcePane: pane, Circle: "other"})
	if se, ok := service.AsSpawnError(err); !ok || se.Status != http.StatusConflict {
		t.Fatalf("circle mismatch error = %v, want 409", err)
	}
	_, err = h.spawnPeer(context.Background(), SpawnRequest{Path: root, Backend: agentTypePtr(proto.AgentClaudeCode)})
	if se, ok := service.AsSpawnError(err); !ok || se.Status != http.StatusUnprocessableEntity {
		t.Fatalf("missing source pane error = %v, want 422", err)
	}
	if _, err = h.spawnPeer(context.Background(), SpawnRequest{Path: root, Backend: agentTypePtr(proto.AgentClaudeCode), Circle: "window-7"}); err != nil {
		t.Fatalf("explicit live window circle: %v", err)
	}
}

func agentTypePtr(value proto.AgentType) *proto.AgentType { return &value }

// strp is the shared test helper (hub_test.go).

// ---------------------------------------------------------------------------
// Primary path: POST /kill-peer destructive-proof truth table.
// ---------------------------------------------------------------------------

// TestKillPeerSpawnedPaneKillsAndUnregisters is the primary endpoint test: a peer
// whose pane is in the in-process spawned set is destructively-provable, so
// /kill-peer kills the pane, forgets ownership, unregisters the peer, and reports
// tmux_killed=true.
func TestKillPeerSpawnedPaneKillsAndUnregisters(t *testing.T) {
	pane := "%42"
	p := &proto.Peer{
		PeerID:      "repow-default-aaaa",
		DisplayName: "proj-claude-code",
		Backend:     proto.AgentClaudeCode,
		Circle:      "default",
		Role:        proto.RoleAgent,
		PaneID:      &pane,
		TmuxSession: strp("default:proj"),
	}
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{p}}
	tmux := &fakeTmux{killOK: true}
	srv, svc := newSpawnTestHub(t, reg, tmux)

	// Mark the pane as daemon-spawned this life — the strongest destructive proof.
	svc.Ownership().MarkSpawned(pane)

	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: string(p.PeerID)})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kill-peer status = %d, want 200", resp.StatusCode)
	}
	var out KillResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.TmuxKilled == nil || !*out.TmuxKilled {
		t.Fatalf("tmux_killed = %v, want true", out.TmuxKilled)
	}
	if len(tmux.killed) != 1 || tmux.killed[0] != pane {
		t.Fatalf("killed panes = %v, want [%s]", tmux.killed, pane)
	}
	if len(reg.unregistered) != 1 {
		t.Fatalf("unregistered = %v, want one peer", reg.unregistered)
	}
}

// TestKillPeerNoProofUnregistersButSkipsKill enforces the fail-loud rule: a peer
// with a pane id but NO durable proof and NOT in the spawned set (path alone is
// never proof) is unregistered, but the pane is left untouched and tmux_killed is
// null. The pane has no tmux evidence, so it cannot fall through to a kill.
func TestKillPeerNoProofUnregistersButSkipsKill(t *testing.T) {
	pane := "%99"
	p := &proto.Peer{
		PeerID:      "repow-default-bbbb",
		DisplayName: "proj-claude-code",
		Backend:     proto.AgentClaudeCode,
		Circle:      "default",
		Role:        proto.RoleAgent,
		PaneID:      &pane,
	}
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{p}}
	tmux := &fakeTmux{killOK: true} // no evidence map → ProbePane returns nil
	srv, _ := newSpawnTestHub(t, reg, tmux)

	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: string(p.PeerID)})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kill-peer status = %d, want 200", resp.StatusCode)
	}
	var out KillResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.TmuxKilled != nil {
		t.Fatalf("tmux_killed = %v, want null (no proof)", *out.TmuxKilled)
	}
	if len(tmux.killed) != 0 {
		t.Fatalf("killed panes = %v, want none (no proof)", tmux.killed)
	}
	if len(reg.unregistered) != 1 {
		t.Fatalf("unregistered = %v, want the peer still unregistered", reg.unregistered)
	}
}

// TestKillPeerNotFound returns 404 for an unknown identifier.
func TestKillPeerNotFound(t *testing.T) {
	reg := &fakeSpawnRegistry{}
	srv, _ := newSpawnTestHub(t, reg, &fakeTmux{})
	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: "nope"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestKillPeerAmbiguousIs409 returns 409 with candidates when a display_name
// resolves to more than one peer.
func TestKillPeerAmbiguousIs409(t *testing.T) {
	mk := func(id, circle string) *proto.Peer {
		return &proto.Peer{PeerID: proto.PeerID(id), DisplayName: "dup-claude-code",
			Backend: proto.AgentClaudeCode, Circle: circle, Role: proto.RoleAgent}
	}
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{mk("repow-a-1", "alpha"), mk("repow-b-1", "beta")}}
	srv, _ := newSpawnTestHub(t, reg, &fakeTmux{})
	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: "dup-claude-code"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// GET /spawn/config + POST /spawn happy path.
// ---------------------------------------------------------------------------

// TestSpawnConfigReportsEnabled returns the configured commands and enabled=true
// when both commands and allowed_paths are set.
func TestSpawnConfigReportsEnabled(t *testing.T) {
	reg := &fakeSpawnRegistry{}
	srv, _ := newSpawnTestHub(t, reg, &fakeTmux{})
	resp, err := http.Get(srv.URL + "/spawn/config")
	if err != nil {
		t.Fatalf("GET /spawn/config: %v", err)
	}
	defer resp.Body.Close()
	var out SpawnConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Enabled {
		t.Fatalf("enabled = false, want true")
	}
	if out.CircleBoundary != proto.CircleBoundarySession {
		t.Fatalf("circle_boundary = %q, want session", out.CircleBoundary)
	}
	if out.Commands[proto.AgentClaudeCode] != "claude" {
		t.Fatalf("commands missing claude-code: %v", out.Commands)
	}
}

func TestSpawnSurfacesExcludeRetiredRuntimeCommands(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())
	root := t.TempDir()
	tmux := &fakeTmux{}
	commands := map[proto.AgentType]string{
		proto.AgentClaudeCode:     "claude",
		proto.AgentType("gemini"): "gemini --yolo",
	}
	svc := newHermeticSpawnService(t, tmux, service.NewFileOwnership("test-host", tmux.ProbePane), commands, []string{root})
	h := &Hub{authToken: ""}
	h.WithSpawn(svc, &fakeSpawnRegistry{}, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	muxRouter := http.NewServeMux()
	h.registerSpawnRoutes(muxRouter)
	srv := httptest.NewServer(muxRouter)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/spawn/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var config SpawnConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if _, leaked := config.Commands[proto.AgentType("gemini")]; leaked {
		t.Fatalf("retired runtime leaked through spawn config: %v", config.Commands)
	}

	retired := proto.AgentType("gemini")
	spawn := postSpawnJSON(t, srv, "/spawn", SpawnRequest{Path: root, Backend: &retired, Circle: "default"})
	defer spawn.Body.Close()
	if spawn.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("retired runtime spawn status = %d, want 422", spawn.StatusCode)
	}

	forkResp := postSpawnJSON(t, srv, "/peers/anything/fork-backend", ForkBackendRequest{NewBackend: retired})
	defer forkResp.Body.Close()
	if forkResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("retired runtime fork status = %d, want 422", forkResp.StatusCode)
	}
}

func TestForkBackendSpawnsSiblingWithoutTouchingSource(t *testing.T) {
	tmux := &fakeTmux{spawnResult: service.SpawnResult{
		DisplayName: "project-claude-code", TmuxSession: "default:project", PaneID: "%9",
	}}
	reg := &fakeSpawnRegistry{}
	srv, svc := newSpawnTestHub(t, reg, tmux)
	reg.peers = []*proto.Peer{{
		PeerID:      "peer-source",
		DisplayName: "project-codex",
		Path:        svc.AllowedPaths()[0],
		Machine:     "test-host",
		Backend:     proto.AgentCodex,
		Circle:      "default",
		Role:        proto.RoleOrchestrator,
	}}

	resp := postSpawnJSON(t, srv, "/peers/peer-source/fork-backend", ForkBackendRequest{NewBackend: proto.AgentClaudeCode})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fork status = %d, want 200", resp.StatusCode)
	}
	var out ForkBackendResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.SourcePeerID != "peer-source" || out.SourceDisplayName != "project-codex" || out.NewBackend != proto.AgentClaudeCode {
		t.Fatalf("fork response = %+v", out)
	}
	if service.NormPath(tmux.spawnConfig.Path) != service.NormPath(svc.AllowedPaths()[0]) || tmux.spawnConfig.Circle != "default" || tmux.spawnConfig.Backend != proto.AgentClaudeCode {
		t.Fatalf("spawn config = %+v", tmux.spawnConfig)
	}
	if tmux.spawnConfig.Role != proto.RoleAgent {
		t.Fatalf("fork role = %q, want agent", tmux.spawnConfig.Role)
	}
	if len(tmux.killed) != 0 || len(reg.unregistered) != 0 {
		t.Fatalf("fork touched source: killed=%v unregistered=%v", tmux.killed, reg.unregistered)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("fork response omitted conversation-history warning")
	}
}

func TestForkBackendRejectsSameBackendWithoutSpawning(t *testing.T) {
	tmux := &fakeTmux{}
	reg := &fakeSpawnRegistry{}
	srv, svc := newSpawnTestHub(t, reg, tmux)
	reg.peers = []*proto.Peer{{
		PeerID: "peer-source", DisplayName: "project-codex", Path: svc.AllowedPaths()[0],
		Machine: "test-host", Backend: proto.AgentCodex, Circle: "default", Role: proto.RoleAgent,
	}}

	resp := postSpawnJSON(t, srv, "/peers/peer-source/fork-backend", ForkBackendRequest{NewBackend: proto.AgentCodex})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("same-backend fork status = %d, want 409", resp.StatusCode)
	}
	if tmux.spawnConfig.Backend != "" || len(tmux.killed) != 0 || len(reg.unregistered) != 0 {
		t.Fatalf("rejected fork mutated state: spawn=%+v killed=%v unregistered=%v", tmux.spawnConfig, tmux.killed, reg.unregistered)
	}
}

func TestRemovedSwitchBackendRouteIsNotRegistered(t *testing.T) {
	srv, _ := newSpawnTestHub(t, &fakeSpawnRegistry{}, &fakeTmux{})
	resp := postSpawnJSON(t, srv, "/peers/anything/switch-backend", map[string]string{"new_backend": "claude-code"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removed switch route status = %d, want 404", resp.StatusCode)
	}
}

// TestSpawnClaudeCodeIsPendingHook verifies a self-registering backend (claude-code)
// spawns and leaves registration to its SessionStart hook: registration_state is
// pending_hook, no peer_id is pre-allocated, and the pane is recorded as
// daemon-spawned (so a later kill is provable).
func TestSpawnClaudeCodeIsPendingHook(t *testing.T) {
	reg := &fakeSpawnRegistry{}
	pane := "%7"
	tmux := &fakeTmux{spawnResult: service.SpawnResult{
		DisplayName: "proj-claude-code", TmuxSession: "default:proj", PaneID: pane,
	}}
	srv, svc := newSpawnTestHub(t, reg, tmux)

	// path must be under the allowed root; reuse the allowed root the service was
	// built with by spawning into it directly.
	allowed := svc.AllowedPaths()[0]
	bk := proto.AgentClaudeCode
	resp := postSpawnJSON(t, srv, "/spawn", SpawnRequest{Path: allowed, Backend: &bk, Circle: "default"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spawn status = %d, want 200", resp.StatusCode)
	}
	var out SpawnResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RegistrationState != "pending_hook" {
		t.Fatalf("registration_state = %q, want pending_hook", out.RegistrationState)
	}
	if out.PeerID != nil {
		t.Fatalf("peer_id = %v, want nil (hook self-registers)", *out.PeerID)
	}
	if !svc.Ownership().IsSpawned(pane) {
		t.Fatalf("pane %s not marked spawned after /spawn", pane)
	}
}

// TestSpawnRejectsDoubleSelector 422s when both backend and command are passed.
func TestSpawnRejectsDoubleSelector(t *testing.T) {
	reg := &fakeSpawnRegistry{}
	srv, _ := newSpawnTestHub(t, reg, &fakeTmux{})
	bk := proto.AgentClaudeCode
	resp := postSpawnJSON(t, srv, "/spawn", SpawnRequest{
		Path: "/tmp", Backend: &bk, Command: strp("claude"),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// writeSpawnTestPaneMeta writes a ws-hook meta.json for a pane directly.
func writeSpawnTestPaneMeta(t *testing.T, paneID string, meta map[string]any) {
	t.Helper()
	metaPath := clienthooks.WSHookMetaPath(paneID)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatalf("mkdir pane logs: %v", err)
	}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, raw, 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

// TestDestructivePaneProof_VerifiedByPaneMetadata proves the third proof mode:
// a live pane whose ws-hook meta.json names THIS peer_id authorizes destructive
// control; a mismatching/absent file does not (path match alone is never proof).
func TestDestructivePaneProof_VerifiedByPaneMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())

	pane := "%77"
	tmux := &fakeTmux{killOK: true, evidence: map[string]*service.TmuxPaneEvidence{pane: {TmuxSession: "sess"}}}
	own := service.NewFileOwnership("test-host", tmux.ProbePane)
	svc := newHermeticSpawnService(t, tmux, own,
		map[proto.AgentType]string{proto.AgentClaudeCode: "claude"}, []string{t.TempDir()})
	h := &Hub{}
	h.WithSpawn(svc, nil, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)

	id := proto.PeerID("repow-ops-abc123")
	p := &proto.Peer{PeerID: id, PaneID: &pane}

	writeSpawnTestPaneMeta(t, pane, map[string]any{"peer_id": string(id)})
	if proof := h.destructivePaneProof(p); !proof.ok || proof.mode != "verified_pane_metadata" {
		t.Fatalf("matching pane metadata must prove ownership: ok=%v mode=%q err=%q",
			proof.ok, proof.mode, proof.errCode)
	}

	writeSpawnTestPaneMeta(t, pane, map[string]any{"peer_id": "repow-ops-someoneelse"})
	if proof := h.destructivePaneProof(p); proof.ok {
		t.Fatalf("mismatched pane metadata must NOT prove ownership")
	}
}

func TestRestartPeerDryRunUsesValidatedBackendResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("REPOWIRE_CACHE_DIR", t.TempDir())

	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	projectPath := t.TempDir()
	runtimeID := "runtime-123"
	sessionDir := filepath.Join(home, ".codex", "sessions", "2026", "07", "08")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-"+runtimeID+".jsonl")
	rawPath, _ := json.Marshal(projectPath)
	if err := os.WriteFile(sessionPath, []byte(`{"type":"session_meta","payload":{"cwd":`+string(rawPath)+`}}`+"\n"), 0o644); err != nil {
		t.Fatalf("write codex session: %v", err)
	}

	peerID := "repow-default-codex1"
	_, err = store.UpsertObservation(context.Background(), state.Observation{
		PeerID:           &peerID,
		Backend:          string(proto.AgentCodex),
		ProjectPath:      &projectPath,
		RuntimeSessionID: &runtimeID,
		ResumeCapability: map[string]any{"supported": true, "strategy": "codex_resume"},
		Status:           state.BindingResumable,
	})
	if err != nil {
		t.Fatalf("UpsertObservation: %v", err)
	}

	pane := "%42"
	tmux := &fakeTmux{killOK: true, evidence: map[string]*service.TmuxPaneEvidence{pane: {TmuxSession: "default:proj"}}}
	own := service.NewFileOwnership("test-host", tmux.ProbePane)
	svc := newHermeticSpawnService(t, tmux, own,
		map[proto.AgentType]string{proto.AgentCodex: "codex --dangerously-bypass-approvals-and-sandbox"},
		[]string{projectPath},
	)
	own.MarkSpawned(pane)
	peer := &proto.Peer{
		PeerID:      proto.PeerID(peerID),
		DisplayName: "codex-1",
		Path:        projectPath,
		Machine:     "test-host",
		Backend:     proto.AgentCodex,
		Circle:      "default",
		Status:      proto.StatusOnline,
		Role:        proto.RoleAgent,
		PaneID:      &pane,
	}
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{peer}}
	h := &Hub{store: store}
	h.WithSpawn(svc, reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	mux := http.NewServeMux()
	h.registerSpawnRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := postSpawnJSON(t, srv, "/peers/codex-1/restart", RestartPeerRequest{DryRun: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out RestartPeerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ResumeMode != "resumed" {
		t.Fatalf("resume_mode = %q, want resumed", out.ResumeMode)
	}
	want := "codex --dangerously-bypass-approvals-and-sandbox resume " + runtimeID
	if out.Command == nil || *out.Command != want {
		t.Fatalf("command = %v, want %q", out.Command, want)
	}
	if out.ResumeWarning != nil {
		t.Fatalf("resume_warning = %q, want nil", *out.ResumeWarning)
	}
	if len(tmux.killed) != 0 {
		t.Fatalf("dry-run killed panes: %v", tmux.killed)
	}

	peer.Circle = "window-7"
	tmux.evidence[pane] = &service.TmuxPaneEvidence{PaneID: pane, SessionName: "mesh", WindowID: "@7", WindowPanes: 1, TmuxSession: "mesh:work"}
	tmux.evidence["@7"] = &service.TmuxPaneEvidence{PaneID: pane, SessionName: "mesh", WindowID: "@7", WindowPanes: 1, TmuxSession: "mesh:work"}
	h.WithSpawn(svc, reg, service.NewAskTracker(0), "test-host", proto.CircleBoundaryWindow)
	resp = postSpawnJSON(t, srv, "/peers/codex-1/restart", RestartPeerRequest{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("last-pane restart status = %d, want 409", resp.StatusCode)
	}
	if len(tmux.killed) != 0 {
		t.Fatalf("last-pane restart killed before validating its target: %v", tmux.killed)
	}
	peer.Circle = "default"
	h.WithSpawn(svc, reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)

	if err := os.Remove(sessionPath); err != nil {
		t.Fatalf("remove codex session: %v", err)
	}
	resp = postSpawnJSON(t, srv, "/peers/codex-1/restart", RestartPeerRequest{DryRun: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale session status = %d, want 409", resp.StatusCode)
	}
	if len(tmux.killed) != 0 {
		t.Fatalf("stale dry-run killed panes: %v", tmux.killed)
	}
}
