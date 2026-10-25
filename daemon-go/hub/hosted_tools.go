package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
)

// hostedTools are the mesh tools a hosted agent may call. Replies to its asks
// arrive later as messages, so wait_on_ack is not needed; its final answer
// closes the ask it is working on, so ack and decline are left out; lifecycle
// tools stay with local peers until hosted spend has limits.
var hostedTools = map[string]bool{
	"whoami": true, "list_peers": true, "notify_peer": true, "ask": true, "set_description": true,
}

// RunHostedTool runs one mesh tool as a hosted peer, through the same MCP
// handlers local peers use. The gateway names the agent, and the gateway is
// authenticated by the daemon's own outbound connection, so the peer identity
// is taken as proven; it must still belong to a hosted peer.
func (h *Hub) RunHostedTool(ctx context.Context, peerID proto.PeerID, tool string, args json.RawMessage) (string, bool) {
	if p, _ := h.reg.GetPeerByName(string(peerID), nil); p == nil || p.PeerID != peerID || p.Backend != proto.AgentHosted {
		return fmt.Sprintf("%s is not a hosted peer on this daemon", peerID), true
	}
	if !hostedTools[tool] {
		return fmt.Sprintf("%s is not available to hosted agents", tool), true
	}
	if h.mcp == nil {
		return "mesh tools are not wired on this daemon", true
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Repowire-Peer", string(peerID))
	rec := httptest.NewRecorder()
	h.mcp.handler.ServeHTTP(rec, req)

	var envelope struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		return fmt.Sprintf("%s failed: HTTP %d %s", tool, rec.Code, strings.TrimSpace(rec.Body.String())), true
	}
	if envelope.Error != nil {
		return envelope.Error.Message, true
	}
	if envelope.Result == nil {
		return tool + " returned no result", true
	}
	texts := make([]string, 0, len(envelope.Result.Content))
	for _, c := range envelope.Result.Content {
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n"), envelope.Result.IsError
}
