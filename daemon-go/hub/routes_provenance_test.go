package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

func getPeersJSON(t *testing.T, mux *http.ServeMux, path string) PeersResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	var resp PeersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Registration accepts an explicit provenance; HTTP /peers stays a complete
// inventory with optional source/addressable filters; parent_peer_id resolves
// through the parent's runtime_session_id and is null when unregistered; and
// POST /peers/{name}/provenance demotes a live peer.
func TestPeerProvenanceRegistrationListAndDemotion(t *testing.T) {
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.Routes(mux)
	path := "/work/app"

	parent := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
		Metadata:   map[string]any{"runtime_session_id": "thread-parent", "transport": "codex-app-server"},
		Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true},
	})
	if parent.Code != http.StatusOK {
		t.Fatalf("register parent: %d %s", parent.Code, parent.Body.String())
	}
	child := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
		Metadata:   map[string]any{"runtime_session_id": "thread-child", "transport": "codex-app-server", "agent_nickname": "Pasteur"},
		Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "thread-parent", Addressable: false, AddressableReason: "subagent_direct_input_denied"},
	})
	if child.Code != http.StatusOK {
		t.Fatalf("register child: %d %s", child.Code, child.Body.String())
	}
	orphan := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{
		Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
		Metadata:   map[string]any{"runtime_session_id": "thread-orphan", "transport": "codex-app-server"},
		Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "thread-gone", Addressable: true},
	})
	if orphan.Code != http.StatusOK {
		t.Fatalf("register orphan: %d %s", orphan.Code, orphan.Body.String())
	}
	if rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "bad", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
		Provenance: &proto.Provenance{Source: "desktop", Addressable: true}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid source accepted: %d", rec.Code)
	}
	// A plain hook registration lands as source=hook and stays addressable.
	hook := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "app-claude-code", Path: &path, Backend: proto.AgentClaudeCode, Circle: strptr("c"),
		Metadata: map[string]any{"hook_version": 3}})
	if hook.Code != http.StatusOK {
		t.Fatalf("register hook peer: %d", hook.Code)
	}

	all := getPeersJSON(t, mux, "/peers")
	if len(all.Peers) != 4 {
		t.Fatalf("inventory hides peers: %d", len(all.Peers))
	}
	byRuntime := map[string]PeerInfo{}
	for _, p := range all.Peers {
		rid, _ := p.Metadata["runtime_session_id"].(string)
		byRuntime[rid] = p
		if p.Backend == proto.AgentClaudeCode && (p.Source != proto.SourceHook || !p.Addressable) {
			t.Fatalf("hook peer provenance = %+v", p.Provenance)
		}
	}
	c := byRuntime["thread-child"]
	if c.Addressable || c.AddressableReason != "subagent_direct_input_denied" || c.ParentPeerID == nil || *c.ParentPeerID != byRuntime["thread-parent"].PeerID {
		t.Fatalf("child info = %+v parent=%v", c.Provenance, c.ParentPeerID)
	}
	if o := byRuntime["thread-orphan"]; o.ParentPeerID != nil {
		t.Fatalf("orphan fabricated parent %v", *o.ParentPeerID)
	}
	if got := getPeersJSON(t, mux, "/peers?addressable=false"); len(got.Peers) != 1 || got.Peers[0].PeerID != c.PeerID {
		t.Fatalf("addressable=false filter = %d peers", len(got.Peers))
	}
	if got := getPeersJSON(t, mux, "/peers?source=hook"); len(got.Peers) != 1 || got.Peers[0].Backend != proto.AgentClaudeCode {
		t.Fatalf("source=hook filter = %+v", got.Peers)
	}

	// Demote the orphan, then confirm it disappears from the addressable view.
	rec := postLifecycleJSON(t, mux, "/peers/"+string(byRuntime["thread-orphan"].PeerID)+"/provenance",
		proto.Provenance{Source: proto.SourceCodexAppServer, ParentRuntimeID: "thread-gone", Addressable: false, AddressableReason: "subagent_direct_input_denied"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST provenance: %d %s", rec.Code, rec.Body.String())
	}
	if got := getPeersJSON(t, mux, "/peers?addressable=true"); len(got.Peers) != 2 {
		t.Fatalf("after demotion addressable=true = %d peers, want 2", len(got.Peers))
	}
	if got := getPeersJSON(t, mux, "/peers?listed=true"); len(got.Peers) != 2 {
		t.Fatalf("listed=true = %d peers, want parent and hook peer", len(got.Peers))
	}
	if rec := postLifecycleJSON(t, mux, "/peers/nobody/provenance", proto.Provenance{Source: proto.SourceHook, Addressable: true}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown peer provenance = %d", rec.Code)
	}
}

