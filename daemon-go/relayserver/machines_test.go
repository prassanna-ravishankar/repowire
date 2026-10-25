package relayserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// managedService answers the Worker's request API for one owner: two agents
// on the managed machine (one retired) and one that a daemon owns.
type managedService struct {
	*httptest.Server
	mu        sync.Mutex
	owner     string
	submitted []map[string]any
	failPost  int
}

func newManagedService(t *testing.T) *managedService {
	t.Helper()
	m := &managedService{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+controlToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.owner = r.Header.Get("X-Repowire-Owner")
		w.Header().Set("Content-Type", "application/json")
		write := func(status int, body string) { w.WriteHeader(status); _, _ = io.WriteString(w, body) }
		switch {
		case r.URL.Path == "/agents":
			write(200, `{"items":[
				{"id":"scout","display_name":"Scout","state":"working","home_machine":null,"retired_at":null},
				{"id":"old","state":"retired","home_machine":null,"retired_at":"2026-10-25T00:00:00Z"},
				{"id":"laptop-agent","state":"idle","home_machine":"laptop","retired_at":null}]}`)
		case r.URL.Path == "/agents/scout":
			write(200, `{"id":"scout","state":"idle","home_machine":null,"retired_at":null}`)
		case r.URL.Path == "/agents/laptop-agent":
			write(200, `{"id":"laptop-agent","state":"idle","home_machine":"laptop","retired_at":null}`)
		case strings.HasPrefix(r.URL.Path, "/agents/"):
			write(404, `{"error":"Agent not found"}`)
		case r.URL.Path == "/requests" && r.Method == http.MethodPost:
			if m.failPost > 0 {
				m.failPost--
				write(502, `{"error":"Delivery is uncertain"}`)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.submitted = append(m.submitted, body)
			write(202, `{"request_id":"`+body["id"].(string)+`","state":"accepted","created":true}`)
		case r.URL.Path == "/requests":
			write(200, `{"items":[{"request_id":"rq-1","to_agent":"scout","message":"hi","created_at":"2026-10-25T00:00:00Z","requester":"`+r.URL.Query().Get("requester")+`"}]}`)
		case strings.HasPrefix(r.URL.Path, "/requests/"):
			write(200, `{"correlation_id":"`+strings.TrimPrefix(r.URL.Path, "/requests/")+`","status":"resolved","close_reason":"answered","reply":"42","responder":"scout","requester":"`+r.URL.Query().Get("requester")+`","wait":"`+r.URL.Query().Get("wait")+`"}`)
		default:
			write(404, `{"error":"Not found"}`)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func TestRelayMCPManagedMachine(t *testing.T) {
	f := newAuthFixture(t)
	service := newManagedService(t)
	if err := f.s.ConfigureManaged(ManagedConfig{URL: service.URL, ControlToken: controlToken}); err != nil {
		t.Fatal(err)
	}
	access := f.tokens(t, oauthRead+" "+oauthWrite)["access_token"].(string)
	call := func(name string, args any) (map[string]any, bool) {
		t.Helper()
		status, result := f.rpc(t, access, "tools/call", map[string]any{"name": name, "arguments": args})
		if status != 200 || result["error"] != nil {
			t.Fatalf("%s HTTP %d: %v", name, status, result)
		}
		out := result["result"].(map[string]any)
		data, _ := out["structuredContent"].(map[string]any)
		return data, out["isError"] == true
	}
	owner, _ := f.s.tokens.validate(f.key)

	machines, _ := call("list_machines", struct{}{})
	rows := machines["machines"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["kind"] != "daemon" || rows[1].(map[string]any)["daemon_id"] != "managed" {
		t.Fatalf("machines %v", rows)
	}

	listing, _ := call("list_agents", map[string]any{"daemon_id": "managed"})
	agents := listing["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["peer_id"] != "scout" || agents[0].(map[string]any)["status"] != "busy" {
		t.Fatalf("only live agents no daemon owns are on the managed machine: %v", agents)
	}
	if service.owner != owner.UserID {
		t.Fatalf("owner %q, want the caller's relay user", service.owner)
	}

	if _, failed := call("ask_agent", map[string]any{"daemon_id": "managed", "peer_id": "laptop-agent", "message": "hi"}); !failed {
		t.Fatal("a daemon-owned agent was addressable on the managed machine")
	}

	sent, failed := call("ask_agent", map[string]any{"daemon_id": "managed", "peer_id": "scout", "message": "what is 6*7?"})
	if failed || sent["status"] != "pending" || !strings.HasPrefix(sent["request_id"].(string), "rq-") {
		t.Fatalf("ask %v", sent)
	}
	got := service.submitted[0]
	if got["id"] != sent["request_id"] || got["agent"] != "scout" || got["kind"] != "ask" || !strings.HasPrefix(got["requester"].(string), "relay-mcp-") {
		t.Fatalf("submitted %v", got)
	}

	reply, _ := call("get_reply", map[string]any{"daemon_id": "managed", "request_id": sent["request_id"], "wait_seconds": 5})
	if reply["status"] != "resolved" || reply["reply"] != "42" || reply["requester"] != got["requester"] || reply["wait"] != "5" {
		t.Fatalf("reply %v", reply)
	}
	pending, _ := call("list_requests", map[string]any{"daemon_id": "managed"})
	if r := pending["requests"].([]any); len(r) != 1 || r[0].(map[string]any)["requester"] != got["requester"] {
		t.Fatalf("pending %v", pending)
	}

	notified, failed := call("send_message", map[string]any{"daemon_id": "managed", "peer_id": "scout", "message": "fyi"})
	if failed || notified["delivered"] != true || service.submitted[1]["kind"] != "notify" {
		t.Fatalf("notify %v", notified)
	}

	// An outage after the request left the relay is uncertain, never a resend
	// invitation: the id comes back so get_reply can settle it.
	service.failPost = 1
	uncertain, failed := call("ask_agent", map[string]any{"daemon_id": "managed", "peer_id": "scout", "message": "again"})
	if failed || uncertain["status"] != "uncertain" || !strings.HasPrefix(uncertain["request_id"].(string), "rq-") {
		t.Fatalf("uncertain %v", uncertain)
	}
}

func TestManagedMachineAbsentWhenUnconfigured(t *testing.T) {
	f := newAuthFixture(t)
	access := f.tokens(t, oauthRead)["access_token"].(string)
	_, result := f.rpc(t, access, "tools/call", map[string]any{"name": "list_machines", "arguments": map[string]any{}})
	rows := result["result"].(map[string]any)["structuredContent"].(map[string]any)["machines"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["daemon_id"] != "laptop" {
		t.Fatalf("machines %v", rows)
	}
	_, result = f.rpc(t, access, "tools/call", map[string]any{"name": "list_agents", "arguments": map[string]any{"daemon_id": "managed"}})
	if result["result"].(map[string]any)["isError"] != true {
		t.Fatal("managed machine answered without a managed service")
	}
}
