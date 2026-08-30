package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

type MCPServerEntry struct {
	Name    string   `json:"name"`
	Scope   string   `json:"scope"`
	Type    string   `json:"type"`
	Command *string  `json:"command"`
	Args    []string `json:"args"`
	URL     *string  `json:"url"`
	EnvKeys []string `json:"env_keys"`
}

type MCPServerSpec struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Command *string           `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     *string           `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Scope   string            `json:"-"`
}

var (
	ErrMCPUnsupported = errors.New("MCP config not supported for backend")
	ErrMCPDuplicate   = errors.New("MCP server already exists")
	ErrMCPNotFound    = errors.New("MCP server not configured")
	serverNameRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	envKeyRE          = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	codexSectionRE    = regexp.MustCompile(`^\[mcp_servers\.([^]]+)\]$`)
)

func validateMCPSpec(spec MCPServerSpec) error {
	if !serverNameRE.MatchString(spec.Name) || spec.Name == "__proto__" || spec.Name == "constructor" || spec.Name == "prototype" {
		return errors.New("server name must contain only letters, numbers, underscores, or hyphens")
	}
	for key := range spec.Env {
		if !envKeyRE.MatchString(key) {
			return errors.New("env keys must start with a letter or underscore and contain only letters, numbers, or underscores")
		}
	}
	if spec.Type == "" {
		spec.Type = "stdio"
	}
	if spec.Type == "stdio" && (spec.Command == nil || *spec.Command == "") {
		return errors.New("command is required for stdio type")
	}
	if (spec.Type == "http" || spec.Type == "sse") && (spec.URL == nil || *spec.URL == "") {
		return errors.New("url is required for http/sse type")
	}
	if spec.Type != "stdio" && spec.Type != "http" && spec.Type != "sse" {
		return errors.New("type must be stdio, http, or sse")
	}
	return nil
}

func MCPConfigScope(backend proto.AgentType) (map[string]any, error) {
	base := map[string]any{"backend": backend, "default_scope": "user"}
	switch backend {
	case proto.AgentClaudeCode:
		base["owner"], base["effective_scope"], base["label"] = "peer/project", "peer_project", "Claude Code peer/project config"
		base["description"], base["supported_scopes"], base["is_global"] = "Claude Code MCP edits can target user/global config or the peer's project/worktree via the selected add scope.", []string{"user", "project"}, false
	case proto.AgentCodex:
		base["owner"], base["effective_scope"], base["label"] = "backend", "backend_global", "Codex global backend config"
		base["description"], base["supported_scopes"], base["is_global"] = "Codex MCP edits target the user-level Codex config shared by Codex sessions on this host.", []string{"user"}, true
	default:
		return nil, fmt.Errorf("%w %s", ErrMCPUnsupported, backend)
	}
	return base, nil
}

func ListPeerMCP(ctx context.Context, peer *proto.Peer) ([]MCPServerEntry, error) {
	switch peer.Backend {
	case proto.AgentClaudeCode:
		return listClaudeMCP(ctx, peer.Path)
	case proto.AgentCodex:
		return listCodexMCP()
	default:
		return nil, fmt.Errorf("%w %s", ErrMCPUnsupported, peer.Backend)
	}
}

func AddPeerMCP(ctx context.Context, peer *proto.Peer, spec MCPServerSpec) error {
	if spec.Type == "" {
		spec.Type = "stdio"
	}
	if err := validateMCPSpec(spec); err != nil {
		return err
	}
	entries, err := ListPeerMCP(ctx, peer)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == spec.Name {
			return fmt.Errorf("%w: %q", ErrMCPDuplicate, spec.Name)
		}
	}
	switch peer.Backend {
	case proto.AgentClaudeCode:
		return addClaudeMCP(ctx, peer.Path, spec)
	case proto.AgentCodex:
		return addCodexMCP(spec)
	default:
		return fmt.Errorf("%w %s", ErrMCPUnsupported, peer.Backend)
	}
}

func RemovePeerMCP(ctx context.Context, peer *proto.Peer, name string) error {
	if !serverNameRE.MatchString(name) {
		return errors.New("invalid server name")
	}
	entries, err := ListPeerMCP(ctx, peer)
	if err != nil {
		return err
	}
	found := false
	for _, entry := range entries {
		if entry.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrMCPNotFound, name)
	}
	switch peer.Backend {
	case proto.AgentClaudeCode:
		return runClaudeMCP(ctx, peer.Path, "mcp", "remove", name)
	case proto.AgentCodex:
		return removeCodexMCP(name)
	default:
		return fmt.Errorf("%w %s", ErrMCPUnsupported, peer.Backend)
	}
}