// MCP list_peers hides non-addressable peers unless asked, appends the
// provenance columns after the historical thirteen, and filters by source.
func TestMCPListPeersHidesNonAddressableByDefault(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	path := "/work/app"
	for _, reg := range []RegisterPeerRequest{
		{Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
			Metadata:   map[string]any{"runtime_session_id": "thread-parent"},
			Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true}},
		{Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
			Metadata:   map[string]any{"runtime_session_id": "thread-child", "agent_nickname": "Nash"},
			Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorAgent, ParentRuntimeID: "thread-parent", Addressable: false, AddressableReason: "subagent_direct_input_denied"}},
		{Name: "app-claude-code", Path: &path, Backend: proto.AgentClaudeCode, Circle: strptr("c"), Metadata: map[string]any{"hook_version": 3}},
		// Runtime machinery: addressable, but nobody opened it.
		{Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
			Metadata:   map[string]any{"runtime_session_id": "thread-system"},
			Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorSystem, Ephemeral: true, Addressable: true}},
		// An accepting sub-agent whose parent is not on the mesh.
		{Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
			Metadata:   map[string]any{"runtime_session_id": "thread-orphan", "agent_nickname": "Pascal"},
			Provenance: &proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorAgent, ParentRuntimeID: "thread-gone", Addressable: true}},
	} {
		if rec := postLifecycleJSON(t, mux, "/peers", reg); rec.Code != http.StatusOK {
			t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
		}
	}
	list := func(args map[string]any) []string {
		_, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{
			"jsonrpc": "2.0", "id": 9, "method": "tools/call",
			"params": map[string]any{"name": "list_peers", "arguments": args},
		}, nil)
		var result mcpToolCallResult
		decodeResult(t, envelope, &result)
		if result.IsError || len(result.Content) != 1 {
			t.Fatalf("list_peers result = %+v", result)
		}
		return strings.Split(result.Content[0].Text, "\n")
	}
	rows := list(map[string]any{"circle": "*", "show_offline": true})
	header := strings.Split(rows[0], "\t")
	if len(header) != 18 || header[12] != "model" || header[13] != "source" || header[14] != "initiator" || header[15] != "addressable" || header[16] != "parent_peer_id" || header[17] != "nickname" {
		t.Fatalf("header = %v", header)
	}
	if len(rows) != 3 {
		t.Fatalf("default view rows = %d, want the user thread and the hook peer only:\n%s", len(rows)-1, strings.Join(rows, "\n"))
	}
	for _, row := range rows[1:] {
		if strings.Contains(row, "Nash") || strings.Contains(row, "Pascal") || strings.Contains(row, "thread-system") || strings.Contains(row, "\tsystem\t") {
			t.Fatalf("hidden peer shown by default: %q", row)
		}
	}
	rows = list(map[string]any{"circle": "*", "show_offline": true, "include_hidden": true})
	if len(rows) != 6 {
		t.Fatalf("include_hidden rows = %d, want 5", len(rows)-1)
	}
	var child []string
	for _, row := range rows[1:] {
		if cols := strings.Split(row, "\t"); cols[17] == "Nash" {
			child = cols
		}
	}
	if child == nil || child[13] != "codex-app-server" || child[14] != "agent" || child[15] != "false" || !strings.HasPrefix(child[16], "repow-") {
		t.Fatalf("child row = %v", child)
	}
	if rows = list(map[string]any{"circle": "*", "show_offline": true, "source": "hook"}); len(rows) != 2 || !strings.Contains(rows[1], "claude-code") {
		t.Fatalf("source=hook rows = %v", rows)
	}
}
