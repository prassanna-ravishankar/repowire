package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingConfigUsesDefaults(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg.Daemon.Host != defaultHost || cfg.Daemon.Port != defaultPort {
		t.Fatalf("daemon addr = %s:%d, want defaults", cfg.Daemon.Host, cfg.Daemon.Port)
	}
	if !cfg.Daemon.MCPHTTP.RequireAuth {
		t.Fatalf("mcp_http.require_auth default must stay true")
	}
	if cfg.Daemon.CircleBoundary != "session" {
		t.Fatalf("circle boundary = %q, want session", cfg.Daemon.CircleBoundary)
	}
	if cfg.Relay.URL != defaultRelayURL {
		t.Fatalf("relay url = %q, want %q", cfg.Relay.URL, defaultRelayURL)
	}
}

func TestLoadConfigYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if err := os.WriteFile(path, []byte(`
daemon:
  host: 0.0.0.0
  port: 9999
  circle_boundary: window
  auth_token: secret
  spawn:
    commands:
      claude-code: claude
      codex: codex --dangerously-bypass-approvals-and-sandbox
    allowed_paths:
      - /tmp/work
  mcp_http:
    enabled: true
relay:
  enabled: true
  url: wss://example.test
  api_key: key
telegram:
  bot_token: telegram-file-token
  chat_id: "42"
slack:
  bot_token: slack-file-token
  app_token: slack-file-app-token
  channel_id: C123
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.Host != "0.0.0.0" || cfg.Daemon.Port != 9999 || cfg.Daemon.AuthToken != "secret" {
		t.Fatalf("daemon config not loaded: %+v", cfg.Daemon)
	}
	if cfg.Daemon.CircleBoundary != "window" {
		t.Fatalf("circle boundary = %q, want window", cfg.Daemon.CircleBoundary)
	}
	if cfg.Daemon.Spawn.Commands["codex"] == "" || len(cfg.Daemon.Spawn.AllowedPaths) != 1 {
		t.Fatalf("spawn config not loaded: %+v", cfg.Daemon.Spawn)
	}
	if !cfg.Daemon.MCPHTTP.Enabled || !cfg.Daemon.MCPHTTP.RequireAuth {
		t.Fatalf("mcp_http defaults/overrides wrong: %+v", cfg.Daemon.MCPHTTP)
	}
	if !cfg.Relay.Enabled || cfg.Relay.URL != "wss://example.test" || cfg.Relay.APIKey != "key" {
		t.Fatalf("relay config not loaded: %+v", cfg.Relay)
	}
	if cfg.Telegram.BotToken != "telegram-file-token" || cfg.Telegram.ChatID != "42" {
		t.Fatalf("telegram config not loaded: %+v", cfg.Telegram)
	}
	if cfg.Slack.BotToken != "slack-file-token" || cfg.Slack.AppToken != "slack-file-app-token" || cfg.Slack.ChannelID != "C123" {
		t.Fatalf("slack config not loaded: %+v", cfg.Slack)
	}
}

func TestLoadEnvOverridesYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	t.Setenv("REPOWIRE_DAEMON__PORT", "8888")
	t.Setenv("REPOWIRE_DAEMON__CIRCLE_BOUNDARY", "window")
	t.Setenv("REPOWIRE_AUTH_TOKEN", "env-token")
	t.Setenv("REPOWIRE_SPAWN_COMMANDS", `{"codex":"codex"}`)
	t.Setenv("REPOWIRE_SPAWN_ALLOWED_PATHS", "/a,/b")
	t.Setenv("REPOWIRE_API_KEY", "relay-token")
	t.Setenv("REPOWIRE_DAEMON__MCP_HTTP__ENABLED", "true")
	t.Setenv("REPOWIRE_EXPERIMENTS__ACP_BROKER_CLIENT", "true")
	t.Setenv("TELEGRAM_BOT_TOKEN", "telegram-env-token")
	t.Setenv("REPOWIRE_TELEGRAM__CHAT_ID", "99")
	t.Setenv("SLACK_BOT_TOKEN", "slack-env-token")
	t.Setenv("REPOWIRE_SLACK__APP_TOKEN", "slack-env-app-token")
	t.Setenv("SLACK_CHANNEL_ID", "C999")
	if err := os.WriteFile(path, []byte(`
daemon:
  port: 9999
  auth_token: file-token
  spawn:
    commands:
      claude-code: claude
    allowed_paths:
      - /tmp/work
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.Port != 8888 || cfg.Daemon.AuthToken != "env-token" {
		t.Fatalf("env override failed: %+v", cfg.Daemon)
	}
	if cfg.Daemon.CircleBoundary != "window" {
		t.Fatalf("circle boundary env override = %q", cfg.Daemon.CircleBoundary)
	}
	if got := cfg.Daemon.Spawn.CommandsJSON(); got != `{"codex":"codex"}` {
		t.Fatalf("commands json = %s", got)
	}
	if len(cfg.Daemon.Spawn.AllowedPaths) != 2 || cfg.Daemon.Spawn.AllowedPaths[1] != "/b" {
		t.Fatalf("allowed paths = %#v", cfg.Daemon.Spawn.AllowedPaths)
	}
	if !cfg.Relay.Enabled || cfg.Relay.APIKey != "relay-token" {
		t.Fatalf("legacy relay key did not enable relay: %+v", cfg.Relay)
	}
	if !cfg.Daemon.MCPHTTP.Enabled || !cfg.Experiments.ACPBrokerClient {
		t.Fatalf("nested boolean env overrides failed: %+v %+v", cfg.Daemon.MCPHTTP, cfg.Experiments)
	}
	if cfg.Telegram.BotToken != "telegram-env-token" || cfg.Telegram.ChatID != "99" {
		t.Fatalf("telegram env overrides failed: %+v", cfg.Telegram)
	}
	if cfg.Slack.BotToken != "slack-env-token" || cfg.Slack.AppToken != "slack-env-app-token" || cfg.Slack.ChannelID != "C999" {
		t.Fatalf("slack env overrides failed: %+v", cfg.Slack)
	}
}

func TestLoadRejectsInvalidCircleBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if err := os.WriteFile(path, []byte("daemon:\n  circle_boundary: pane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("invalid circle boundary was accepted")
	}
}

func hasWarning(cfg Config, part string) bool {
	for _, w := range cfg.Warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

// TestLoadMistypedOptionalKeyWarnsAndKeepsTheRest (#427): a type error in an
// optional key is a warning and that key's default, never a failed load, which
// under launchd KeepAlive is a crash loop. Its siblings still load.
func TestLoadMistypedOptionalKeyWarnsAndKeepsTheRest(t *testing.T) {
	cfg, err := tryLoadYAML(t, `
daemon:
  port: 9100
  heartbeat_interval: 45
  spawn:
    commands: [codex]
    allowed_paths: [/work]
  mcp_http:
    enabled: true
    require_auth: maybe
relay:
  enabled: definitely
  url: wss://relay.example
telegram:
  chat_id: [1, 2]
  bot_token: tok
experiments:
  chat_turn_streaming: true
  remote_tool_approval:
    enabled: true
    timeout_seconds: soon
`)
	if err != nil {
		t.Fatalf("a mistyped optional key failed the load: %v", err)
	}
	checks := []struct {
		ok   bool
		what string
	}{
		{cfg.Daemon.Port == 9100 && cfg.Daemon.HeartbeatInterval == 45, "daemon siblings kept"},
		{len(cfg.Daemon.Spawn.Commands) == 0 && len(cfg.Daemon.Spawn.AllowedPaths) == 1, "spawn.commands dropped, allowed_paths kept"},
		{cfg.Daemon.MCPHTTP.Enabled && cfg.Daemon.MCPHTTP.RequireAuth, "mcp_http.enabled kept, require_auth back to its default (true)"},
		{!cfg.Relay.Enabled && cfg.Relay.URL == "wss://relay.example", "relay.enabled dropped, url kept"},
		{cfg.Telegram.ChatID == "" && cfg.Telegram.BotToken == "tok", "telegram.chat_id dropped, bot_token kept"},
		{cfg.Experiments.ChatTurnStreaming && cfg.Experiments.RemoteToolApproval.Enabled && cfg.Experiments.RemoteToolApproval.TimeoutSeconds == 45, "remote_tool_approval.timeout_seconds back to default, enabled kept"},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: %+v", c.what, cfg)
		}
	}
	for _, key := range []string{"daemon.spawn.commands", "daemon.mcp_http.require_auth", "relay.enabled", "telegram.chat_id", "experiments.remote_tool_approval.timeout_seconds"} {
		if !hasWarning(cfg, key) {
			t.Errorf("no warning names %s: %q", key, cfg.Warnings)
		}
	}
}

// TestLoadCoreKeyErrorsStayFatal: a key the daemon cannot run correctly
// without, a syntax error, or an unreadable daemon block still fails the load
// rather than starting on a silently different address or without its token.
func TestLoadCoreKeyErrorsStayFatal(t *testing.T) {
	for name, body := range map[string]string{
		"port":            "daemon:\n  port: eighty\n",
		"host":            "daemon:\n  host: [a, b]\n",
		"auth_token":      "daemon:\n  auth_token: {a: b}\n",
		"circle_boundary": "daemon:\n  circle_boundary: [session]\n",
		"daemon block":    "daemon: [1, 2]\n",
		"syntax":          "daemon:\n  port: 1\n bad indent: [\n",
	} {
		if _, err := tryLoadYAML(t, body); err == nil {
			t.Errorf("%s: load succeeded", name)
		}
	}
}

// Review #561: decoding key by key must not change a value that loaded before.
// A scalar that reads as a number in YAML keeps its exact text in a string
// field (a leading zero, an octal-looking token).
func TestLoadKeepsScalarTextExactly(t *testing.T) {
	cfg, err := tryLoadYAML(t, "daemon:\n  auth_token: 012345\n  heartbeat_interval: soon\ntelegram:\n  chat_id: 00042\n  bot_token: [x]\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.AuthToken != "012345" || cfg.Telegram.ChatID != "00042" {
		t.Fatalf("auth_token %q chat_id %q; want the text as written", cfg.Daemon.AuthToken, cfg.Telegram.ChatID)
	}
}

// Review #561: a security control that does not decode fails closed. A
// mistyped remote_tool_approval.enabled turns approval on rather than off.
func TestLoadMistypedApprovalFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"enabled mistyped": "experiments:\n  remote_tool_approval:\n    enabled: [true]\n",
		"block unreadable": "experiments:\n  remote_tool_approval: [enabled]\n",
	} {
		cfg, err := tryLoadYAML(t, body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !cfg.Experiments.RemoteToolApproval.Enabled || len(cfg.Experiments.RemoteToolApproval.GatedTools) == 0 {
			t.Errorf("%s: approval %+v; want enabled with the default gated tools", name, cfg.Experiments.RemoteToolApproval)
		}
		if !hasWarning(cfg, "remote_tool_approval") {
			t.Errorf("%s: no warning: %q", name, cfg.Warnings)
		}
	}
}

// Review #561: an experiments block that is not a mapping is an optional
// section like any other, for routing's probe too.
func TestLoadUnreadableExperimentsBlockDegrades(t *testing.T) {
	cfg, err := tryLoadYAML(t, "experiments: []\ndaemon:\n  port: 9101\n")
	if err != nil || cfg.Daemon.Port != 9101 || !hasWarning(cfg, "experiments") {
		t.Fatalf("experiments: [] = %v, port %d, warnings %q", err, cfg.Daemon.Port, cfg.Warnings)
	}
}