func runClaudeMCP(ctx context.Context, cwd string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("claude %s timed out", strings.Join(args, " "))
	}
	if err != nil {
		return fmt.Errorf("claude %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

func listClaudeMCP(ctx context.Context, cwd string) ([]MCPServerEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "mcp", "list")
	if cwd != "" {
		cmd.Dir = cwd
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, errors.New("claude mcp list timed out")
	}
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "no mcp servers") {
			return []MCPServerEntry{}, nil
		}
		return nil, fmt.Errorf("claude mcp list failed: %s", strings.TrimSpace(string(out)))
	}
	entries := []MCPServerEntry{}
	for _, line := range strings.Split(string(out), "\n") {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || name == "" || strings.Contains(name, " ") {
			continue
		}
		if index := strings.Index(rest, " - "); index >= 0 {
			rest = rest[:index]
		}
		fields := strings.Fields(strings.TrimSpace(rest))
		if len(fields) == 0 {
			continue
		}
		entry := MCPServerEntry{Name: name, Scope: "user", Type: "stdio", Args: []string{}, EnvKeys: []string{}}
		if strings.HasPrefix(fields[0], "http://") || strings.HasPrefix(fields[0], "https://") {
			entry.Type = "http"
			entry.URL = &fields[0]
		} else {
			entry.Command = &fields[0]
			entry.Args = fields[1:]
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func addClaudeMCP(ctx context.Context, cwd string, spec MCPServerSpec) error {
	scope := spec.Scope
	if scope == "" {
		scope = "user"
	}
	args := []string{"mcp", "add", "-s", scope}
	if spec.Type == "http" || spec.Type == "sse" {
		args = append(args, "--transport", spec.Type)
	}
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-e", key+"="+spec.Env[key])
	}
	args = append(args, spec.Name)
	if spec.Type == "stdio" {
		args = append(args, "--", *spec.Command)
		args = append(args, spec.Args...)
	} else {
		args = append(args, *spec.URL)
	}
	if scope != "project" {
		cwd = ""
	}
	return runClaudeMCP(ctx, cwd, args...)
}

func codexMCPPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}
func listCodexMCP() ([]MCPServerEntry, error) {
	raw, err := os.ReadFile(codexMCPPath())
	if os.IsNotExist(err) {
		return []MCPServerEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	sections := parseCodexSections(string(raw))
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := []MCPServerEntry{}
	for _, name := range names {
		body := sections[name]
		entry := MCPServerEntry{Name: name, Scope: "user", Type: "stdio", Args: parseTOMLStrings(body["args"]), EnvKeys: parseTOMLEnvKeys(body["env"])}
		if value := parseTOMLString(body["command"]); value != "" {
			entry.Command = &value
		}
		if value := parseTOMLString(body["url"]); value != "" {
			entry.URL = &value
			entry.Type = "http"
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
func parseCodexSections(content string) map[string]map[string]string {
	out := map[string]map[string]string{}
	current := ""
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if match := codexSectionRE.FindStringSubmatch(trim); match != nil {
			current = match[1]
			out[current] = map[string]string{}
			continue
		}
		if strings.HasPrefix(trim, "[") {
			current = ""
			continue
		}
		if current != "" {
			if key, value, ok := strings.Cut(trim, "="); ok {
				out[current][strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	return out
}
func parseTOMLString(raw string) string { value, _ := strconv.Unquote(raw); return value }
func parseTOMLStrings(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		out = []string{}
	}
	return out
}
func parseTOMLEnvKeys(raw string) []string {
	matches := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*=`).FindAllStringSubmatch(raw, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match[1])
	}
	sort.Strings(out)
	return out
}
func addCodexMCP(spec MCPServerSpec) error {
	var body strings.Builder
	fmt.Fprintf(&body, "[mcp_servers.%s]\n", spec.Name)
	if spec.Command != nil {
		raw, _ := json.Marshal(*spec.Command)
		fmt.Fprintf(&body, "command = %s\n", raw)
	}
	if len(spec.Args) > 0 {
		raw, _ := json.Marshal(spec.Args)
		fmt.Fprintf(&body, "args = %s\n", raw)
	}
	if spec.URL != nil {
		raw, _ := json.Marshal(*spec.URL)
		fmt.Fprintf(&body, "url = %s\n", raw)
	}
	if len(spec.Env) > 0 {
		keys := make([]string, 0, len(spec.Env))
		for key := range spec.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := []string{}
		for _, key := range keys {
			raw, _ := json.Marshal(spec.Env[key])
			pairs = append(pairs, key+" = "+string(raw))
		}
		fmt.Fprintf(&body, "env = { %s }\n", strings.Join(pairs, ", "))
	}
	existing, err := os.ReadFile(codexMCPPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := strings.TrimRight(string(existing), "\n")
	if content != "" {
		content += "\n\n"
	}
	content += body.String()
	return atomicWrite(codexMCPPath(), []byte(content), 0o600)
}
func removeCodexMCP(name string) error {
	path := codexMCPPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out []string
	skipping := false
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "[mcp_servers."+name+"]" {
			skipping = true
			continue
		}
		if skipping && strings.HasPrefix(trim, "[") {
			skipping = false
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return atomicWrite(path, []byte(strings.Join(out, "")), 0o600)
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := bytes.NewReader(content).WriteTo(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
