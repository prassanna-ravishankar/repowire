package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeerMessageVerbsCallTheDaemon(t *testing.T) {
	var got []string
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.RequestURI()
		got = append(got, call)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies[call] = body
		switch {
		case r.URL.Path == "/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{"delivery": "pull", "messages": []any{
				map[string]any{"message_id": "msg-1", "kind": "ask", "from": "alpha", "correlation_id": "ask-1", "preview": "review?"},
			}})
		case strings.HasSuffix(r.URL.Path, "/delegate"):
			_ = json.NewEncoder(w).Encode(map[string]any{"delegated_to": "gamma", "delegations": []any{map[string]any{}}})
		case strings.HasSuffix(r.URL.Path, "/message-delivery"):
			_ = json.NewEncoder(w).Encode(map[string]any{"peer": "beta", "delivery": "pull"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"message_id": "msg-1", "kind": "ask", "from": "alpha", "text": "review?"})
		}
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}
	a := func(argv ...string) args { return parse(argv, peerBoolFlags...) }

	for name, code := range map[string]int{
		"messages": runPeerMessages(c, a("--peer", "beta", "--unread", "--limit", "5")),
		"peek":     runPeerRead(c, a("msg-1", "--peek")),
		"fetch":    runPeerRead(c, a("msg-1")),
		"delivery": runPeerDelivery(c, a("pull", "--peer", "beta")),
		"delegate": runPeerDelegate(c, a("ask-1", "gamma", "--note", "owns it", "--from-peer", "repow-b")),
	} {
		if code != 0 {
			t.Fatalf("%s exited %d", name, code)
		}
	}
	for _, want := range []string{
		"GET /messages?limit=5&peer=beta&unread=1",
		"GET /messages/msg-1",
		"POST /messages/msg-1/read",
		"POST /peers/beta/message-delivery",
		"POST /asks/ask-1/delegate",
	} {
		found := false
		for _, call := range got {
			found = found || call == want
		}
		if !found {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if b := bodies["POST /asks/ask-1/delegate"]; b["to_peer"] != "gamma" || b["note"] != "owns it" || b["from_peer"] != "repow-b" {
		t.Fatalf("delegate body = %v", b)
	}
	if b := bodies["POST /peers/beta/message-delivery"]; b["mode"] != "pull" {
		t.Fatalf("delivery body = %v", b)
	}
	if code := runPeerDelivery(c, a("sideways", "--peer", "beta")); code == 0 {
		t.Fatal("an unknown delivery mode was sent")
	}
}
