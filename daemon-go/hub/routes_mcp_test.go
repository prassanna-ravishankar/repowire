package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/routing"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// newMCPTestHub builds a Hub over a real registry (like newTestHub) plus a real
// service.PeerDelivery wired over it, then attaches WithMCP. cfg lets each test
// dial in the auth policy under test.
func newMCPTestHub(t *testing.T, cfg config.MCPHTTPConfig) (*Hub, *peer.Registry) {
	t.Helper()
	h := newTestHub(t)
	delivery := service.NewPeerDelivery(h.regForTest(), h.Router(), h.Transport(), service.NewAskTracker(0), nil)
	h.WithMCP(cfg, delivery)
	return h, h.regForTest()
}

// mcpRPC POSTs a single JSON-RPC request to /mcp with the headers the SDK's
// streamable-HTTP transport requires (Content-Type: application/json, Accept
// naming both JSON and event-stream), and decodes the JSON-RPC envelope into
// out (out's "result" field, typically). Stateless+JSONResponse mode (see
// WithMCP) means each call is self-contained — no Mcp-Session-Id handshake
// needed.
func mcpRPC(t *testing.T, url string, body map[string]any, headers map[string]string) (status int, envelope map[string]json.RawMessage) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// Only a 200 application/json body is a JSON-RPC envelope worth decoding.
	// The SDK returns non-JSON for the error shapes callers assert on by status:
	// an unknown method → 400 text ("JSON RPC not handled: ..."), an unregistered
	// /mcp → 404 HTML (dashboard catch-all), a rejected auth → 401. Decoding those
	// would spuriously fail; return status-only instead.
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.Contains(ct, "application/json") || len(bytes.TrimSpace(respBody)) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (body=%q)", err, respBody)
	}
	return resp.StatusCode, envelope
}

func decodeResult(t *testing.T, envelope map[string]json.RawMessage, out any) {
	t.Helper()
	raw, ok := envelope["result"]
	if !ok {
		t.Fatalf("no result in envelope: %v", envelope)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
}

// TestMCPInitialize asserts the initialize result carries protocolVersion +
// serverInfo, per the MCP handshake.
func TestMCPInitialize(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	decodeResult(t, envelope, &result)
	if result.ProtocolVersion == "" {
		t.Errorf("protocolVersion missing")
	}
	if result.ServerInfo.Name != "repowire" {
		t.Errorf("serverInfo.name = %q, want repowire", result.ServerInfo.Name)
	}
}

func TestMCPLocalhostBindingRejectsRemoteAddress(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, Bind: "localhost-only", RequireAuth: true})
	h.authToken = "secret"
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.RemoteAddr = "203.0.113.20:1234"
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	h.handleMCP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("remote MCP status = %d, want 401", recorder.Code)
	}
}

