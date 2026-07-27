package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"gopkg.in/yaml.v3"
)

func TestParseFlagsAfterPositionals(t *testing.T) {
	a := parse([]string{"peer", "hello", "--circle", "two", "--dry-run", "-m", "again"}, "dry-run")
	if strings.Join(a.pos, "|") != "peer|hello" || a.string("circle", "") != "two" || !a.bool("dry-run") || a.string("message", "") != "again" {
		t.Fatalf("unexpected parse: %+v", a)
	}
}

func TestTmuxCircleFromOutput(t *testing.T) {
	if got := tmuxCircleFromOutput(proto.CircleBoundarySession, "mesh\t@9\n"); got != "mesh" {
		t.Fatalf("session circle = %q", got)
	}
	if got := tmuxCircleFromOutput(proto.CircleBoundaryWindow, "mesh\t@9\n"); got != "window-9" {
		t.Fatalf("window circle = %q", got)
	}
}

func TestOrchestratorStartOnlyIncludesSourcePaneForCurrentCircle(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/spawn" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		body = nil
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port := endpoint.Port()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMUX_PANE", "%42")
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nprintf 'mesh\\t@9\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", configPath)
	if err := os.MkdirAll(filepath.Join(home, ".repowire", "orchestrator"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".repowire", "orchestrator", "AGENTS.md"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("daemon:\n  host: %s\n  port: %s\n", endpoint.Hostname(), port)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"orchestrator", "start", "--runtime", "codex", "--circle", "mesh"}); code != 0 {
		t.Fatalf("orchestrator start exited %d", code)
	}
	if got := body["source_pane"]; got != "%42" {
		t.Fatalf("source_pane = %#v, want %%42", got)
	}
	if code := Run([]string{"orchestrator", "start", "--runtime", "codex", "--circle", "other"}); code != 0 {
		t.Fatalf("cross-circle orchestrator start exited %d", code)
	}
	if _, ok := body["source_pane"]; ok {
		t.Fatalf("cross-circle spawn included source_pane: %#v", body)
	}
	if code := Run([]string{"peer", "new", home, "--circle", "mesh"}); code != 0 {
		t.Fatalf("peer new exited %d", code)
	}
	if got := body["source_pane"]; got != "%42" {
		t.Fatalf("peer new source_pane = %#v, want %%42", got)
	}
	if code := Run([]string{"peer", "new", home, "--circle", "other"}); code != 0 {
		t.Fatalf("cross-circle peer new exited %d", code)
	}
	if _, ok := body["source_pane"]; ok {
		t.Fatalf("cross-circle peer spawn included source_pane: %#v", body)
	}
}

func TestEmbeddedRuntimesDoNotOfferCircleMutation(t *testing.T) {
	for name, asset := range map[string]string{"pi": piPlugin, "opencode": opencodePlugin} {
		if strings.Contains(asset, "set_circle") {
			t.Fatalf("%s still exposes removed set_circle protocol", name)
		}
		for _, want := range []string{"circle_boundary", "window_id", "tmux_window", "pane_alive: true, circle"} {
			if !strings.Contains(asset, want) {
				t.Fatalf("%s embedded runtime lacks %q boundary parity", name, want)
			}
		}
	}
	for _, want := range []string{`role !== "orchestrator" && spawnCircle !== circle`, `tmuxPane && spawnCircle === circle`} {
		if !strings.Contains(piPlugin, want) {
			t.Fatalf("pi spawn lacks policy %q", want)
		}
	}
	if strings.Contains(piPlugin, "Circle maps to tmux session name") {
		t.Fatal("pi spawn description still claims circles always map to tmux sessions")
	}
}

func TestSubcommandHelpDoesNotRunCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if code := Run([]string{"setup", "--help"}); code != 0 {
		t.Fatalf("setup --help exited %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("setup --help mutated config: %v", err)
	}
}

func TestSetupRejectsUnknownOptionBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if code := Run([]string{"setup", "--htp-mcp"}); code == 0 {
		t.Fatal("setup accepted an unknown option")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid setup mutated config: %v", err)
	}
}

func TestEnableDaemonMCPPreservesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	if err := os.WriteFile(path, []byte("slack:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enableDaemonMCP(false); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	raw, _ := os.ReadFile(path)
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data["slack"] == nil {
		t.Fatal("unrelated config was dropped")
	}
	daemon := data["daemon"].(map[string]any)
	mcp := daemon["mcp_http"].(map[string]any)
	if mcp["enabled"] != true || daemon["auth_token"] == "" {
		t.Fatalf("mcp/auth not configured: %v", daemon)
	}
}

func TestSetupConfiguresDetectedSpawnCommandAndUpdateChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("REPOWIRE_CONFIG", path)
	previous := execLookPath
	execLookPath = func(name string) (string, error) {
		if name == "codex" {
			return "/bin/codex", nil
		}
		return "", os.ErrNotExist
	}
	t.Cleanup(func() { execLookPath = previous })
	if err := enableDaemonMCP(false); err != nil {
		t.Fatal(err)
	}
	if err := setUpdateChecks(true); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	raw, _ := os.ReadFile(path)
	if err := yaml.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	daemon := data["daemon"].(map[string]any)
	commands := daemon["spawn"].(map[string]any)["commands"].(map[string]any)
	if commands["codex"] == nil || data["updates"].(map[string]any)["check_enabled"] != true {
		t.Fatalf("setup config incomplete: %#v", data)
	}
}

