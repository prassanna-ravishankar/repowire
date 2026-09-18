package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderPeersFitsTerminalAndPreservesTSV(t *testing.T) {
	result := map[string]any{"peers": []any{map[string]any{
		"peer_id": "repow-default-12345678", "display_name": "very-long-peer-name", "status": "online",
		"backend": "claude-code", "circle": "default", "path": "/a/very/long/project/path/that/would/overflow",
		"metadata": map[string]any{"project": "repowire"},
	}}}
	var narrow bytes.Buffer
	renderPeers(&narrow, result, 48)
	for _, line := range strings.Split(strings.TrimSuffix(narrow.String(), "\n"), "\n") {
		if len([]rune(line)) > 48 {
			t.Fatalf("line exceeds terminal width: %q", line)
		}
	}

	var piped bytes.Buffer
	renderPeers(&piped, result, 0)
	if !strings.HasPrefix(piped.String(), "peer_id\tname\tproject") {
		t.Fatalf("redirected output is not TSV: %q", piped.String())
	}
}

func TestRenderPeersMarksNonAddressablePeers(t *testing.T) {
	result := map[string]any{"peers": []any{map[string]any{
		"peer_id": "repow-1-child", "display_name": "app-2-codex", "status": "online", "backend": "codex", "circle": "1",
		"source": "codex-app-server", "initiator": "agent", "addressable": false, "parent_peer_id": "repow-1-parent",
		"metadata": map[string]any{"agent_nickname": "Pasteur"},
	}}}
	var piped bytes.Buffer
	renderPeers(&piped, result, 0)
	if !strings.HasSuffix(strings.TrimSpace(piped.String()), "codex-app-server\tagent\tfalse\trepow-1-parent") {
		t.Fatalf("TSV lacks provenance columns: %q", piped.String())
	}
	var wide bytes.Buffer
	renderPeers(&wide, result, 140)
	if !strings.Contains(wide.String(), "[no-input sub-agent Pasteur]") {
		t.Fatalf("interactive output does not flag the sub-agent: %q", wide.String())
	}
}