// TestMCPToolsList asserts the full stdio-parity tool surface is advertised.
func TestMCPToolsList(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var result struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	decodeResult(t, envelope, &result)

	want := map[string]bool{}
	for _, name := range []string{
		"ack", "answer", "ask", "ask_many", "ask_many_result", "broadcast", "decline",
		"job_cancel", "job_create", "job_list",
		"job_result", "job_show", "job_status", "job_update", "kill_peer",
		"list_peers", "mark_reviewed", "notify_peer", "orchestrator_status",
		"restart_peer", "review_queue", "revoke_share", "schedule_create", "schedule_cron",
		"schedule_delete", "schedule_list", "schedule_self", "set_description", "set_name",
		"share_session", "spawn_peer", "wait_on_ack", "whoami",
	} {
		want[name] = false
	}
	if len(result.Tools) != len(want) {
		t.Fatalf("tools = %v, want %d entries", result.Tools, len(want))
	}
	for _, tl := range result.Tools {
		if _, ok := want[tl.Name]; !ok {
			t.Errorf("unexpected tool %q", tl.Name)
		}
		want[tl.Name] = true
		if tl.Name == "ask_many_result" {
			if len(tl.InputSchema.Properties) != 1 || tl.InputSchema.Properties["parent_id"] == nil {
				t.Errorf("ask_many_result schema = %#v; want only parent_id", tl.InputSchema.Properties)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestMCPSpawnCirclePolicy(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{})
	register := func(circle string, role proto.PeerRole, pane *string) proto.PeerID {
		t.Helper()
		id, _, err := reg.AllocateAndRegister(context.Background(), peer.AllocateParams{
			Circle: circle, Backend: proto.AgentClaudeCode, Role: role, PaneID: pane,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	pane := "%7"
	agent := register("alpha", proto.RoleAgent, &pane)
	if circle, sourcePane, err := h.mcpSpawnPlacement(string(agent), ""); err != nil || circle != "alpha" || sourcePane != pane {
		t.Fatalf("agent placement = %q, %q, %v; want alpha, %%7, nil", circle, sourcePane, err)
	}
	if _, _, err := h.mcpSpawnPlacement(string(agent), "beta"); err == nil {
		t.Fatal("agent cross-circle spawn was allowed")
	}
	for _, circle := range []string{"0", "1"} {
		numericPane := "%" + circle
		numericAgent := register(circle, proto.RoleAgent, &numericPane)
		for _, requested := range []string{"", circle} {
			if got, sourcePane, err := h.mcpSpawnPlacement(string(numericAgent), requested); err != nil || got != circle || sourcePane != numericPane {
				t.Fatalf("numeric circle placement = %q, %q, %v; want %q, %q, nil", got, sourcePane, err, circle, numericPane)
			}
		}
		if _, _, err := h.mcpSpawnPlacement(string(numericAgent), "other"); err == nil {
			t.Fatal("numeric-circle agent cross-circle spawn was allowed")
		}
	}

	orchestrator := register("alpha", proto.RoleOrchestrator, &pane)
	if circle, sourcePane, err := h.mcpSpawnPlacement(string(orchestrator), "beta"); err != nil || circle != "beta" || sourcePane != "" {
		t.Fatalf("orchestrator cross-circle spawn = %q, %q, %v; want beta, empty pane, nil", circle, sourcePane, err)
	}
	if _, _, err := h.mcpSpawnPlacement(mcpDefaultIdentity, ""); err == nil {
		t.Fatal("anonymous MCP invented a spawn circle")
	}
}

func TestMCPKillCirclePolicy(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{})
	register := func(circle string, role proto.PeerRole) proto.PeerID {
		t.Helper()
		id, _, err := reg.AllocateAndRegister(context.Background(), peer.AllocateParams{
			Circle: circle, Backend: proto.AgentClaudeCode, Role: role,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	agent := register("alpha", proto.RoleAgent)
	if circle, err := h.mcpKillCircle(string(agent), ""); err != nil || circle == nil || *circle != "alpha" {
		t.Fatalf("agent kill circle = %v, %v; want alpha, nil", circle, err)
	}
	if _, err := h.mcpKillCircle(string(agent), "beta"); err == nil {
		t.Fatal("agent cross-circle kill was allowed")
	}

	orchestrator := register("alpha", proto.RoleOrchestrator)
	if circle, err := h.mcpKillCircle(string(orchestrator), "beta"); err != nil || circle == nil || *circle != "beta" {
		t.Fatalf("orchestrator kill circle = %v, %v; want beta, nil", circle, err)
	}
	if circle, err := h.mcpKillCircle(mcpDefaultIdentity, ""); err != nil || circle != nil {
		t.Fatalf("anonymous admin kill circle = %v, %v; want nil, nil", circle, err)
	}
}

// mcpToolCallResult is the shared tools/call result decode shape.
type mcpToolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// TestMCPToolsCallListPeersEmpty exercises tools/call -> list_peers against an
// empty registry: a content array with isError:false.
func TestMCPToolsCallListPeersEmpty(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "list_peers", "arguments": map[string]any{}},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var result mcpToolCallResult
	decodeResult(t, envelope, &result)
	if result.IsError {
		t.Errorf("isError = true, want false: %+v", result.Content)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text block", result.Content)
	}
}

// TestMCPToolsCallWhoamiIdentity asserts whoami reports the default mcp-http
// identity with no header, and the real registered identity once a matching
// peer is registered and the header is set.
func TestMCPToolsCallWhoamiIdentity(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.store = store
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	callWhoami := func(headers map[string]string) mcpToolCallResult {
		_, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{
			"jsonrpc": "2.0", "id": 4, "method": "tools/call",
			"params": map[string]any{"name": "whoami", "arguments": map[string]any{}},
		}, headers)
		var result mcpToolCallResult
		decodeResult(t, envelope, &result)
		if len(result.Content) != 1 {
			t.Fatalf("content = %+v, want 1 block", result.Content)
		}
		return result
	}

	if result := callWhoami(nil); !strings.Contains(result.Content[0].Text, "mcp-http") {
		t.Errorf("whoami with no identity header = %q, want it to mention mcp-http", result.Content[0].Text)
	}

	path := "/work/alpha"
	id, name, err := reg.AllocateAndRegister(context.Background(), peer.AllocateParams{
		Circle:   "default",
		Backend:  proto.AgentClaudeCode,
		Role:     proto.RoleAgent,
		Path:     &path,
		Metadata: map[string]any{"in_process": true},
	})
	if err != nil {
		t.Fatalf("AllocateAndRegister: %v", err)
	}
	// A bare identity header is untrusted and must be treated as direct HTTP.
	if result := callWhoami(map[string]string{"X-Repowire-Peer": string(name)}); !strings.Contains(result.Content[0].Text, "mcp-http") {
		t.Fatalf("unproved identity claim was accepted: %q", result.Content[0].Text)
	}
	backend, peerID, displayName := "claude-code", string(id), string(name)
	cert, err := store.MintBirthCertificate(context.Background(), peerID, displayName, backend, &path, nil, nil, nil, nil, nil, time.Hour, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := reg.GetPeer(id)
	result := callWhoami(map[string]string{"X-Repowire-Peer": string(name), "X-Repowire-Identity-Proof": cert.Nonce})
	if result.IsError {
		t.Errorf("whoami with valid identity: isError = true, text=%q", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, string(id)) {
		t.Errorf("whoami text = %q, want it to contain peer_id %q", result.Content[0].Text, id)
	}
	after, _ := reg.GetPeer(id)
	if before.LastSeen == nil || after.LastSeen == nil || !after.LastSeen.After(*before.LastSeen) {
		t.Errorf("validated MCP request did not refresh last_seen: before=%v after=%v", before.LastSeen, after.LastSeen)
	}
}

// TestMCPUnknownMethod asserts an unrecognized method is rejected. The SDK's
// streamable-HTTP transport answers an unsupported top-level method with HTTP
// 400 + a plain-text body ("JSON RPC not handled: ..."), not a JSON-RPC error
// envelope — so assert on the status.
func TestMCPUnknownMethod(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, _ := mcpRPC(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "bogus/method",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown method rejected)", status)
	}
}

// TestMCPRequireAuthMissingBearer asserts a RequireAuth=true daemon 401s a
// request with no bearer token.
func TestMCPRequireAuthMissingBearer(t *testing.T) {
	h := newTestHub(t)
	h.authToken = "secret-token"
	delivery := service.NewPeerDelivery(h.regForTest(), h.Router(), h.Transport(), service.NewAskTracker(0), nil)
	h.WithMCP(config.MCPHTTPConfig{Enabled: true, RequireAuth: true}, delivery)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, _ := mcpRPC(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "ping",
	}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestMCPDisabledNotRegistered asserts Enabled:false leaves /mcp unregistered
// (404, not routed into the SDK handler at all).
func TestMCPDisabledNotRegistered(t *testing.T) {
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: false})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, _ := mcpRPC(t, srv.URL+"/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "ping",
	}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route not registered)", status)
	}
}

// peer_name is optional on the ask tool: absent hands the ask to the routing
// strategy (the default static strategy refuses it as no_target); present but
// empty is still a missing name.
func TestMCPAskPeerNameIsOptional(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	asks := service.NewAskTracker(0)
	h.WithAskLifecycle(asks, service.NewPeerDelivery(reg, h.Router(), h.Transport(), asks, nil), reg)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil)
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	decodeResult(t, envelope, &list)
	for _, tl := range list.Tools {
		if tl.Name != "ask" {
			continue
		}
		if slices.Contains(tl.InputSchema.Required, "peer_name") || !slices.Contains(tl.InputSchema.Required, "query") {
			t.Fatalf("ask required = %v; want query only", tl.InputSchema.Required)
		}
		if typ := tl.InputSchema.Properties["peer_name"]["type"]; typ != "string" {
			t.Fatalf("peer_name schema type = %v; want string, not nullable", typ)
		}
	}

	call := func(args map[string]any) string {
		_, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "ask", "arguments": args},
		}, nil)
		var result mcpToolCallResult
		decodeResult(t, envelope, &result)
		if !result.IsError || len(result.Content) != 1 {
			t.Fatalf("ask %v: want an error result, got %+v", args, result)
		}
		return result.Content[0].Text
	}
	if text := call(map[string]any{"query": "who owns the lexer?"}); !strings.Contains(text, "no_target") {
		t.Fatalf("absent peer_name under static: %q", text)
	}
	if text := call(map[string]any{"peer_name": "", "query": "x"}); !strings.Contains(text, "peer_name") {
		t.Fatalf("empty peer_name: %q", text)
	}
	// null is not omission: it must never reach a strategy.
	if text := call(map[string]any{"peer_name": nil, "query": "x"}); strings.Contains(text, "no_target") || !strings.Contains(text, "peer_name") {
		t.Fatalf("null peer_name: %q", text)
	}
	// A key that only looks like peer_name is refused, never read as omitted.
	if text := call(map[string]any{"peerName": "beta", "query": "x"}); strings.Contains(text, "no_target") {
		t.Fatalf("peerName: %q", text)
	}
	if asks.OpenCount() != 0 {
		t.Fatal("a refused ask was registered")
	}

	// Under a strategy that would pick a recipient, null and look-alike keys
	// still never reach it.
	arm := &stubArm{name: "stub", scores: []routing.Score{{PeerID: "repow-c", Score: 1}}}
	router, err := routing.NewRouter(*liveCfg("stub"), map[string]routing.Arm{"stub": arm})
	if err != nil {
		t.Fatal(err)
	}
	h.WithRouting(router)
	call(map[string]any{"peer_name": nil, "query": "x"})
	call(map[string]any{"peerName": "beta", "query": "x"})
	if arm.calls != 0 || asks.OpenCount() != 0 {
		t.Fatalf("null or look-alike peer_name reached the strategy: calls %d open %d", arm.calls, asks.OpenCount())
	}
}

