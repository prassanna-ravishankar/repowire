package config

import (
	"os"
	"path/filepath"
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
