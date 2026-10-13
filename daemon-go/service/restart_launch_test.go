package service

import (
	"slices"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestObservePermission(t *testing.T) {
	for _, tc := range []struct {
		backend proto.AgentType
		args    string
		mode    string
		words   string
	}{
		{proto.AgentClaudeCode, "claude --dangerously-skip-permissions", "bypassPermissions", "--dangerously-skip-permissions"},
		{proto.AgentClaudeCode, "node /x/cli.js --permission-mode plan --resume abc", "plan", "--permission-mode plan"},
		{proto.AgentClaudeCode, "claude --permission-mode=acceptEdits", "acceptEdits", "--permission-mode=acceptEdits"},
		{proto.AgentClaudeCode, "claude --permission-mode bogus", PreservedVerbatim, "--permission-mode bogus"},
		{proto.AgentClaudeCode, "claude --allowedTools Bash", PreservedVerbatim, "--allowedTools Bash"},
		{proto.AgentClaudeCode, "claude --dangerously-skip-permissions --permission-mode plan", PreservedVerbatim, "--dangerously-skip-permissions --permission-mode plan"},
		{proto.AgentClaudeCode, "claude --model opus", "default", ""},
		{proto.AgentCodex, "codex --yolo", "bypass", "--yolo"},
		{proto.AgentCodex, "codex --full-auto", "full-auto", "--full-auto"},
		{proto.AgentCodex, "codex --sandbox read-only --ask-for-approval untrusted", PreservedVerbatim, "--sandbox read-only --ask-for-approval untrusted"},
		{proto.AgentCodex, "codex -s workspace-write -c approval_policy=never -c model=o3", PreservedVerbatim, "-s workspace-write -c approval_policy=never"},
		{proto.AgentOpenCode, "opencode", "default", ""},
	} {
		got := ObservePermission(tc.backend, strings.Fields(tc.args))
		if got.Mode != tc.mode || strings.Join(got.Words, " ") != tc.words {
			t.Errorf("%s %q = %+v, want %s %q", tc.backend, tc.args, got, tc.mode, tc.words)
		}
	}
}

func TestApplyPermissionReplacesTemplateFlagsKeepingQuotes(t *testing.T) {
	for _, tc := range []struct {
		backend proto.AgentType
		base    string
		words   []string
		want    string
	}{
		{proto.AgentClaudeCode, "claude --dangerously-skip-permissions --model opus", []string{"--permission-mode", "plan"}, "claude --model opus --permission-mode plan"},
		{proto.AgentClaudeCode, `claude --settings '{"a": 1}' --permission-mode plan`, []string{"--dangerously-skip-permissions"}, `claude --settings '{"a": 1}' --dangerously-skip-permissions`},
		{proto.AgentCodex, "codex --dangerously-bypass-approvals-and-sandbox", nil, "codex"},
		{proto.AgentCodex, "env A=1 codex --full-auto", []string{"--sandbox", "read-only"}, "env A=1 codex --sandbox read-only"},
	} {
		got, err := ApplyPermission(tc.base, tc.backend, tc.words)
		if err != nil || got != tc.want {
			t.Errorf("ApplyPermission(%q, %v) = %q, %v; want %q", tc.base, tc.words, got, err, tc.want)
		}
	}
}

func TestBuildResumeCommandReplacesExistingSessionArgs(t *testing.T) {
	for _, tc := range []struct {
		backend proto.AgentType
		base    string
		want    string
	}{
		{proto.AgentClaudeCode, "claude --resume old --model opus", "claude --model opus --resume new"},
		{proto.AgentClaudeCode, "claude -c --resume=old", "claude --resume new"},
		{proto.AgentCodex, "codex resume old", "codex resume new"},
		{proto.AgentCodex, "codex --full-auto resume --last", "codex --full-auto resume new"},
		{proto.AgentOpenCode, "opencode --session old", "opencode --session new"},
	} {
		got, err := BuildResumeCommand(tc.base, tc.backend, "new")
		if err != nil || got != tc.want {
			t.Errorf("BuildResumeCommand(%q) = %q, %v; want %q", tc.base, got, err, tc.want)
		}
	}
	for backend, base := range map[proto.AgentType]string{proto.AgentClaudeCode: "claude --resume old -c", proto.AgentCodex: "codex resume old"} {
		got, _ := StripResume(base, backend)
		if strings.Contains(got, "old") || strings.Contains(got, "resume") || strings.Contains(got, "-c") {
			t.Errorf("StripResume(%q) = %q, still selects a session", base, got)
		}
	}
}

func TestRedactCommand(t *testing.T) {
	got, redacted := RedactCommand(`env API_KEY=sk-1 claude --settings '{"t":"s3"}' --api-key=sk-2 --resume abc --model opus --dangerously-skip-permissions resume id-1 'a prompt'`)
	want := `env API_KEY=<redacted> claude --settings <redacted> --api-key=<redacted> --resume abc --model opus --dangerously-skip-permissions resume id-1 <redacted>`
	if got != want {
		t.Fatalf("redacted = %q\nwant       %q", got, want)
	}
	if !slices.Equal(redacted, []string{"API_KEY", "--settings", "--api-key", "positional"}) {
		t.Fatalf("redacted names = %v", redacted)
	}
}
