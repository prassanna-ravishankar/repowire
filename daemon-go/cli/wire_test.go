package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWireVerbsCallTheDaemon(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		if r.URL.Path == "/wire/threads" {
			_ = json.NewEncoder(w).Encode(map[string]any{"threads": []any{map[string]any{"id": "ask-1", "kind": "ask", "state": "delivered", "holder_turns": 2}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ask-1", "kind": "ask", "state": "delivered", "stages": []any{map[string]any{"seq": 1, "stage": "created"}}})
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}

	for name, code := range map[string]int{
		"ls":         wireCommand(c, []string{"ls", "--stuck", "--circle", "dev", "--since", "7d"}),
		"show":       wireCommand(c, []string{"show", "ask-1"}),
		"visibility": wireCommand(c, []string{"visibility", "beta", "private"}),
	} {
		if code != 0 {
			t.Fatalf("%s exited %d", name, code)
		}
	}
	want := map[string]bool{"GET /wire/threads?circle=dev&since=7d&stuck_only=true": false, "GET /wire/threads/ask-1": false, "POST /peers/beta/wire-visibility": false}
	for _, call := range got {
		if _, ok := want[call]; ok {
			want[call] = true
		}
	}
	for call, seen := range want {
		if !seen {
			t.Fatalf("missing %q in %q", call, got)
		}
	}
}
