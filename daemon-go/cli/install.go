package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/mobile"
	"github.com/repowire/repowire/daemon-go/relayserver"
)

var execLookPath = exec.LookPath

//go:embed assets/opencode.ts
var opencodePlugin string

//go:embed assets/pi.ts
var piPlugin string

//go:embed assets/channel/server.ts
var channelServer string

//go:embed assets/channel/daemon-session.ts
var channelDaemonSession string

//go:embed assets/channel/package.json
var channelPackage string

//go:embed assets/channel/bun.lock
var channelLock string

//go:embed all:assets/orchestrator
var orchestratorAssets embed.FS

func runSetup(argv []string) int {
	a := parse(argv, "relay", "http-mcp", "no-service", "non-interactive", "experimental-channels", "update-checks")
	if len(a.pos) > 0 {
		return usage("setup [--relay] [--experimental-channels] [--http-mcp] [--update-checks|--no-update-checks] [--no-service] [--non-interactive]")
	}
	allowed := map[string]bool{"relay": true, "http-mcp": true, "no-service": true, "non-interactive": true, "experimental-channels": true, "update-checks": true}
	for name := range a.flags {
		if !allowed[name] {
			return fatal(fmt.Errorf("unknown setup option --%s", name))
		}
	}
	if err := enableDaemonMCP(a.bool("relay")); err != nil {
		return fatal(err)
	}
	if _, configured := a.flags["update-checks"]; configured {
		if err := setUpdateChecks(a.bool("update-checks")); err != nil {
			return fatal(err)
		}
	}
	if err := cleanupRetiredRuntimeIntegrations(); err != nil {
		fmt.Fprintln(os.Stderr, "repowire: retired runtime cleanup:", err)
	}
	installed := 0
	for _, runtimeName := range []string{"claude-code", "codex", "opencode", "pi"} {
		if runtimeAvailable(runtimeName) {
			if err := installRuntime(runtimeName); err != nil {
				fmt.Fprintf(os.Stderr, "repowire: %s setup: %v\n", runtimeName, err)
			} else {
				fmt.Println("installed", runtimeName, "transport")
				installed++
			}
		}
	}
	if a.bool("experimental-channels") && runtimeAvailable("claude-code") {
		if err := installChannel(); err != nil {
			fmt.Fprintln(os.Stderr, "repowire: channel setup failed; full hooks remain installed:", err)
		} else {
			fmt.Println("installed claude-code experimental channel transport")
		}
	} else if !a.bool("experimental-channels") {
		if err := disableChannel(); err != nil {
			fmt.Fprintln(os.Stderr, "repowire: disable channel transport:", err)
		}
	}
	if installed == 0 {
		fmt.Println("no supported agent runtimes detected; daemon/MCP configured")
	}
	if err := installTmuxLifecycle(); err != nil {
		fmt.Fprintln(os.Stderr, "repowire: tmux lifecycle hooks:", err)
	}
	if !a.bool("no-service") {
		if err := installService(); err != nil {
			return fatal(err)
		}
	}
	return 0
}

func runtimeAvailable(name string) bool {
	binary := map[string]string{"claude-code": "claude", "codex": "codex", "opencode": "opencode", "pi": "pi"}[name]
	if binary == "" {
		return false
	}
	_, err := exec.LookPath(binary)
	return err == nil
}

