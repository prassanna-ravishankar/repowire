package service

import (
	"context"
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
