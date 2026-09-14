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