func enableDaemonMCP(relay bool) error {
	path := config.Path()
	data := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	daemon := mapChild(data, "daemon")
	mcp := mapChild(daemon, "mcp_http")
	mcp["enabled"] = true
	mcp["bind"] = "localhost-only"
	mcp["require_auth"] = true
	if value, _ := daemon["auth_token"].(string); value == "" {
		daemon["auth_token"] = randomToken()
	}
	commands := mapChild(mapChild(daemon, "spawn"), "commands")
	for backend, oldDefault := range map[string]string{
		"gemini": "gemini --yolo", "antigravity": "agy --dangerously-skip-permissions",
	} {
		if command, _ := commands[backend].(string); command == oldDefault {
			delete(commands, backend)
		}
	}
	for backend, spec := range map[string]struct{ binary, command string }{
		"claude-code": {"claude", "claude --dangerously-skip-permissions"},
		"codex":       {"codex", "codex --dangerously-bypass-approvals-and-sandbox"},
		"opencode":    {"opencode", "opencode"},
		"pi":          {"pi", "pi"},
	} {
		if _, exists := commands[backend]; exists {
			continue
		}
		if _, err := execLookPath(spec.binary); err == nil {
			commands[backend] = spec.command
		}
	}
	if relay {
		relayConfig := mapChild(data, "relay")
		relayConfig["enabled"] = true
		if value, _ := relayConfig["api_key"].(string); value == "" {
			relayConfig["api_key"] = "rw_" + randomToken()
		}
	}
	raw, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func setUpdateChecks(enabled bool) error {
	path := config.Path()
	data := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(raw, &data); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	mapChild(data, "updates")["check_enabled"] = enabled
	raw, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func randomToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
func executable() string {
	path, err := os.Executable()
	if err != nil {
		return "repowire"
	}
	return stableExecutable(path)
}

func stableExecutable(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	if index := strings.Index(filepath.ToSlash(resolved), "/Cellar/repowire/"); index >= 0 {
		linked := filepath.FromSlash(filepath.ToSlash(resolved)[:index] + "/bin/repowire")
		if target, err := filepath.EvalSymlinks(linked); err == nil && target == resolved {
			return linked
		}
	}
	return resolved
}
func homebrewCellarPath(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/Cellar/repowire/")
}
func hookCommand(args string) string { return strconv.Quote(executable()) + " " + args }

func installRuntime(name string) error {
	switch name {
	case "claude-code":
		return installClaude()
	case "codex":
		return installCodex()
	case "opencode":
		if err := installPluginAsset("opencode", home(".config", "opencode", "plugins", "repowire.ts")); err != nil {
			return err
		}
		return removeIfExists(home(".opencode", "plugin", "repowire.ts"))
	case "pi":
		return installPluginAsset("pi", home(".pi", "agent", "extensions", "repowire.ts"))
	default:
		return fmt.Errorf("unknown runtime %s", name)
	}
}
func uninstallRuntime(name string) error {
	switch name {
	case "claude-code":
		return uninstallClaude()
	case "codex":
		return uninstallCodex()
	case "opencode":
		if err := removeIfExists(home(".config", "opencode", "plugins", "repowire.ts")); err != nil {
			return err
		}
		return removeIfExists(home(".opencode", "plugin", "repowire.ts"))
	case "pi":
		return removeIfExists(home(".pi", "agent", "extensions", "repowire.ts"))
	default:
		return nil
	}
}

func installClaude() error {
	versionOutput, err := exec.Command("claude", "--version").Output()
	if err != nil || !claudeInboxVersionSupported(string(versionOutput)) {
		return errors.New("Claude Code 2.1.224 or newer is required for native inbox delivery")
	}
	path := home(".claude", "settings.json")
	data, err := readJSON(path, true)
	if err != nil {
		return err
	}
	hooks := mapChild(data, "hooks")
	entries := map[string]map[string]any{
		"Stop": hookEntry(hookCommand("hook stop"), "", 0), "StopFailure": hookEntry(hookCommand("hook stop"), "", 0),
		"SessionStart": hookEntry(hookCommand("hook session"), "", 0), "SessionEnd": hookEntry(hookCommand("hook session"), "", 0),
		"UserPromptSubmit": hookEntry(hookCommand("hook prompt"), "", 0), "Notification": hookEntry(hookCommand("hook notification"), "idle_prompt", 0),
	}
	cfg, _ := config.Load()
	if cfg.Experiments.RemoteToolApproval.Enabled {
		entries["PreToolUse"] = hookEntry(hookCommand("hook pretooluse"), strings.Join(cfg.Experiments.RemoteToolApproval.GatedTools, "|"), 60)
	} else {
		removeRepowireEntries(hooks, "PreToolUse")
	}
	for event, entry := range entries {
		replaceHook(hooks, event, entry)
	}
	if err := writeJSON(path, data); err != nil {
		return err
	}
	rootPath := home(".claude.json")
	root, err := readJSON(rootPath, true)
	if err != nil {
		return err
	}
	servers := mapChild(root, "mcpServers")
	servers["repowire"] = map[string]any{"type": "stdio", "command": executable(), "args": []string{"mcp"}, "env": map[string]string{"REPOWIRE_BACKEND": "claude-code"}}
	return writeJSON(rootPath, root)
}

func claudeInboxVersionSupported(output string) bool {
	fields := strings.Fields(output)
	return len(fields) > 0 && versionAtLeast(fields[0], 2, 1, 224)
}

func installChannel() error {
	if _, err := exec.LookPath("bun"); err != nil {
		return fmt.Errorf("bun runtime not found")
	}
	versionOutput, err := exec.Command("claude", "--version").Output()
	if err != nil || !versionAtLeast(strings.Fields(string(versionOutput))[0], 2, 1, 80) {
		return fmt.Errorf("Claude Code 2.1.80+ is required")
	}
	dir := home(".repowire", "channel")
	for name, content := range map[string]string{
		"server.ts":         channelServer,
		"daemon-session.ts": channelDaemonSession,
		"package.json":      channelPackage,
		"bun.lock":          channelLock,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "install")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bun install: %s", strings.TrimSpace(string(output)))
	}

	rootPath := home(".claude.json")
	root, err := readJSON(rootPath, true)
	if err != nil {
		return err
	}
	entry := map[string]any{"command": "bun", "args": []string{filepath.Join(dir, "server.ts")}}
	if cfg, err := config.Load(); err == nil && cfg.Daemon.AuthToken != "" {
		entry["env"] = map[string]string{"REPOWIRE_AUTH_TOKEN": cfg.Daemon.AuthToken}
	}
	mapChild(root, "mcpServers")["repowire-channel"] = entry
	if err := writeJSON(rootPath, root); err != nil {
		return err
	}

	// Channel mode still keeps Stop/StopFailure for dashboard chat turns, but
	// registration and inbound delivery are owned by the channel server.
	settingsPath := home(".claude", "settings.json")
	settings, err := readJSON(settingsPath, true)
	if err != nil {
		return err
	}
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		for _, event := range []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Notification", "PreToolUse"} {
			removeRepowireEntries(hooks, event)
		}
	}
	return writeJSON(settingsPath, settings)
}

