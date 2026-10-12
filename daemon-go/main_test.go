package main

import (
	"os"
	"strings"
	"testing"
)

// A bad -spawn-commands value disables spawn with a named warning; it never
// stops the daemon, and no partly decoded command survives.
func TestBadSpawnCommandsDisableSpawnInsteadOfExiting(t *testing.T) {
	commands, warning := parseSpawnCommands(`{"claude-code": "claude", "codex": 7}`)
	if len(commands) != 0 || !strings.Contains(warning, "-spawn-commands") || !strings.Contains(warning, "spawn is disabled") {
		t.Fatalf("commands %v warning %q", commands, warning)
	}
	commands, warning = parseSpawnCommands(`{"claude-code": "claude"}`)
	if commands["claude-code"] != "claude" || warning != "" {
		t.Fatalf("valid commands %v warning %q", commands, warning)
	}
}

// Optional features never reach a log.Fatal at startup: the routing
// constructor and the spawn-commands parser return warnings, not errors.
// main's remaining Fatalf calls are the pre-mesh conditions only.
func TestOnlyPreMeshConditionsAreFatal(t *testing.T) {
	src := readMain(t)
	allowed := []string{"codexbridge.Run", `"load config: %v"`, `"parse daemon addr %q: %v"`, `"open state db %q: %v"`, `"hydrate registry: %v"`, `"serve: %v"`}
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "log.Fatal") {
			continue
		}
		if i > 0 {
			line = lines[i-1] + line // the codex-bridge subcommand's guard is on the line before
		}
		ok := false
		for _, a := range allowed {
			ok = ok || strings.Contains(line, a)
		}
		if !ok {
			t.Errorf("unexpected fatal path: %s", strings.TrimSpace(line))
		}
	}
}

func readMain(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
