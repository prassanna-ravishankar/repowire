package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// The wire tools answer a proven orchestrator in every circle and refuse an
// ordinary agent and anonymous HTTP MCP; the operator routes answer JSON.
// Neither carries message text.
func TestWireViewAuthorizationAndEnvelope(t *testing.T) {
	ctx := context.Background()
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, AllowUnauthenticatedLocalhost: true})
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.store = store
	h.WithWire(service.NewWire(service.NewAskTracker(0), store, reg, store, config.Defaults().Wire.Stuck))
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	register := func(circle string, role proto.PeerRole, thread string) (proto.PeerID, map[string]string) {
		path := "/work/" + thread
		id, name, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{
			Circle: circle, Backend: proto.AgentCodex, Role: role, Path: &path,
			Metadata: map[string]any{"runtime_session_id": thread},
		})
		if err != nil {
			t.Fatal(err)
		}
		cert, err := store.MintBirthCertificate(ctx, string(id), string(name), "codex", &path, &thread, nil, nil, nil, nil, 0, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return id, map[string]string{"X-Repowire-Peer": string(id), "X-Repowire-Identity-Proof": cert.Nonce}
	}
	_, orch := register("ops", proto.RoleOrchestrator, "orch")
	asker, agent := register("dev", proto.RoleAgent, "asker")
	holder, _ := register("other", proto.RoleAgent, "holder")
	if err := store.RecordTrace(ctx, "ask-wire1", "ask", "created", "", "ask-wire1", string(holder), string(asker), map[string]any{"text": "SECRET-ASK"}); err != nil {
		t.Fatal(err)
	}

	call := func(headers map[string]string, tool string, args map[string]any) mcpToolCallResult {
		t.Helper()
		_, env := mcpRPC(t, srv.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args}}, headers)
		var r mcpToolCallResult
		decodeResult(t, env, &r)
		if len(r.Content) != 1 {
			t.Fatalf("%s content = %+v", tool, r.Content)
		}
		return r
	}

	// An orchestrator in circle ops sees a thread between dev and other.
	res := call(orch, "wire_query", map[string]any{})
	if res.IsError || !strings.Contains(res.Content[0].Text, "ask-wire1") || strings.Contains(res.Content[0].Text, "SECRET") {
		t.Fatalf("orchestrator wire_query: %+v", res)
	}
	res = call(orch, "wire_show", map[string]any{"thread_id": "ask-wire1"})
	if res.IsError || !strings.Contains(res.Content[0].Text, "created") || strings.Contains(res.Content[0].Text, "SECRET") {
		t.Fatalf("orchestrator wire_show: %+v", res)
	}
	for name, headers := range map[string]map[string]string{"agent": agent, "anonymous": nil} {
		for _, tool := range []string{"wire_query", "wire_show"} {
			if res := call(headers, tool, map[string]any{"thread_id": "ask-wire1"}); !res.IsError {
				t.Fatalf("%s %s was answered: %+v", name, tool, res)
			}
		}
	}

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	status, body := get("/wire/threads?kind=ask")
	var page service.WirePage
	if status != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil || len(page.Threads) != 1 || strings.Contains(body, "SECRET") {
		t.Fatalf("GET /wire/threads: %d %s", status, body)
	}
	if status, _ := get("/wire/threads/ask-wire1"); status != http.StatusOK {
		t.Fatalf("GET thread: %d", status)
	}
	if status, _ := get("/wire/threads/ask-missing"); status != http.StatusNotFound {
		t.Fatalf("missing thread: %d", status)
	}
	if status, _ := get("/wire/threads?cursor=bogus"); status != http.StatusUnprocessableEntity {
		t.Fatalf("bad cursor: %d", status)
	}
	if status, _ := get("/wire/threads?since=soon"); status != http.StatusUnprocessableEntity {
		t.Fatalf("bad since: %d", status)
	}
}
