package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetentionVerbsCallTheDaemon(t *testing.T) {
	var got []string
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		got = append(got, call)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies[call] = body
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"policy": map[string]any{"content_days": 7, "records_days": 0}, "next_sweep": map[string]any{"events.strip": 3}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"counts": map[string]any{"events.strip": 3}})
	}))
	defer server.Close()
	c := &client{base: server.URL, http: server.Client()}

	for name, argv := range map[string][]string{
		"default": nil,
		"show":    {"show", "--json"},
		"dry":     {"sweep", "--dry-run"},
	} {
		if code := retentionCommand(c, argv); code != 0 {
			t.Fatalf("%s exited %d", name, code)
		}
	}
	if b := bodies["POST /retention/sweep"]; b["dry_run"] != true || b["vacuum"] != false {
		t.Fatalf("dry-run body = %v", b)
	}
	if code := retentionCommand(c, []string{"sweep", "--vacuum"}); code != 0 {
		t.Fatal("sweep --vacuum failed")
	}
	if b := bodies["POST /retention/sweep"]; b["dry_run"] != false || b["vacuum"] != true {
		t.Fatalf("vacuum body = %v", b)
	}
	if len(got) != 4 || got[0] != "GET /retention" || got[1] != "GET /retention" {
		t.Fatalf("calls = %v", got)
	}
	if code := retentionCommand(c, []string{"purge"}); code == 0 {
		t.Fatal("an unknown verb succeeded")
	}
}
