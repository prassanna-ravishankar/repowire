package hub

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHistorySourcePathResolvesRuntimeLocators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		raw, backend, want string
	}{
		{"claude-jsonl:/-tmp-repo/session.jsonl", "claude-code", filepath.Join(home, ".claude", "projects", "-tmp-repo", "session.jsonl")},
		{"codex-rollout:/2026/07/10/rollout-session.jsonl", "codex", filepath.Join(home, ".codex", "sessions", "2026", "07", "10", "rollout-session.jsonl")},
		{"~/history.jsonl", "codex", filepath.Join(home, "history.jsonl")},
	}
	for _, test := range tests {
		if got := historySourcePath(test.raw, test.backend); got != test.want {
			t.Fatalf("historySourcePath(%q, %q) = %q, want %q", test.raw, test.backend, got, test.want)
		}
	}
}

func TestHistoryParsersReplayClaudeAndCodexTurns(t *testing.T) {
	claudePath := filepath.Join(t.TempDir(), "claude-session.jsonl")
	claude := "" +
		`{"type":"user","sessionId":"claude-session","timestamp":"2026-07-10T10:00:00Z","message":{"content":"do it"}}` + "\n" +
		`{"type":"assistant","sessionId":"claude-session","uuid":"turn-1","timestamp":"2026-07-10T10:00:01Z","message":{"content":[{"type":"tool_use","name":"Read","input":{"file":"a.go"}},{"type":"text","text":"done"}]}}` + "\n"
	if err := os.WriteFile(claudePath, []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeTurns := parseClaudeHistory(claudePath)
	if len(claudeTurns) != 2 || claudeTurns[1].Text != "done" || len(claudeTurns[1].ToolCalls) != 1 {
		t.Fatalf("unexpected Claude replay: %#v", claudeTurns)
	}

	codexPath := filepath.Join(t.TempDir(), "rollout-session.jsonl")
	codex := "" +
		`{"type":"session_meta","payload":{"id":"codex-session","cwd":"/tmp/repo"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-07-10T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}}` + "\n" +
		`{"type":"turn_context","payload":{"turn_id":"turn-2","cwd":"/tmp/repo"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-07-10T10:00:01Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}` + "\n"
	if err := os.WriteFile(codexPath, []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	codexTurns := parseCodexHistory(codexPath)
	if len(codexTurns) != 2 || codexTurns[1].TurnID != "turn-2" || len(codexTurns[1].ToolCalls) != 1 {
		t.Fatalf("unexpected Codex replay: %#v", codexTurns)
	}
}