func TestRemoveTomlSection(t *testing.T) {
	got := removeTomlSection("x=1\n[mcp_servers.repowire]\ncommand=\"x\"\n[mcp_servers.repowire.env]\nREPOWIRE_BACKEND=\"codex\"\n[mcp_servers.repowire.tools.whoami]\napproval_mode=\"approve\"\n[other]\ny=2\n", "mcp_servers.repowire")
	if strings.Contains(got, "repowire") || !strings.Contains(got, "[other]\ny=2") {
		t.Fatalf("unexpected TOML: %q", got)
	}
}

func TestReplaceCodexMCPConfigKeepsToolSettingsWithoutDuplicateEnv(t *testing.T) {
	content := "[mcp_servers.repowire]\ncommand=\"old\"\nenv = { REPOWIRE_BACKEND = \"codex\" }\n[mcp_servers.repowire.env]\nREPOWIRE_BACKEND=\"codex\"\n[mcp_servers.repowire.tools.whoami]\napproval_mode=\"approve\"\n[other]\nx=1\n"
	content = replaceTomlSection(content, "mcp_servers.repowire", []string{"command=\"new\"", "args=[\"mcp\"]"})
	content = replaceTomlSection(content, "mcp_servers.repowire.env", []string{"REPOWIRE_BACKEND=\"codex\""})
	if strings.Contains(content, "env = {") || strings.Count(content, "[mcp_servers.repowire.env]") != 1 || !strings.Contains(content, "[mcp_servers.repowire.tools.whoami]\napproval_mode=\"approve\"") {
		t.Fatalf("invalid MCP replacement:\n%s", content)
	}
}

func TestPluginAssetsExtractTypeScript(t *testing.T) {
	for _, name := range []string{"opencode", "pi"} {
		asset, err := pluginAsset(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(asset, "Repowire") || !strings.Contains(asset, "WebSocket") {
			t.Fatalf("%s asset was not extracted", name)
		}
	}
}

func TestChannelAssetsAndVersionGate(t *testing.T) {
	if !strings.Contains(channelServer, "Repowire") || !strings.Contains(channelPackage, "@modelcontextprotocol/sdk") {
		t.Fatal("channel assets were not embedded")
	}
	for _, want := range []string{"circle_boundary", "window_id", "tmux_window", "circle_source", "pane_id: placement.pane", "circle: placement.circle"} {
		if !strings.Contains(channelDaemonSession, want) {
			t.Fatalf("channel connector lacks %q boundary parity", want)
		}
	}
	if strings.Contains(channelDaemonSession, `?? "default"`) {
		t.Fatal("channel connector still assigns an implicit default circle")
	}
	if !versionAtLeast("2.1.80", 2, 1, 80) || !versionAtLeast("2.2.0", 2, 1, 80) || versionAtLeast("2.1.79", 2, 1, 80) {
		t.Fatal("Claude Code channel version gate is wrong")
	}
}

func TestDisableChannelKeepsNormalMCP(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	path := filepath.Join(homeDir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"repowire":{},"repowire-channel":{}},"other":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := disableChannel(); err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	servers := root["mcpServers"].(map[string]any)
	if servers["repowire"] == nil || servers["repowire-channel"] != nil || root["other"] != true {
		t.Fatalf("channel cleanup changed unrelated config: %#v", root)
	}
}

func TestVersionGreater(t *testing.T) {
	if !versionGreater("0.18.0", "0.17.9") || versionGreater("0.17.0", "0.17.0") || versionGreater("0.16.9", "0.17.0") {
		t.Fatal("semantic version comparison is wrong")
	}
	if versionGreater("", "0.17.0") || versionGreater("not-a-version", "0.17.0") {
		t.Fatal("malformed versions must not be considered upgrades")
	}
}

func TestServiceLabelKeepsExistingInstallIdentity(t *testing.T) {
	if serviceLabel() != "io.repowire.daemon" {
		t.Fatalf("service label = %q", serviceLabel())
	}
}

func TestMemoryBodyDropsExistingFrontmatter(t *testing.T) {
	content := "---\nname: old\n---\n\n# Old\n\nbody\n"
	if got := strings.TrimSpace(memoryBody(content)); got != "# Old\n\nbody" {
		t.Fatalf("memory body = %q", got)
	}
	if safeMemoryName("../escape") || !safeMemoryName("release-notes_1") {
		t.Fatal("memory name validation is unsafe")
	}
}

func TestOrchestratorTemplateAndPersonaAreStandalone(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	if _, err := initOrchestrator(false); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(homeDir, ".repowire", "orchestrator")
	for _, path := range []string{"AGENTS.md", "BOOTSTRAP.md", filepath.Join(".agents", "skills", "coordination", "SKILL.md")} {
		if _, err := os.Stat(filepath.Join(workspace, path)); err != nil {
			t.Fatalf("embedded orchestrator file %s missing: %v", path, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(workspace, "CLAUDE.md")); err != nil || target != "AGENTS.md" {
		t.Fatalf("CLAUDE.md link = %q, %v", target, err)
	}
	persona := filepath.Join(homeDir, ".repowire", "personas", "focused", "SOUL.md")
	if err := os.MkdirAll(filepath.Dir(persona), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persona, []byte("# Focused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runPersona([]string{"use", "focused"}); code != 0 {
		t.Fatalf("persona use exited %d", code)
	}
	if got := strings.TrimSpace(readText(filepath.Join(workspace, "personas", "ACTIVE_PERSONA"))); got != "focused" {
		t.Fatalf("active persona = %q", got)
	}
}
