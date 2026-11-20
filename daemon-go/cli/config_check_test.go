package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
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
