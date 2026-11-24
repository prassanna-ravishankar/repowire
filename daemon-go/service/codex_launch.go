package service

import (
	"os"
	"path/filepath"
	"strings"
)

// codexEmbeddedFlags make a Codex TUI run a private embedded App Server instead
// of attaching to Codex's shared one (codex-cli 0.171 tui/src/daemon_startup.rs
// `exclusion`, plus `--search`, which forces embedded on its own).
var codexEmbeddedFlags = set("--no-daemon", "--oss", "--strict-config", "--dangerously-bypass-hook-trust", "--search", "-p", "--profile")

// CodexEmbeddedReason names the launch option that keeps a Codex TUI off
// Codex's shared App Server, or "" when the launch attaches to it. The codex
// bridge only reaches threads on the shared server, so a session launched this
// way never receives mesh messages. env is the spawn environment.
func CodexEmbeddedReason(command string, env map[string]string) string {
	if env["CODEX_EXEC_SERVER_URL"] != "" {
		return "CODEX_EXEC_SERVER_URL"
	}
	words, ok := splitShellWords(command)
	if !ok {
		return ""
	}
	prog := programIndex(words)
	for _, w := range words[:prog] {
		if strings.HasPrefix(w.Value, "CODEX_EXEC_SERVER_URL=") {
			return "CODEX_EXEC_SERVER_URL"
		}
	}
	for i := prog + 1; i < endOfOptions(words, prog+1); i++ {
		word := words[i].Value
		if !isFlag(word) {
			continue
		}
		name, value, hasValue := strings.Cut(word, "=")
		if strings.HasPrefix(word, "-c") && !strings.HasPrefix(word, "--") && len(word) > 2 {
			name, value, hasValue = "-c", word[2:], true // clap's attached short value: -ckey=value
		}
		if codexEmbeddedFlags[name] {
			return name
		}
		if !codexValueFlags[name] {
			continue
		}
		if !hasValue && i+1 < len(words) {
			i++
			value = words[i].Value
		}
		if (name == "-c" || name == "--config") && !codexDaemonSafeOverride(value) {
			key, _, _ := strings.Cut(value, "=")
			return "-c " + strings.TrimSpace(key) // the key only: an override value may be a secret
		}
	}
	return ""
}

// codexDaemonSafeOverride reports whether a `-c key=value` override keeps the
// shared App Server: only boolean feature toggles and the two TUI display
// settings do. Inline tables under `features` or `tui` are accepted rather
// than parsed, so an unusual spelling is never refused.
func codexDaemonSafeOverride(override string) bool {
	key, value, _ := strings.Cut(override, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	isBool := value == "true" || value == "false"
	switch {
	case key == "features" || key == "tui":
		return strings.HasPrefix(value, "{")
	case key == "suppress_unstable_features_warning" || key == "tui.fullscreen_transcript":
		return isBool
	case strings.HasPrefix(key, "features."):
		return !strings.Contains(strings.TrimPrefix(key, "features."), ".") && isBool
	}
	return false
}

// codexHooksTransport reports whether `repowire codex install` chose the
// hooks transport (Codex without a shared App Server), where every Codex TUI
// registers and receives through its own ws-hook whatever its launch options.
func codexHooksTransport() bool {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".codex")
	}
	raw, err := os.ReadFile(filepath.Join(root, "hooks.json"))
	return err == nil && strings.Contains(string(raw), "hook session --backend=codex")
}
