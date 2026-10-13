package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
		{proto.AgentClaudeCode, "claude --model opus", ModeDefault, ""},
		{proto.AgentCodex, "codex --dangerously-bypass-approvals-and-sandbox", "bypass", "--dangerously-bypass-approvals-and-sandbox"},
		{proto.AgentCodex, "codex -s read-only --ask-for-approval=never", "sandbox=read-only,approval=never", "-s read-only --ask-for-approval=never"},
		{proto.AgentOpenCode, "opencode", ModeDefault, ""},
	} {
		got := ObservePermission(tc.backend, strings.Fields(tc.args))
		if got.Mode != tc.mode || strings.Join(got.Words, " ") != tc.words {
			t.Errorf("%s %q = %+v, want %s %q", tc.backend, tc.args, got, tc.mode, tc.words)
		}
	}
}

// Anything outside the modeled shapes is argv-verbatim: variadic tool lists,
// unknown modes or values, conflicting sets, flags missing from codex 0.162.
func TestObservePermissionUnmodeledIsArgvVerbatim(t *testing.T) {
	for backend, cases := range map[proto.AgentType][]string{
		proto.AgentClaudeCode: {
			"claude --permission-mode bogus", "claude --permission-mode default", "claude --allowedTools Bash",
			"claude --disallowedTools Bash Edit", "claude --tools Read", "claude --allow-dangerously-skip-permissions",
			"claude --dangerously-skip-permissions --permission-mode plan", "claude --permission-prompt-tool x",
		},
		proto.AgentCodex: {
			"codex --sandbox read-only --ask-for-approval untrusted", "codex --full-auto", "codex --yolo",
			`codex -c sandbox_mode="read-only"`, "codex -c approval_policy=never", "codex --approve-for-me",
			"codex --dangerously-bypass-approvals-and-sandbox -s read-only",
		},
	} {
		for _, args := range cases {
			if got := ObservePermission(backend, strings.Fields(args)); got.Mode != ModeArgvVerbatim {
				t.Errorf("%q = %+v, want argv-verbatim", args, got)
			}
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
		{proto.AgentClaudeCode, "claude --disallowedTools Bash Edit --model opus", nil, "claude --model opus"},
		{proto.AgentCodex, "codex --dangerously-bypass-approvals-and-sandbox", nil, "codex"},
		{proto.AgentCodex, "env A=1 codex -s workspace-write -a on-request", []string{"-s", "read-only"}, "env A=1 codex -s read-only"},
	} {
		got, err := ApplyPermission(tc.base, tc.backend, tc.words)
		if err != nil || got != tc.want {
			t.Errorf("ApplyPermission(%q, %v) = %q, %v; want %q", tc.base, tc.words, got, err, tc.want)
		}
	}
	if _, err := ApplyPermission("codex --full-auto", proto.AgentCodex, nil); !errors.Is(err, ErrUnmodeledPermissions) {
		t.Errorf("an unmodeled template flag must not be stripped blindly: %v", err)
	}
}

func TestBuildResumeCommandReplacesExistingSessionArgs(t *testing.T) {
	for _, tc := range []struct {
		backend proto.AgentType
		base    string
		want    string
	}{
		{proto.AgentClaudeCode, "claude --resume old --model opus", "claude --model opus --resume new"},
		{proto.AgentClaudeCode, "claude --resume=old -r other --session-id=x --continue", "claude --resume new"},
		{proto.AgentCodex, "codex resume old", "codex resume new"},
		{proto.AgentCodex, "codex resume --model gpt-5 old-id", "codex --model gpt-5 resume new"},
		{proto.AgentCodex, "codex resume --model gpt-5 --last", "codex --model gpt-5 resume new"},
		{proto.AgentCodex, "codex --model resume", "codex --model resume resume new"},
		{proto.AgentOpenCode, "opencode --session old", "opencode --session new"},
	} {
		got, err := BuildResumeCommand(tc.base, tc.backend, "new")
		if err != nil || got != tc.want {
			t.Errorf("BuildResumeCommand(%q) = %q, %v; want %q", tc.base, got, err, tc.want)
		}
	}
	for backend, cases := range map[proto.AgentType]map[string]string{
		proto.AgentClaudeCode: {"claude --resume old -c": "claude", "claude -r old-id": "claude"},
		proto.AgentCodex: {
			"codex resume old":                    "codex",
			"codex resume --model gpt-5 --last":   "codex --model gpt-5",
			"codex resume --model gpt-5 old-id p": "codex --model gpt-5",
			"codex --model resume":                "codex --model resume",
		},
	} {
		for base, want := range cases {
			if got, _ := StripResume(base, backend); got != want {
				t.Errorf("StripResume(%q) = %q, want %q", base, got, want)
			}
		}
	}
}

func TestRedactCommandShowsOnlyKnownGrammar(t *testing.T) {
	id := "5e1f0c2a-0000-4000-8000-000000000001"
	got, redacted := RedactCommand(`env TOKEN=SECRET claude SECRET --model SECRET --settings='{"k":"SECRET"}' --resume SECRET --resume ` + id + ` --model opus -s read-only --permission-mode plan --dangerously-skip-permissions resume ` + id)
	want := `env TOKEN=<redacted> claude <redacted> --model <redacted> --settings=<redacted> --resume <redacted> --resume ` + id + ` --model opus -s read-only --permission-mode plan --dangerously-skip-permissions resume ` + id
	if got != want {
		t.Fatalf("redacted = %q\nwant       %q", got, want)
	}
	if strings.Contains(got, "SECRET") || !slices.Contains(redacted, "positional") {
		t.Fatalf("leak or missing names: %q %v", got, redacted)
	}
}

// A grandchild holding the output pipe must not outlive the deadline.
func TestBoundedCommandKillsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hang")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 6\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cmd, cancel := BoundedCommand(200*time.Millisecond, script)
	defer cancel()
	_, _ = cmd.CombinedOutput()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond+boundedWaitDelay+300*time.Millisecond {
		t.Fatalf("bounded command took %s", elapsed)
	}
}