// #494: a presented identity proof that fails is never downgraded to
// mcp-http. Session setup still works, every tool call fails with the reason
// and is journaled, and callers presenting no proof stay anonymous by design.
func TestMCPRejectedIdentityProofFailsLoud(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, AllowUnauthenticatedLocalhost: true})
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.store = store
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	path, thread := "/work/paper", "thread-1"
	id, name, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{
		Circle: "1", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &path,
		Metadata: map[string]any{"runtime_session_id": thread},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherPath := "/work/other"
	other, otherName, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{
		Circle: "1", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &otherPath,
		Metadata: map[string]any{"runtime_session_id": "thread-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mint := func(peerID proto.PeerID, display proto.DisplayName, issued time.Time) string {
		cert, err := store.MintBirthCertificate(ctx, string(peerID), string(display), "codex", &path, &thread, nil, nil, nil, nil, 0, issued)
		if err != nil {
			t.Fatal(err)
		}
		return cert.Nonce
	}
	expired := mint(id, name, time.Now().Add(-25*time.Hour))
	otherProof := mint(other, otherName, time.Time{})

	rpc := func(headers map[string]string, method string, params map[string]any) (int, map[string]json.RawMessage) {
		return mcpRPC(t, srv.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, headers)
	}
	call := func(headers map[string]string, tool string, args map[string]any) mcpToolCallResult {
		t.Helper()
		_, env := rpc(headers, "tools/call", map[string]any{"name": tool, "arguments": args})
		var r mcpToolCallResult
		decodeResult(t, env, &r)
		if len(r.Content) != 1 {
			t.Fatalf("%s content = %+v", tool, r.Content)
		}
		return r
	}
	claim := func(peer, proof string) map[string]string {
		return map[string]string{"X-Repowire-Peer": peer, "X-Repowire-Identity-Proof": proof}
	}

	expiredClaim := claim(string(id), expired)
	if status, env := rpc(expiredClaim, "tools/list", map[string]any{}); status != http.StatusOK || env["error"] != nil {
		t.Fatalf("tools/list with an expired proof must still work: %d %s", status, env["error"])
	}
	for _, tc := range []struct {
		headers map[string]string
		reason  string
	}{
		{expiredClaim, "expired"},
		{claim(string(id), "invented"), "unknown"},
		{claim(string(id), otherProof), "peer_mismatch"},
		{claim("ghost-codex", otherProof), "peer_not_registered"},
	} {
		got := call(tc.headers, "set_description", map[string]any{"description": "should not land"})
		if !got.IsError || !strings.Contains(got.Content[0].Text, "rejected ("+tc.reason+")") || strings.Contains(got.Content[0].Text, "mcp-http") {
			t.Fatalf("reason %s: result = %+v", tc.reason, got)
		}
	}
	if p, _ := reg.GetPeer(id); p.Description != "" {
		t.Fatalf("rejected call ran: description = %q", p.Description)
	}
	var rejected []map[string]any
	for _, ev := range reg.EventsSince("") {
		if ev["type"] == "mcp_identity_proof_rejected" {
			rejected = append(rejected, ev)
		}
	}
	if len(rejected) != 4 {
		t.Fatalf("want 4 mcp_identity_proof_rejected events, got %d", len(rejected))
	}
	if raw, _ := json.Marshal(rejected); strings.Contains(string(raw), expired) || !strings.Contains(string(raw), `"expired"`) || !strings.Contains(string(raw), "set_description") {
		t.Fatalf("rejection events must carry reason and tool, never the nonce: %s", raw)
	}

	// No proof presented: anonymous mcp-http, deliberately. A client cannot
	// forge the rejection marker either.
	for _, headers := range []map[string]string{
		nil,
		{"X-Repowire-Peer": string(id)},
		{"X-Repowire-Identity-Rejected": "expired", "X-Repowire-Identity-Rejected-Claim": string(id)},
	} {
		if got := call(headers, "whoami", map[string]any{}); got.IsError || !strings.Contains(got.Content[0].Text, "mcp-http") {
			t.Fatalf("headers %v: whoami = %+v, want anonymous mcp-http", headers, got)
		}
	}
}

// #494: a connected thread exchanges an expired certificate for a fresh one
// that works as MCP proof; renewal refuses a disconnected peer, another
// session, and a certificate that does not match.
func TestRenewRuntimeIdentityWhileConnected(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, AllowUnauthenticatedLocalhost: true})
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.store = store
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	path, thread := "/work/paper", "thread-1"
	id, name, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{
		Circle: "1", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &path,
		Metadata: map[string]any{"runtime_session_id": thread},
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.MintBirthCertificate(ctx, string(id), string(name), "codex", &path, &thread, nil, nil, nil, nil, 0, time.Now().Add(-25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	renew := func(cert *state.RuntimeIdentityCertificate) (int, map[string]any) {
		t.Helper()
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(map[string]any{"birth_certificate": certEnvelope(cert), "backend": "codex", "path": path})
		resp, err := http.Post(srv.URL+"/peers/identity/renew", "application/json", &body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if status, _ := renew(old); status != http.StatusConflict {
		t.Fatalf("renewal for a disconnected peer: %d, want 409", status)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(dialCtx, conn, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: name, Circle: "1",
		Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &path, PeerID: &id}); err != nil {
		t.Fatal(err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(dialCtx, conn, &connected); err != nil || connected.SessionID != id {
		t.Fatalf("connect: %+v %v", connected, err)
	}
	for deadline := time.Now().Add(2 * time.Second); !h.transport.IsConnected(id) && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}

	status, out := renew(old)
	fresh, _ := out["birth_certificate"].(map[string]any)
	nonce, _ := fresh["nonce"].(string)
	if status != http.StatusOK || nonce == "" || nonce == old.Nonce {
		t.Fatalf("renewal while connected: %d %v", status, out)
	}
	if problem, err := store.MCPIdentityProofProblem(ctx, nonce, string(id)); err != nil || problem != "" {
		t.Fatalf("renewed proof does not validate: %q %v", problem, err)
	}
	_, env := mcpRPC(t, srv.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "set_description", "arguments": map[string]any{"description": "renewed"}}},
		map[string]string{"X-Repowire-Peer": string(id), "X-Repowire-Identity-Proof": nonce})
	var result mcpToolCallResult
	decodeResult(t, env, &result)
	if p, _ := reg.GetPeer(id); result.IsError || p.Description != "renewed" {
		t.Fatalf("set_description with the renewed proof: %+v (description %q)", result, p.Description)
	}

	otherThread := "thread-2"
	wrongSession, err := store.MintBirthCertificate(ctx, string(id), string(name), "codex", &path, &otherThread, nil, nil, nil, nil, 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := renew(wrongSession); status != http.StatusConflict {
		t.Fatalf("renewal for another session: %d, want 409", status)
	}
	forged := *old
	forged.Nonce = "invented"
	if status, _ := renew(&forged); status != http.StatusConflict {
		t.Fatalf("renewal with an unknown certificate: %d, want 409", status)
	}
}
