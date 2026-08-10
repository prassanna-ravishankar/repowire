package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// newEventsRig builds a hub with a real registry and an httptest server serving
// ONLY the events route group, so the test is independent of the other HTTP
// groups' wiring state. Auth is disabled (empty token via newTestHub).
func newEventsRig(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	h := newTestHub(t)
	mux := http.NewServeMux()
	h.EventRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

// postJSON is the package-shared test helper (routes_messaging_test.go /
// routes_ask_lifecycle_test.go); reused here, not redeclared.

// TestIngestChatTurnCanonicalizesAndAppears posts a chat turn resolved by
// pane_id, asserts the daemon canonicalises "peer" to the registered display
// name and stamps the canonical peer_id, then asserts GET /events surfaces the
// recorded chat_turn event with that canonical shape. This is the primary
// endpoint round-trip for the route group.
func TestIngestChatTurnCanonicalizesAndAppears(t *testing.T) {
	h, srv := newEventsRig(t)

	pane := "%7"
	path := "/work/repowire"
	id, name, err := h.reg.AllocateAndRegister(context.Background(), peer.AllocateParams{
		Circle:  "default",
		Backend: proto.AgentClaudeCode,
		Path:    &path,
		PaneID:  &pane,
		Role:    proto.RoleAgent,
		Machine: "test-machine",
	})
	if err != nil {
		t.Fatalf("AllocateAndRegister: %v", err)
	}

	// Post a chat turn addressed only by pane_id, with a deliberately wrong
	// "peer" name the daemon must overwrite with the canonical display_name.
	resp := postJSON(t, srv.URL+"/events/chat", map[string]any{
		"peer":    "stale-name",
		"role":    "assistant",
		"text":    "hello mesh",
		"turn_id": "turn-abc",
		"pane_id": pane,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /events/chat status = %d, want 200", resp.StatusCode)
	}
	var ok okResponse
	if err := json.NewDecoder(resp.Body).Decode(&ok); err != nil || !ok.OK {
		t.Fatalf("expected {ok:true}, got ok=%v err=%v", ok.OK, err)
	}

	// GET /events must surface the chat_turn with canonical peer_id + name.
	events := getEventsT(t, srv.URL+"/events")
	var found map[string]any
	for _, ev := range events {
		if ev["type"] == "chat_turn" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("chat_turn event not found in %d events", len(events))
	}
	if got := found["peer_id"]; got != string(id) {
		t.Errorf("peer_id = %v, want canonical %q", got, id)
	}
	if got := found["peer"]; got != string(name) {
		t.Errorf("peer = %v, want canonical display_name %q", got, name)
	}
	if got := found["text"]; got != "hello mesh" {
		t.Errorf("text = %v, want %q", got, "hello mesh")
	}
}

// TestChatDeltaDroppedAfterFinal verifies a chat_turn finalises its turn_id so a
// later chat_delta for the same (session,turn) is dropped with 200 (no retry
// loop) and never lands as an event.
func TestChatDeltaDroppedAfterFinal(t *testing.T) {
	_, srv := newEventsRig(t)

	sess := "sess-1"
	// Finalise the turn via a chat_turn.
	r1 := postJSON(t, srv.URL+"/events/chat", map[string]any{
		"peer":       "p",
		"role":       "assistant",
		"text":       "done",
		"turn_id":    "t-1",
		"session_id": sess,
	})
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", r1.StatusCode)
	}

	// A late delta for the same turn must be dropped with 200.
	r2 := postJSON(t, srv.URL+"/events/chat_delta", map[string]any{
		"peer":        "p",
		"role":        "assistant",
		"turn_id":     "t-1",
		"session_id":  sess,
		"chunk_index": 0,
		"kind":        "text",
		"text":        "late chunk",
	})
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("dropped delta status = %d, want 200", r2.StatusCode)
	}

	// No chat_turn_delta should have been recorded for the finalised turn.
	for _, ev := range getEventsT(t, srv.URL+"/events") {
		if ev["type"] == "chat_turn_delta" {
			t.Fatalf("late delta should not be recorded: %+v", ev)
		}
	}
}

// TestEventsSinceFiltersByID confirms the gap-recoverable since= cursor returns
// only events after the supplied id.
func TestEventsSinceFiltersByID(t *testing.T) {
	_, srv := newEventsRig(t)

	for _, txt := range []string{"one", "two", "three"} {
		r := postJSON(t, srv.URL+"/events/chat", map[string]any{
			"peer": "p", "role": "user", "text": txt,
		})
		r.Body.Close()
	}

	all := getEventsT(t, srv.URL+"/events")
	if len(all) < 3 {
		t.Fatalf("want >=3 events, got %d", len(all))
	}
	firstID, _ := all[0]["id"].(string)
	if firstID == "" {
		t.Fatalf("event missing id: %+v", all[0])
	}

	after := getEventsT(t, srv.URL+"/events?since="+firstID)
	if len(after) != len(all)-1 {
		t.Fatalf("since=first returned %d events, want %d", len(after), len(all)-1)
	}
	for _, ev := range after {
		if ev["id"] == firstID {
			t.Fatalf("since cursor must exclude the named id")
		}
	}
}

func getEventsT(t *testing.T, url string) []map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, resp.StatusCode)
	}
	var events []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	return events
}
