package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configOverrides are the environment variables config.applyEnv reads; the
// check must see only the file under test.
var configOverrides = []string{
	"REPOWIRE_API_KEY", "REPOWIRE_AUTH_TOKEN", "REPOWIRE_DAEMON__AUTH_TOKEN", "REPOWIRE_DAEMON__HOST", "REPOWIRE_DAEMON__PORT",
	"REPOWIRE_DAEMON_HOST", "REPOWIRE_DAEMON_PORT", "REPOWIRE_DAEMON__CIRCLE_BOUNDARY", "REPOWIRE_DAEMON__MCP_HTTP__ENABLED",
	"REPOWIRE_DAEMON__MCP_HTTP__BIND", "REPOWIRE_DAEMON__MCP_HTTP__REQUIRE_AUTH", "REPOWIRE_RELAY__API_KEY", "REPOWIRE_RELAY__ENABLED",
	"REPOWIRE_RELAY__URL", "REPOWIRE_RELAY_API_KEY", "REPOWIRE_RELAY_URL", "TELEGRAM_BOT_TOKEN", "REPOWIRE_TELEGRAM__BOT_TOKEN",
	"TELEGRAM_CHAT_ID", "REPOWIRE_TELEGRAM__CHAT_ID", "SLACK_BOT_TOKEN", "REPOWIRE_SLACK__BOT_TOKEN", "SLACK_APP_TOKEN",
	"REPOWIRE_SLACK__APP_TOKEN", "SLACK_CHANNEL_ID", "REPOWIRE_SLACK__CHANNEL_ID", "REPOWIRE_CONFIG",
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	for _, key := range configOverrides {
		t.Setenv(key, "")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const healthyConfig = `daemon:
    host: 127.0.0.1
    port: 8377
    auth_token: tok
relay:
    enabled: true
    url: wss://relay.example
    api_key: rw_key
telegram:
    bot_token: "123:abc"
    chat_id: "42"
`

func TestConfigCheckReportsSubsystems(t *testing.T) {
	var out bytes.Buffer
	if code := configCheck(writeConfig(t, healthyConfig), []string{"relay", "telegram"}, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, want := range []string{"relay: enabled", "telegram: enabled", "slack: off"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "rw_key") || strings.Contains(out.String(), "123:abc") {
		t.Fatalf("report leaks a credential:\n%s", out.String())
	}
}

// A config the lenient binary accepted can now switch a credential block off
// (#561); the check must say so and fail the requirement.
func TestConfigCheckFailsWhenARequiredSubsystemIsDisabled(t *testing.T) {
	broken := strings.Replace(healthyConfig, "enabled: true", "enabled: definitely", 1)
	var out bytes.Buffer
	if code := configCheck(writeConfig(t, broken), []string{"relay", "telegram"}, &out); code != 2 {
		t.Fatalf("exit %d, want 2:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "relay is disabled because part of it could not be read") || !strings.Contains(out.String(), "relay: off") {
		t.Fatalf("report:\n%s", out.String())
	}
}

func TestConfigCheckFatalConfig(t *testing.T) {
	var out bytes.Buffer
	if code := configCheck(writeConfig(t, "daemon:\n    port: lots\n"), nil, &out); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
}

// No credential reaches the report: not a rejected value quoted by a decode
// error, not one embedded in the relay URL.
func TestConfigCheckNeverPrintsCredentials(t *testing.T) {
	for name, body := range map[string]string{
		"rejected token": strings.Replace(healthyConfig, `bot_token: "123:abc"`, "bot_token: !!int SECRET_BOT_TOKEN", 1),
		"core key":       strings.Replace(healthyConfig, "auth_token: tok", "auth_token: !!int SECRET_AUTH_TOKEN", 1),
		"relay url":      strings.Replace(healthyConfig, "url: wss://relay.example", "url: wss://user:SECRET_PASS@relay.example/ws?api_key=SECRET_QUERY", 1),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			configCheck(writeConfig(t, body), nil, &out)
			for _, secret := range []string{"SECRET_BOT_TOKEN", "SECRET_AUTH_TOKEN", "SECRET_PASS", "SECRET_QUERY", "rw_key", "123:abc"} {
				if strings.Contains(out.String(), secret) {
					t.Fatalf("report contains %q:\n%s", secret, out.String())
				}
			}
		})
	}
	var out bytes.Buffer
	configCheck(writeConfig(t, strings.Replace(healthyConfig, "url: wss://relay.example", "url: wss://u:p@relay.example/x?y=z", 1)), nil, &out)
	if !strings.Contains(out.String(), "url wss://relay.example,") {
		t.Fatalf("relay endpoint not shown as scheme and host:\n%s", out.String())
	}
}

// Checking a path that does not exist is an error, not a check of the defaults.
func TestConfigCheckMissingFile(t *testing.T) {
	var out bytes.Buffer
	if code := configCheck(filepath.Join(t.TempDir(), "nope.yaml"), []string{"relay"}, &out); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
}