func disableChannel() error {
	path := home(".claude.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	root, err := readJSON(path, true)
	if err != nil {
		return err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil || servers["repowire-channel"] == nil {
		return nil
	}
	delete(servers, "repowire-channel")
	return writeJSON(path, root)
}

func versionAtLeast(value string, want ...int) bool {
	parts := strings.Split(value, ".")
	for index, expected := range want {
		actual := versionPart(parts, index)
		if actual != expected {
			return actual > expected
		}
	}
	return true
}

func uninstallClaude() error {
	if err := uninstallJSONRuntime(home(".claude", "settings.json"), []string{"Stop", "StopFailure", "SessionStart", "SessionEnd", "UserPromptSubmit", "Notification", "PreToolUse"}, home(".claude.json")); err != nil {
		return err
	}
	rootPath := home(".claude.json")
	root, err := readJSON(rootPath, false)
	if err != nil {
		return err
	}
	if servers, ok := root["mcpServers"].(map[string]any); ok {
		delete(servers, "repowire-channel")
	}
	return writeJSON(rootPath, root)
}

func installCodex() error {
	hooksPath := home(".codex", "hooks.json")
	data, _ := readJSON(hooksPath, false)
	hooks := mapChild(data, "hooks")
	specs := map[string][]string{"SessionStart": {"hook session --backend=codex", "startup|resume|clear"}, "Stop": {"hook stop --backend=codex", ""}, "UserPromptSubmit": {"hook prompt --backend=codex", ""}}
	nativeThreads := codexAppServerSupported()
	for event, spec := range specs {
		if nativeThreads && event != "Stop" {
			removeRepowireEntries(hooks, event)
		} else {
			if nativeThreads {
				spec = []string{"hook stop --backend=codex --reminders-only", ""}
			}
			replaceHook(hooks, event, hookEntry(hookCommand(spec[0]), spec[1], 0))
		}
	}
	removeRepowireEntries(hooks, "SessionEnd")
	if err := writeJSON(hooksPath, data); err != nil {
		return err
	}
	configPath := home(".codex", "config.toml")
	raw, _ := os.ReadFile(configPath)
	content := string(raw)
	content = ensureTomlFeature(content, "hooks", "true")
	content = replaceTomlSection(content, "mcp_servers.repowire", []string{"command = " + strconv.Quote(executable()), "args = [\"mcp\"]"})
	content = replaceTomlSection(content, "mcp_servers.repowire.env", []string{"REPOWIRE_BACKEND = \"codex\""})
	for event, spec := range specs {
		if nativeThreads && event != "Stop" {
			continue
		}
		if nativeThreads {
			spec = []string{"hook stop --backend=codex --reminders-only", ""}
		}
		entries, _ := hooks[event].([]any)
		for groupIndex, raw := range entries {
			entry, _ := raw.(map[string]any)
			if !isRepowireHook(entry) {
				continue
			}
			handlers, _ := entry["hooks"].([]any)
			for handlerIndex, hraw := range handlers {
				handler, _ := hraw.(map[string]any)
				command, _ := handler["command"].(string)
				key := fmt.Sprintf("%s:%s:%d:%d", hooksPath, map[string]string{"SessionStart": "session_start", "Stop": "stop", "UserPromptSubmit": "user_prompt_submit"}[event], groupIndex, handlerIndex)
				content = replaceTomlSection(content, "hooks.state.\""+key+"\"", []string{"trusted_hash = \"" + codexHookHash(event, command, spec[1]) + "\""})
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(configPath, []byte(content), 0o600)
}

func installPluginAsset(name, target string) error {
	source, err := pluginAsset(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(source), 0o600)
}

func pluginAsset(name string) (string, error) {
	switch name {
	case "opencode":
		return opencodePlugin, nil
	case "pi":
		return piPlugin, nil
	default:
		return "", fmt.Errorf("unknown plugin asset %q", name)
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func antigravityManifestPath() string {
	return home(".gemini", "antigravity-cli", "import_manifest.json")
}
func removeLegacyAntigravityManifestEntry() error {
	path := antigravityManifestPath()
	data, err := readJSON(path, false)
	if err != nil {
		return err
	}
	items, _ := data["imports"].([]any)
	kept := []any{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if fmt.Sprint(item["name"]) != "repowire" {
			kept = append(kept, raw)
		}
	}
	if len(kept) == 0 {
		return removeIfExists(path)
	}
	data["imports"] = kept
	return writeJSON(path, data)
}
func uninstallAntigravity() error {
	if err := os.RemoveAll(home(".gemini", "antigravity-cli", "plugins", "repowire")); err != nil {
		return err
	}
	return removeLegacyAntigravityManifestEntry()
}

// cleanupRetiredRuntimeIntegrations removes only Repowire-owned entries left
// by releases that supported Gemini CLI and Antigravity. Other user settings
// and plugins are preserved.
func cleanupRetiredRuntimeIntegrations() error {
	if err := uninstallJSONRuntime(home(".gemini", "settings.json"), []string{"SessionStart", "SessionEnd", "BeforeAgent", "AfterAgent"}, home(".gemini", "settings.json")); err != nil {
		return err
	}
	return uninstallAntigravity()
}

func hookEntry(command, matcher string, timeout int) map[string]any {
	handler := map[string]any{"type": "command", "command": command}
	if timeout > 0 {
		handler["timeout"] = timeout
	}
	entry := map[string]any{"hooks": []any{handler}}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	return entry
}
func replaceHook(hooks map[string]any, event string, entry map[string]any) {
	removeRepowireEntries(hooks, event)
	items, _ := hooks[event].([]any)
	hooks[event] = append(items, entry)
}
func removeRepowireEntries(hooks map[string]any, event string) {
	items, _ := hooks[event].([]any)
	kept := items[:0]
	for _, raw := range items {
		entry, _ := raw.(map[string]any)
		if !isRepowireHook(entry) {
			kept = append(kept, raw)
		}
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
}
func isRepowireHook(entry map[string]any) bool {
	items, _ := entry["hooks"].([]any)
	for _, raw := range items {
		handler, _ := raw.(map[string]any)
		if strings.Contains(fmt.Sprint(handler["command"]), "repowire") {
			return true
		}
		if strings.Contains(fmt.Sprint(handler["command"]), executable()) {
			return true
		}
	}
	return false
}

func codexHookHash(event, command, matcher string) string {
	label := map[string]string{"SessionStart": "session_start", "Stop": "stop", "UserPromptSubmit": "user_prompt_submit"}[event]
	identity := map[string]any{"event_name": label, "hooks": []any{map[string]any{"async": false, "command": command, "timeout": 600, "type": "command"}}}
	if matcher != "" {
		identity["matcher"] = matcher
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func ensureTomlFeature(content, key, value string) string {
	section := "[features]"
	if !strings.Contains(content, section) {
		return strings.TrimRight(content, "\n") + "\n\n" + section + "\n" + key + " = " + value + "\n"
	}
	lines := strings.Split(content, "\n")
	in := false
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") {
			in = trim == section
			continue
		}
		if in && strings.HasPrefix(trim, key+" ") {
			lines[i] = key + " = " + value
			return strings.Join(lines, "\n")
		}
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == section {
			lines = append(lines[:i+1], append([]string{key + " = " + value}, lines[i+1:]...)...)
			break
		}
	}
	return strings.Join(lines, "\n")
}
func replaceTomlSection(content, name string, lines []string) string {
	header := "[" + name + "]"
	source := strings.Split(content, "\n")
	out := []string{}
	inside := false
	found := false
	for _, line := range source {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			if inside {
				inside = false
			}
			if trim == header {
				if !found {
					out = append(out, header)
					out = append(out, lines...)
					found = true
				}
				inside = true
				continue
			}
		}
		if !inside {
			out = append(out, line)
		}
	}
	if !found {
		out = append(out, "", header)
		out = append(out, lines...)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

func uninstallJSONRuntime(settingsPath string, events []string, mcpPath string) error {
	data, _ := readJSON(settingsPath, false)
	if hooks, ok := data["hooks"].(map[string]any); ok {
		for _, event := range events {
			removeRepowireEntries(hooks, event)
		}
	}
	if mcpPath == settingsPath {
		if servers, ok := data["mcpServers"].(map[string]any); ok {
			delete(servers, "repowire")
		}
	}
	if err := writeJSON(settingsPath, data); err != nil {
		return err
	}
	if mcpPath != "" && mcpPath != settingsPath {
		root, _ := readJSON(mcpPath, false)
		if servers, ok := root["mcpServers"].(map[string]any); ok {
			delete(servers, "repowire")
		}
		return writeJSON(mcpPath, root)
	}
	return nil
}
func uninstallCodex() error {
	if err := uninstallJSONRuntime(home(".codex", "hooks.json"), []string{"SessionStart", "Stop", "UserPromptSubmit", "SessionEnd"}, ""); err != nil {
		return err
	}
	path := home(".codex", "config.toml")
	raw, _ := os.ReadFile(path)
	return os.WriteFile(path, []byte(removeTomlSection(string(raw), "mcp_servers.repowire")), 0o600)
}
func removeTomlSection(content, name string) string {
	header, prefix, inside := "["+name+"]", "["+name+".", false
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if trimmed == header || strings.HasPrefix(trimmed, prefix) {
				inside = true
				continue
			}
			inside = false
		}
		if !inside {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

func runRuntimeInstall(name string, argv []string) int {
	action := "status"
	if len(argv) > 0 {
		action = argv[0]
	}
	switch action {
	case "install":
		if err := installRuntime(name); err != nil {
			return fatal(err)
		}
		fmt.Println(name, "transport installed")
		return 0
	case "uninstall":
		if err := uninstallRuntime(name); err != nil {
			return fatal(err)
		}
		fmt.Println(name, "transport removed")
		return 0
	case "status":
		fmt.Printf("%s runtime: %s; integration: %s\n", name,
			map[bool]string{true: "detected", false: "not detected"}[runtimeAvailable(name)],
			map[bool]string{true: "installed", false: "not installed"}[runtimeIntegrated(name)])
		return 0
	default:
		return usage(name + " <install|uninstall|status>")
	}
}

func runtimeIntegrated(name string) bool {
	switch name {
	case "claude-code":
		settings, _ := readJSON(home(".claude", "settings.json"), false)
		hooks, _ := settings["hooks"].(map[string]any)
		root, _ := readJSON(home(".claude.json"), false)
		servers, _ := root["mcpServers"].(map[string]any)
		return hasRepowireHook(hooks, "Stop") && servers["repowire"] != nil
	case "codex":
		raw, _ := os.ReadFile(home(".codex", "config.toml"))
		return strings.Contains(string(raw), "[mcp_servers.repowire]")
	case "opencode":
		_, err := os.Stat(home(".config", "opencode", "plugins", "repowire.ts"))
		return err == nil
	case "pi":
		_, err := os.Stat(home(".pi", "agent", "extensions", "repowire.ts"))
		return err == nil
	default:
		return false
	}
}

func hasRepowireHook(hooks map[string]any, event string) bool {
	entries, _ := hooks[event].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if isRepowireHook(entry) {
			return true
		}
	}
	return false
}

func installTmuxLifecycle() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil
	}
	exe := strconv.Quote(executable())
	defs := [][]string{{"pane-exited", "-gw", exe + " lifecycle pane-died '#{hook_pane}'"}, {"session-closed", "-g", exe + " lifecycle session-closed '#{session_name}'"}, {"after-rename-session", "-g", exe + " lifecycle session-renamed '#{session_name}'"}, {"after-rename-window", "-gw", exe + " lifecycle window-renamed '#{window_name}' '#{session_name}'"}, {"client-detached", "-g", exe + " lifecycle client-detached '#{session_name}'"}}
	for _, def := range defs {
		command := "run-shell -b -- " + strconv.Quote(def[2])
		if out, err := exec.Command("tmux", "set-hook", def[1], def[0]+"[42]", command).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s", def[0], strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func runService(argv []string) int {
	if len(argv) == 0 {
		return usage("service <install|start|restart [daemon|bridge|all]|status|uninstall>")
	}
	switch argv[0] {
	case "install":
		if err := installService(); err != nil {
			return fatal(err)
		}
		fmt.Println("service installed")
		return 0
	case "start":
		if err := startService(); err != nil {
			return fatal(err)
		}
		fmt.Println("service started")
		return 0
	case "restart":
		component := "daemon"
		if len(argv) > 1 {
			component = argv[1]
		}
		var err error
		switch component {
		case "daemon":
			err = restartService()
		case "bridge":
			err = restartCodexBridgeService()
		case "all":
			if err = restartService(); err == nil {
				err = restartCodexBridgeService()
			}
		default:
			return usage("service restart [daemon|bridge|all]")
		}
		if err != nil {
			return fatal(err)
		}
		fmt.Println(component + " service restarted")
		return 0
	case "status":
		return serviceStatus()
	case "uninstall":
		if err := uninstallService(); err != nil {
			return fatal(err)
		}
		fmt.Println("service removed")
		return 0
	default:
		return usage("service <install|start|restart [daemon|bridge|all]|status|uninstall>")
	}
}

func serviceLabel() string                  { return "io.repowire.daemon" }
func codexBridgeLabel() string              { return "io.repowire.codex-bridge" }
func mobileServiceLabel(name string) string { return "io.repowire." + name }

func mobileServiceInstalled(name string) bool {
	if runtime.GOOS == "darwin" {
		_, err := os.Stat(home("Library", "LaunchAgents", mobileServiceLabel(name)+".plist"))
		return err == nil
	}
	_, err := os.Stat(home(".config", "systemd", "user", "repowire-"+name+".service"))
	return err == nil
}

func mobileServiceRunning(name string) bool {
	if runtime.GOOS == "darwin" {
		return launchAgentLoaded(mobileServiceLabel(name))
	}
	return systemdServiceActive("repowire-" + name + ".service")
}

const obsoleteServiceLabel = "com.repowire.daemon"

func installService() error {
	if runtime.GOOS == "darwin" {
		dir := home("Library", "LaunchAgents")
		_ = os.MkdirAll(dir, 0o755)
		obsoletePath := filepath.Join(dir, obsoleteServiceLabel+".plist")
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+obsoleteServiceLabel).Run()
		_ = os.Remove(obsoletePath)
		path := filepath.Join(dir, serviceLabel()+".plist")
		logPath := home(".repowire", "daemon.log")
		locale := os.Getenv("LC_ALL")
		if locale == "" {
			locale = os.Getenv("LANG")
		}
		if locale == "" {
			locale = "en_US.UTF-8"
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>serve</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string><key>LC_ALL</key><string>%s</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, serviceLabel(), executable(), html.EscapeString(os.Getenv("PATH")), html.EscapeString(locale), logPath, logPath)
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid()), path).Run()
		if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
			return err
		}
		if err := exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), path).Run(); err != nil {
			return err
		}
		if err := installCodexBridgeService(os.Getenv("PATH"), locale); err != nil {
			return err
		}
		return installConfiguredMobileServices(os.Getenv("PATH"), locale)
	}
	dir := home(".config", "systemd", "user")
	_ = os.MkdirAll(dir, 0o755)
	unit := fmt.Sprintf("[Unit]\nDescription=Repowire daemon\n[Service]\nExecStart=%s serve\nRestart=always\n[Install]\nWantedBy=default.target\n", executable())
	if err := os.WriteFile(filepath.Join(dir, "repowire.service"), []byte(unit), 0o600); err != nil {
		return err
	}
	if err := installCodexBridgeService(os.Getenv("PATH"), ""); err != nil {
		return err
	}
	if err := installConfiguredMobileServices(os.Getenv("PATH"), ""); err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return exec.Command("systemctl", "--user", "enable", "--now", "repowire.service").Run()
}

func installConfiguredMobileServices(pathValue, locale string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	configured := map[string]bool{
		"telegram": cfg.Telegram.BotToken != "" && cfg.Telegram.ChatID != "",
		"slack":    cfg.Slack.BotToken != "" && cfg.Slack.AppToken != "" && cfg.Slack.ChannelID != "",
	}
	for _, name := range []string{"telegram", "slack"} {
		if !configured[name] {
			if err := removeMobileService(name); err != nil {
				return err
			}
			continue
		}
		if err := installMobileService(name, pathValue, locale); err != nil {
			return err
		}
	}
	return nil
}

func installMobileService(name, pathValue, locale string) error {
	if runtime.GOOS == "darwin" {
		dir := home("Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, mobileServiceLabel(name)+".plist")
		logPath := home(".repowire", name+".log")
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>%s</string><string>start</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string><key>LC_ALL</key><string>%s</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, mobileServiceLabel(name), executable(), name, html.EscapeString(pathValue), html.EscapeString(locale), logPath, logPath)
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+mobileServiceLabel(name)).Run()
		if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
			return err
		}
		return exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), path).Run()
	}
	unitName := "repowire-" + name + ".service"
	unit := fmt.Sprintf("[Unit]\nDescription=Repowire %s peer\nAfter=repowire.service\nWants=repowire.service\n[Service]\nEnvironment=\"PATH=%s\"\nExecStart=%s %s start\nRestart=always\n[Install]\nWantedBy=default.target\n", name, pathValue, executable(), name)
	path := home(".config", "systemd", "user", unitName)
	if err := os.WriteFile(path, []byte(unit), 0o600); err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return exec.Command("systemctl", "--user", "enable", "--now", unitName).Run()
}

