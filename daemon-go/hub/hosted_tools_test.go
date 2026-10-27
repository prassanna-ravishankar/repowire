package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

func TestRunHostedToolActsAsTheHostedPeer(t *testing.T) {
	h, reg := newMCPTestHub(t, config.MCPHTTPConfig{Enabled: true, AllowUnauthenticatedLocalhost: true})
	ctx := context.Background()
	register := func(backend proto.AgentType, path string) proto.PeerID {
		id, _, err := reg.AllocateAndRegister(ctx, peer.AllocateParams{
			Circle: "default", Backend: backend, Role: proto.RoleAgent, Path: &path,
			Metadata: map[string]any{"in_process": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	hosted, local := register(proto.AgentHosted, "/hosted/helper"), register(proto.AgentClaudeCode, "/work/local")

	text, isError := h.RunHostedTool(ctx, hosted, "whoami", nil)
	if isError || !strings.Contains(text, string(hosted)) {
		t.Fatalf("whoami = %q (error %v), want the hosted peer", text, isError)
	}
	args, _ := json.Marshal(map[string]string{"description": "triaging flaky tests"})
	if text, isError := h.RunHostedTool(ctx, hosted, "set_description", args); isError {
		t.Fatalf("set_description failed: %q", text)
	}
	if p, _ := reg.GetPeer(hosted); p.Description != "triaging flaky tests" {
		t.Fatalf("description = %q", p.Description)
	}

	for name, call := range map[string]struct {
		peer proto.PeerID
		tool string
	}{
		"local peer":     {local, "whoami"},
		"unknown peer":   {"repow-default-00000000", "whoami"},
		"lifecycle tool": {hosted, "kill_peer"},
	} {
		if text, isError := h.RunHostedTool(ctx, call.peer, call.tool, nil); !isError {
			t.Errorf("%s: %s ran (%q), want refusal", name, call.tool, text)
		}
	}
}
