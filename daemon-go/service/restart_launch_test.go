package service

import (
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestPermissionModeFromArgs(t *testing.T) {
	for _, tc := range []struct {
		backend proto.AgentType
		args    string
		want    string
	}{
		{proto.AgentClaudeCode, "claude --dangerously-skip-permissions", "bypassPermissions"},
		{proto.AgentClaudeCode, "node /x/cli.js --permission-mode plan --resume abc", "plan"},
		{proto.AgentClaudeCode, "claude --permission-mode=acceptEdits", "acceptEdits"},
		{proto.AgentClaudeCode, "claude --permission-mode bogus", ""},
		{proto.AgentClaudeCode, "claude", ""},
		{proto.AgentCodex, "codex --dangerously-bypass-approvals-and-sandbox", "bypass"},
		{proto.AgentCodex, "codex --yolo", "bypass"},
		{proto.AgentCodex, "codex --full-auto", "full-auto"},
		{proto.AgentOpenCode, "opencode --dangerously-skip-permissions", ""},
	} {
		if got := PermissionModeFromArgs(tc.backend, tc.args); got != tc.want {
			t.Errorf("%s %q = %q, want %q", tc.backend, tc.args, got, tc.want)
		}
	}
}

func TestWithPermissionModeReplacesTemplateFlags(t *testing.T) {
	for _, tc := range []struct {
		backend    proto.AgentType
		base, mode string
		want       string
	}{
		{proto.AgentClaudeCode, "claude --dangerously-skip-permissions --model opus", "plan", "claude --model opus --permission-mode plan"},
		{proto.AgentClaudeCode, "claude --permission-mode plan", "bypassPermissions", "claude --dangerously-skip-permissions"},
		{proto.AgentClaudeCode, "claude", "unknown", "claude"},
		{proto.AgentCodex, "codex --dangerously-bypass-approvals-and-sandbox", "full-auto", "codex --full-auto"},
		{proto.AgentCodex, "codex --full-auto", "default", "codex"},
	} {
		if got := WithPermissionMode(tc.base, tc.backend, tc.mode); got != tc.want {
			t.Errorf("WithPermissionMode(%q, %s) = %q, want %q", tc.base, tc.mode, got, tc.want)
		}
	}
}
