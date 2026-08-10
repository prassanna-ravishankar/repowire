package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestCodexPeerMCPRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	peer := &proto.Peer{Backend: proto.AgentCodex}
	command := "npx"
	spec := MCPServerSpec{Name: "docs", Type: "stdio", Command: &command, Args: []string{"server"}, Env: map[string]string{"TOKEN": "secret"}}
	if err := AddPeerMCP(context.Background(), peer, spec); err != nil {
		t.Fatal(err)
	}
	entries, err := ListPeerMCP(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "docs" || entries[0].Command == nil || *entries[0].Command != "npx" || len(entries[0].EnvKeys) != 1 || entries[0].EnvKeys[0] != "TOKEN" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
	if err := RemovePeerMCP(context.Background(), peer, "docs"); err != nil {
		t.Fatal(err)
	}
	entries, _ = ListPeerMCP(context.Background(), peer)
	if len(entries) != 0 {
		t.Fatalf("server not removed: %#v", entries)
	}
}

func TestGeminiPeerMCPPreservesOtherSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	peer := &proto.Peer{Backend: proto.AgentGemini}
	url := "http://127.0.0.1:9000/mcp"
	if err := AddPeerMCP(context.Background(), peer, MCPServerSpec{Name: "local", Type: "http", URL: &url}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) == "" || !containsAll(string(raw), `"theme": "dark"`, `"local"`) {
		t.Fatalf("settings not preserved: %s", raw)
	}
}

func containsAll(text string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(text, value) {
			return false
		}
	}
	return true
}
