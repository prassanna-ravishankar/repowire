package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// codexEmbeddedFlags make a Codex TUI run a private embedded App Server instead
// of attaching to Codex's shared one (codex-cli 0.171 tui/src/daemon_startup.rs
// `exclusion`; `--search` forces embedded on its own, and `--approve-for-me`
// expands to approval and sandbox config overrides).
var codexEmbeddedFlags = set("--no-daemon", "--oss", "--strict-config", "--dangerously-bypass-hook-trust", "--search",
	"--approve-for-me", "--not-so-yolo", "-p", "--profile")

// codexEmbeddedEnv select an embedded server from the environment: an executor
// URL, or workload identity (codex-rs/login/src/auth/workload_identity.rs
// `has_marker`).
var codexEmbeddedEnv = []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE",
	"OPENAI_IDENTITY_TOKEN_SOURCE", "OPENAI_SPIFFE_AUDIENCE", "OPENAI_SPIFFE_ID"}

// CodexEmbeddedReason names the launch option that keeps a Codex TUI off
// Codex's shared App Server, or "" when the launch line attaches to it. The
// codex bridge only reaches threads on the shared server, so a session
// launched this way never receives mesh messages. env is the spawn
// environment. This is a static reading of the launch line; doctor's
// CODEX_NOT_ON_APP_SERVER reports what it cannot see (for example variables
// inherited from the tmux server).
func CodexEmbeddedReason(command string, env map[string]string) string {
	words, ok := splitShellWords(command)
	if !ok {
		return ""
	}
	prog := programIndex(words)
	launchEnv := codexLaunchEnv(words[:prog], env)
	for _, name := range codexEmbeddedEnv {
		if launchEnv[name] != "" {
			return name
		}
	}
	for i := prog + 1; i < endOfOptions(words, prog+1); i++ {
		word := words[i].Value
		if !isFlag(word) {
			continue
		}
		name, value, hasValue := strings.Cut(word, "=")
		if short := word[:2]; !strings.HasPrefix(word, "--") && len(word) > 2 && codexValueFlags[short] {
			// clap's attached short value: -pwork, -ckey=value, -c=key=value
			name, value, hasValue = short, strings.TrimPrefix(word[2:], "="), true
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

// codexLaunchEnv is the spawn environment overlaid with the command's own
// VAR=value prefix.
func codexLaunchEnv(prefix []shellWord, env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		out[k] = v
	}
	for _, w := range prefix {
		if k, v, ok := strings.Cut(w.Value, "="); ok {
			out[k] = v
		}
	}
	return out
}

// codexDaemonSafeOverride reports whether a `-c key=value` override keeps the
// shared App Server (codex `config_exclusion`): boolean feature toggles, as
// `features.<name>` or a non-empty `features` table of booleans, and the TUI's
// fullscreen_transcript display setting. Legacy top-level feature aliases are
// not recognised; spell them `features.<name>`.
func codexDaemonSafeOverride(override string) bool {
	key, value, _ := strings.Cut(override, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch {
	case key == "features":
		table, ok := codexBoolTable(value)
		return ok && len(table) > 0
	case key == "tui":
		table, ok := codexBoolTable(value)
		_, transcript := table["fullscreen_transcript"]
		return ok && len(table) == 1 && transcript
	case key == "suppress_unstable_features_warning" || key == "tui.fullscreen_transcript":
		return codexBool(value)
	case strings.HasPrefix(key, "features."):
		return !strings.Contains(strings.TrimPrefix(key, "features."), ".") && codexBool(value)
	}
	return false
}

// codexBool reports whether a TOML value is a bare boolean (a trailing
// comment allowed).
func codexBool(value string) bool {
	value, _, _ = strings.Cut(value, "#")
	value = strings.TrimSpace(value)
	return value == "true" || value == "false"
}

// codexBoolTable reads a flat inline TOML table whose values are all
// booleans; ok is false for anything else.
func codexBoolTable(value string) (map[string]bool, bool) {
	value, _, _ = strings.Cut(value, "#")
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return nil, false
	}
	table := map[string]bool{}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return table, true
	}
	for _, entry := range strings.Split(body, ",") {
		k, v, ok := strings.Cut(entry, "=")
		k = strings.TrimSpace(k)
		if len(k) >= 2 && (k[0] == '"' || k[0] == '\'') && k[len(k)-1] == k[0] {
			k = k[1 : len(k)-1] // a quoted key names the same entry
		} else if strings.Contains(k, ".") {
			return nil, false // a dotted key nests a table
		}
		if !ok || k == "" || strings.ContainsAny(k, "{}\"'\\") || !codexBool(v) {
			return nil, false
		}
		table[k] = strings.TrimSpace(v) == "true"
	}
	return table, true
}

// codexHooksTransport reports whether `repowire codex install` chose the
// hooks transport (Codex without a shared App Server) in the Codex home the
// launch uses, where every Codex TUI registers and receives through its own
// ws-hook whatever its launch options.
func codexHooksTransport(env map[string]string) bool {
	root := env["CODEX_HOME"]
	if root == "" {
		root = os.Getenv("CODEX_HOME")
	}
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".codex")
	}
	raw, err := os.ReadFile(filepath.Join(root, "hooks.json"))
	return err == nil && strings.Contains(string(raw), "hook session --backend=codex")
}

// IsCodexEmbeddedRefusal reports whether err is CheckCodexLaunch's refusal.
func IsCodexEmbeddedRefusal(err error) bool {
	var spawnErr *SpawnError
	if !errors.As(err, &spawnErr) {
		return false
	}
	detail, _ := spawnErr.Detail.(map[string]any)
	return detail["error"] == "codex_embedded_app_server"
}