func removeMobileService(name string) error {
	if runtime.GOOS == "darwin" {
		label := mobileServiceLabel(name)
		path := home("Library", "LaunchAgents", label+".plist")
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+label).Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	unitName := "repowire-" + name + ".service"
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
	err := os.Remove(home(".config", "systemd", "user", unitName))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func installCodexBridgeService(pathValue, locale string) error {
	if !codexAppServerSupported() {
		return removeCodexBridgeService()
	}
	if runtime.GOOS == "darwin" {
		dir := home("Library", "LaunchAgents")
		path := filepath.Join(dir, codexBridgeLabel()+".plist")
		loaded := launchAgentLoaded(codexBridgeLabel())
		logPath := home(".repowire", "codex-bridge.log")
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>codex-bridge</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string><key>LC_ALL</key><string>%s</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, codexBridgeLabel(), executable(), html.EscapeString(pathValue), html.EscapeString(locale), logPath, logPath)
		if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
			return err
		}
		if loaded {
			return nil
		}
		return exec.Command("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), path).Run()
	}
	unit := codexBridgeSystemdUnit(pathValue)
	path := home(".config", "systemd", "user", "repowire-codex.service")
	active := systemdServiceActive("repowire-codex.service")
	if err := os.WriteFile(path, []byte(unit), 0o600); err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if active {
		return nil
	}
	return exec.Command("systemctl", "--user", "enable", "--now", "repowire-codex.service").Run()
}

