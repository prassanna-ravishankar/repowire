package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/cli"
	"github.com/repowire/repowire/daemon-go/config"
)

// repoRoot is the repository root: the directory above daemon-go.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json")); err != nil {
		t.Fatalf("no .claude-plugin/marketplace.json above daemon-go: %v", err)
	}
	return dir
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The Claude Code plugin ships beside the binary (#448). This keeps the two
// from drifting apart: the plugin's version is the binary's, every
// marketplace entry resolves to a plugin of that name, every skill names its
// own directory and says what it is for, and every Repowire tool a skill
// shows a call to is one the daemon's MCP server serves.
func TestClaudePluginMatchesTheBinary(t *testing.T) {
	root := repoRoot(t)

	var plugin struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	readJSON(t, filepath.Join(root, ".claude-plugin", "plugin.json"), &plugin)
	if plugin.Version != cli.Version {
		t.Errorf(".claude-plugin/plugin.json version %q, binary %q: bump them together (docs/contributing/versioning.md)", plugin.Version, cli.Version)
	}

	var market struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	readJSON(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), &market)
	seen := map[string]bool{}
	for _, entry := range market.Plugins {
		if seen[entry.Name] {
			t.Errorf("marketplace lists %q twice", entry.Name)
		}
		seen[entry.Name] = true
		var manifest struct {
			Name string `json:"name"`
		}
		readJSON(t, filepath.Join(root, entry.Source, ".claude-plugin", "plugin.json"), &manifest)
		if manifest.Name != entry.Name {
			t.Errorf("marketplace entry %q (source %s) resolves to plugin %q", entry.Name, entry.Source, manifest.Name)
		}
	}
	if !seen[plugin.Name] {
		t.Errorf("the marketplace does not list the root plugin %q", plugin.Name)
	}

	tools := mcpToolNames(t)
	skills, err := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	if err != nil || len(skills) == 0 {
		t.Fatalf("no skills found: %v", err)
	}
	call := regexp.MustCompile("`[^`]*?\\b([a-z][a-z_]+)\\(")
	for _, path := range skills {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		dir := filepath.Base(filepath.Dir(path))
		name, description := frontmatter(text)
		if name != dir {
			t.Errorf("skills/%s/SKILL.md names itself %q", dir, name)
		}
		if strings.TrimSpace(description) == "" {
			t.Errorf("skills/%s/SKILL.md has no description", dir)
		}
		for _, code := range codeSpans(text) {
			for _, m := range call.FindAllStringSubmatch("`"+code+"`", -1) {
				if !tools[m[1]] {
					t.Errorf("skills/%s/SKILL.md shows %s(...), which the MCP server does not serve", dir, m[1])
				}
			}
		}
	}
}

// mcpToolNames is every tool the daemon's MCP server lists.
func mcpToolNames(t *testing.T) map[string]bool {
	t.Helper()
	h, _ := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, RequireAuth: false, AllowUnauthenticatedLocalhost: true})
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, envelope := mcpRPC(t, srv.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil)
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	decodeResult(t, envelope, &list)
	out := map[string]bool{}
	for _, tl := range list.Tools {
		out[tl.Name] = true
	}
	if len(out) == 0 {
		t.Fatal("the MCP server lists no tools")
	}
	return out
}

// frontmatter reads a SKILL.md's name and description.
func frontmatter(text string) (name, description string) {
	body, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", ""
	}
	head, _, _ := strings.Cut(body, "\n---")
	for _, line := range strings.Split(head, "\n") {
		key, value, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.TrimSpace(value)
		case "description":
			description = strings.TrimSpace(value)
		}
	}
	return name, description
}

// codeSpans are a markdown text's inline code spans and fenced blocks.
func codeSpans(text string) []string {
	var out []string
	inFence := false
	var fence []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				out = append(out, strings.Join(fence, "\n"))
				fence = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fence = append(fence, line)
			continue
		}
		for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}
