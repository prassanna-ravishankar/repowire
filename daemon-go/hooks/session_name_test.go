package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestSessionTitlesTakeTheLatestOfEachKind(t *testing.T) {
	entries := []map[string]any{
		{"type": "ai-title", "aiTitle": "First guess"},
		{"type": "custom-title", "customTitle": "auth-refactor"},
		{"type": "user", "message": map[string]any{"role": "user", "content": "hi"}},
		{"type": "ai-title", "aiTitle": "Auth refactor plan"},
		{"type": "custom-title", "customTitle": "Fix Telegram access"},
	}
	custom, ai := sessionTitles(entries)
	if custom != "Fix Telegram access" || ai != "Auth refactor plan" {
		t.Fatalf("sessionTitles = %q, %q", custom, ai)
	}
	if custom, ai := sessionTitles(nil); custom != "" || ai != "" {
		t.Fatalf("an empty transcript named the session %q, %q", custom, ai)
	}
}

func TestNativeFieldsLeaveOutEmptyValues(t *testing.T) {
	if got := nativeFields("", ""); len(got) != 0 {
		t.Fatalf("nativeFields with nothing = %v", got)
	}
	got := nativeFields("Fix Telegram access", "Telegram fix")
	if got["native_name"] != "Fix Telegram access" || got["native_title"] != "Telegram fix" {
		t.Fatalf("nativeFields = %v", got)
	}
}

func TestAgentNameMatchesTheSession(t *testing.T) {
	listing := []byte(`[{"sessionId":"aaa","name":"repowire-29"},{"sessionId":"bbb","name":"repowire-session-titles-e5"}]`)
	if got := agentName(listing, "bbb"); got != "repowire-session-titles-e5" {
		t.Fatalf("agentName = %q", got)
	}
	if got := agentName(listing, "zzz"); got != "" {
		t.Fatalf("an unlisted session got %q", got)
	}
	if got := agentName([]byte("not json"), "aaa"); got != "" {
		t.Fatalf("a bad listing gave %q", got)
	}
}

// A set session name wins; an unnamed Claude Code session falls back to the
// default name agent view shows; other backends have no such fallback.
func TestSessionNativeName(t *testing.T) {
	looked := ""
	stub := claudeDefaultName
	claudeDefaultName = func(id string) string { looked = id; return "repo-e5" }
	t.Cleanup(func() { claudeDefaultName = stub })

	if got := sessionNativeName("claude-code", Payload{SessionID: "s1", SessionTitle: "Fix it"}); got != "Fix it" || looked != "" {
		t.Fatalf("named session = %q (looked up %q)", got, looked)
	}
	if got := sessionNativeName("claude-code", Payload{SessionID: "s1"}); got != "repo-e5" || looked != "s1" {
		t.Fatalf("unnamed claude-code = %q (looked up %q)", got, looked)
	}
	if got := sessionNativeName("codex", Payload{SessionID: "s2"}); got != "" {
		t.Fatalf("unnamed codex = %q, want no fallback", got)
	}
}

// UserPromptSubmit forwards the session's own name on the busy update it
// already sends, which is how a /rename reaches the mesh.
func TestPromptReportsTheSessionName(t *testing.T) {
	_, binDir := hookTestEnvironment(t)
	var update map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/update":
			_ = json.NewDecoder(r.Body).Decode(&update)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/peers/by-pane/%26":
			_ = json.NewEncoder(w).Encode(map[string]any{"peer_id": "repow-26", "description": "x"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configureHookTestDaemon(t, server.URL)
	t.Setenv("PATH", binDir+":/usr/bin:/bin")

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	handlePrompt(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "go", "session_title": "Fix Telegram access"}, "claude-code")
	os.Stdout = stdout
	_ = devnull.Close()
	if update["native_name"] != "Fix Telegram access" || update["status"] != "busy" {
		t.Fatalf("/session/update body = %v", update)
	}
}