func launchAgentLoaded(label string) bool {
	return exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+label).Run() == nil
}

func systemdServiceActive(unit string) bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", unit).Run() == nil
}

func codexBridgeSystemdUnit(pathValue string) string {
	return fmt.Sprintf("[Unit]\nDescription=Repowire Codex thread bridge\nAfter=repowire.service\nWants=repowire.service\n[Service]\nEnvironment=\"PATH=%s\"\nExecStart=%s codex-bridge\nRestart=always\n[Install]\nWantedBy=default.target\n", pathValue, executable())
}

func codexAppServerSupported() bool {
	path, err := execLookPath("codex")
	if err != nil {
		return false
	}
	out, err := exec.Command(path, "app-server", "--help").CombinedOutput()
	return err == nil && strings.Contains(string(out), "--listen")
}

func removeCodexBridgeService() error {
	if runtime.GOOS == "darwin" {
		path := home("Library", "LaunchAgents", codexBridgeLabel()+".plist")
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid()), path).Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "repowire-codex.service").Run()
	err := os.Remove(home(".config", "systemd", "user", "repowire-codex.service"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func startService() error {
	if runtime.GOOS == "darwin" {
		if err := startLaunchAgent(serviceLabel()); err != nil {
			return err
		}
		if _, err := os.Stat(home("Library", "LaunchAgents", codexBridgeLabel()+".plist")); err == nil {
			if err := startLaunchAgent(codexBridgeLabel()); err != nil {
				return err
			}
		}
		for _, name := range []string{"telegram", "slack"} {
			label := mobileServiceLabel(name)
			if _, err := os.Stat(home("Library", "LaunchAgents", label+".plist")); err == nil {
				if err := startLaunchAgent(label); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := exec.Command("systemctl", "--user", "start", "repowire.service").Run(); err != nil {
		return err
	}
	if _, err := os.Stat(home(".config", "systemd", "user", "repowire-codex.service")); err == nil {
		if err := exec.Command("systemctl", "--user", "start", "repowire-codex.service").Run(); err != nil {
			return err
		}
	}
	for _, name := range []string{"telegram", "slack"} {
		unit := "repowire-" + name + ".service"
		if _, err := os.Stat(home(".config", "systemd", "user", unit)); err == nil {
			if err := exec.Command("systemctl", "--user", "start", unit).Run(); err != nil {
				return err
			}
		}
	}
	return nil
}

func startLaunchAgent(label string) error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if err := exec.Command("launchctl", "kickstart", domain+"/"+label).Run(); err == nil {
		return nil
	}
	return exec.Command("launchctl", "bootstrap", domain, home("Library", "LaunchAgents", label+".plist")).Run()
}

func restartService() error {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+obsoleteServiceLabel).Run()
		_ = os.Remove(home("Library", "LaunchAgents", obsoleteServiceLabel+".plist"))
		if err := exec.Command("launchctl", "kickstart", "-k", "gui/"+strconv.Itoa(os.Getuid())+"/"+serviceLabel()).Run(); err != nil {
			return err
		}
		return nil
	}
	if err := exec.Command("systemctl", "--user", "restart", "repowire.service").Run(); err != nil {
		return err
	}
	return nil
}

func restartCodexBridgeService() error {
	if runtime.GOOS == "darwin" {
		path := home("Library", "LaunchAgents", codexBridgeLabel()+".plist")
		if _, err := os.Stat(path); err != nil {
			return err
		}
		return exec.Command("launchctl", "kickstart", "-k", "gui/"+strconv.Itoa(os.Getuid())+"/"+codexBridgeLabel()).Run()
	}
	return exec.Command("systemctl", "--user", "restart", "repowire-codex.service").Run()
}
func stopService() error {
	if runtime.GOOS == "darwin" {
		for _, name := range []string{"telegram", "slack"} {
			_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+mobileServiceLabel(name)).Run()
		}
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+codexBridgeLabel()).Run()
		return exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+serviceLabel()).Run()
	}
	for _, name := range []string{"telegram", "slack"} {
		_ = exec.Command("systemctl", "--user", "stop", "repowire-"+name+".service").Run()
	}
	_ = exec.Command("systemctl", "--user", "stop", "repowire-codex.service").Run()
	return exec.Command("systemctl", "--user", "stop", "repowire.service").Run()
}
func serviceStatus() int {
	var commands []*exec.Cmd
	if runtime.GOOS == "darwin" {
		commands = append(commands, exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+serviceLabel()))
		if _, err := os.Stat(home("Library", "LaunchAgents", codexBridgeLabel()+".plist")); err == nil {
			commands = append(commands, exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+codexBridgeLabel()))
		}
		for _, name := range []string{"telegram", "slack"} {
			label := mobileServiceLabel(name)
			if _, err := os.Stat(home("Library", "LaunchAgents", label+".plist")); err == nil {
				commands = append(commands, exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+label))
			}
		}
	} else {
		commands = append(commands, exec.Command("systemctl", "--user", "status", "repowire.service", "--no-pager"))
		if _, err := os.Stat(home(".config", "systemd", "user", "repowire-codex.service")); err == nil {
			commands = append(commands, exec.Command("systemctl", "--user", "status", "repowire-codex.service", "--no-pager"))
		}
		for _, name := range []string{"telegram", "slack"} {
			unit := "repowire-" + name + ".service"
			if _, err := os.Stat(home(".config", "systemd", "user", unit)); err == nil {
				commands = append(commands, exec.Command("systemctl", "--user", "status", unit, "--no-pager"))
			}
		}
	}
	for _, cmd := range commands {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return 1
		}
	}
	return 0
}
func uninstallService() error {
	for _, name := range []string{"telegram", "slack"} {
		if err := removeMobileService(name); err != nil {
			return err
		}
	}
	if err := removeCodexBridgeService(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		for _, label := range []string{serviceLabel(), obsoleteServiceLabel} {
			path := home("Library", "LaunchAgents", label+".plist")
			_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+label).Run()
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "repowire.service").Run()
	return os.Remove(home(".config", "systemd", "user", "repowire.service"))
}

