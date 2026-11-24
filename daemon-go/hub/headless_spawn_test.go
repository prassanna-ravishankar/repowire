package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

func headlessSpawn(t *testing.T, enabled bool, backend proto.AgentType) (*http.Response, *fakeSpawnRegistry, *fakeTmux) {
	t.Helper()
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	reg := &fakeSpawnRegistry{allocID: "repow-headless-1", allocName: "proj-claude-code"}
	tmux := &fakeTmux{killOK: true}
	root := t.TempDir()
	svc := newHermeticSpawnService(t, tmux, service.NewFileOwnership("test-host", tmux.Probe),
		map[proto.AgentType]string{proto.AgentClaudeCode: "claude", proto.AgentCodex: "codex"}, []string{root})
	h := &Hub{}
	h.WithSpawn(svc, reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	if enabled {
		h.WithHeadlessClaudeCode(HeadlessLaunch{Command: "npx", Args: []string{"-y", "@zed-industries/claude-agent-acp"}})
	}
	mux := http.NewServeMux()
	h.registerSpawnRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return postSpawnJSON(t, srv, "/spawn", SpawnRequest{Path: root, Backend: &backend, Circle: "default", Headless: true}), reg, tmux
}

// A headless claude-code spawn registers a pane-less peer whose metadata.acp
// launches the ACP adapter with the session hooks off; no pane is created.
func TestHeadlessSpawnRegistersAnACPPeerWithNoPane(t *testing.T) {
	resp, reg, tmux := headlessSpawn(t, true, proto.AgentClaudeCode)
	defer resp.Body.Close()
	var out SpawnResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.RegistrationState != "registered" || out.PeerID == nil || *out.PeerID != "repow-headless-1" || out.TmuxSession != "" {
		t.Fatalf("status %d %+v", resp.StatusCode, out)
	}
	if len(reg.allocated) != 1 {
		t.Fatalf("allocated %d, want 1", len(reg.allocated))
	}
	p := reg.allocated[0]
	acp, _ := p.Metadata["acp"].(map[string]any)
	env, _ := acp["env"].(map[string]any)
	if p.PaneID != nil || p.Backend != proto.AgentClaudeCode || acp["command"] != "npx" || env[clienthooks.HooksDisabledEnv] != "off" || p.Metadata["headless"] != true {
		t.Fatalf("registered %+v with metadata %v", p, p.Metadata)
	}
	if len(tmux.killed) != 0 || tmux.spawnConfig.Path != "" {
		t.Fatalf("tmux touched: launched %+v killed %v", tmux.spawnConfig, tmux.killed)
	}
}

func TestHeadlessSpawnRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		backend proto.AgentType
		code    string
	}{
		{"off", false, proto.AgentClaudeCode, "headless_disabled"},
		{"not claude-code", true, proto.AgentCodex, "headless_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, reg, _ := headlessSpawn(t, tc.enabled, tc.backend)
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			detail, _ := body["detail"].(map[string]any)
			if resp.StatusCode != http.StatusUnprocessableEntity || detail["error"] != tc.code || len(reg.allocated) != 0 {
				t.Fatalf("status %d body %v allocated %d", resp.StatusCode, body, len(reg.allocated))
			}
		})
	}
}

// The ACP plug owns peers with a metadata.acp command and stops them by
// dropping their child; kill_peer then closes the peer.
func TestACPPlugStopsAnACPPeerThroughKill(t *testing.T) {
	p := &proto.Peer{PeerID: "repow-headless-1", DisplayName: "proj-claude-code", Backend: proto.AgentClaudeCode, Circle: "default", Role: proto.RoleAgent,
		Metadata: map[string]any{"headless": true, "acp": map[string]any{"command": "npx"}}}
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{p}}
	srv, svc := newSpawnTestHub(t, reg, &fakeTmux{killOK: true})
	var dropped []proto.PeerID
	svc.WithPeerContainers(service.NewACPContainer(func(id proto.PeerID) { dropped = append(dropped, id) }))
	if svc.PeerContainerFor(&proto.Peer{PeerID: "x"}) != nil {
		t.Fatal("a peer without metadata.acp was claimed")
	}
	if svc.PeerContainerFor(&proto.Peer{PeerID: "y", Metadata: map[string]any{"acp": map[string]any{"command": "npx"}}}) != nil {
		t.Fatal("an ACP peer the headless plug did not create was claimed")
	}
	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: string(p.PeerID)})
	defer resp.Body.Close()
	var out KillResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Container != "acp" || len(dropped) != 1 || len(reg.closed) != 1 {
		t.Fatalf("status %d %+v dropped %v closed %v", resp.StatusCode, out, dropped, reg.closed)
	}
}