func readJSON(path string, strict bool) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		if strict {
			return nil, fmt.Errorf("corrupted JSON at %s: %w", path, err)
		}
		return map[string]any{}, nil
	}
	return data, nil
}
func writeJSON(path string, data any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
func mapChild(parent map[string]any, key string) map[string]any {
	if child, ok := parent[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	parent[key] = child
	return child
}
func home(parts ...string) string {
	base, _ := os.UserHomeDir()
	return filepath.Join(append([]string{base}, parts...)...)
}

func runBuildUI() int {
	cmd := exec.Command("npm", "run", "build")
	cmd.Dir = "web"
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fatal(err)
	}
	return 0
}
func runUpdate() int {
	args := "--non-interactive"
	if channelConfigured() {
		args += " --experimental-channels"
	}
	if path, err := os.Executable(); err == nil {
		stable := stableExecutable(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil && homebrewCellarPath(resolved) {
			if code := runExternal("brew", "upgrade", "repowire"); code != 0 {
				return code
			}
			return runExternal(stable, append([]string{"setup"}, strings.Fields(args)...)...)
		}
	}
	command := "curl -fsSL https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh -s -- " + args
	return runExternal("sh", "-c", command)
}

func channelConfigured() bool {
	root, _ := readJSON(home(".claude.json"), false)
	servers, _ := root["mcpServers"].(map[string]any)
	return servers["repowire-channel"] != nil
}
func runUninstall(argv []string) int {
	for _, name := range []string{"claude-code", "codex", "opencode", "pi"} {
		_ = uninstallRuntime(name)
	}
	_ = cleanupRetiredRuntimeIntegrations()
	_ = uninstallService()
	if parse(argv, "yes").bool("yes") {
		_ = os.RemoveAll(home(".repowire"))
	}
	fmt.Println("Repowire integrations removed")
	return 0
}

func runRelay(argv []string) int {
	if len(argv) == 0 {
		return usage("relay <start|generate-key>")
	}
	a := parse(argv[1:])
	switch argv[0] {
	case "generate-key":
		fmt.Printf("Generated API key for %s:\n  rw_%s\n", a.string("user-id", "default"), randomToken())
		return 0
	case "start":
		host := a.string("host", "0.0.0.0")
		port := a.integer("port", 8000)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		addr := host + ":" + strconv.Itoa(port)
		fmt.Println("Repowire relay listening on", addr)
		if err := relayserver.ListenAndServe(ctx, addr, relayserver.FindWebOutputDir()); err != nil {
			return fatal(err)
		}
		return 0
	default:
		return usage("relay <start|generate-key>")
	}
}

func runBot(name string, argv []string) int {
	if len(argv) != 1 || argv[0] != "start" {
		return usage(name + " start")
	}
	cfg, err := config.Load()
	if err != nil {
		return fatal(err)
	}
	host := cfg.Daemon.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	daemonURL := os.Getenv("REPOWIRE_DAEMON_URL")
	if daemonURL == "" {
		daemonURL = "http://" + host + ":" + strconv.Itoa(cfg.Daemon.Port)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	peer := mobile.NewDaemonPeer(daemonURL, cfg.Daemon.AuthToken, name, "/"+name, "default")
	switch name {
	case "telegram":
		if err := mobile.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID, peer).Run(ctx); err != nil {
			return fatal(err)
		}
	case "slack":
		if err := mobile.NewSlack(cfg.Slack.BotToken, cfg.Slack.AppToken, cfg.Slack.ChannelID, peer).Run(ctx); err != nil {
			return fatal(err)
		}
	}
	return 0
}

func runExternal(name string, args ...string) int {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fatal(err)
	}
	return 0
}
