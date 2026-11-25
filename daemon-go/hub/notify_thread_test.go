package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/state"
)

// #477: a notify to the human may name an ask it reports progress on. It
// lands in that ask's thread when the sender asked or holds the ask, stays a
// notify (never answering, acking or closing the ask), and goes unthreaded
// when the ask does not qualify. Only a notify to the human may carry one.
func TestNotifyProgressThreadsUnderTheHumansAsk(t *testing.T) {
	ctx := context.Background()
	s := newInboxStack(t)
	alpha := s.connect(t, map[string]any{"display_name": "alpha", "circle": "1", "backend": "claude-code", "role": "agent", "path": "/work/alpha"}, nil)
	beta := s.connect(t, map[string]any{"display_name": "beta", "circle": "1", "backend": "claude-code", "role": "agent", "path": "/work/beta"}, nil)

	asked := decodeBody(t, postJSON(t, s.srv.URL+"/ask", map[string]any{"from_peer": string(alpha.id), "to_peer": "prass", "text": "merge #12?"}))
	cid := asked["correlation_id"].(string)
	item := func(id string) *state.InboxItem {
		t.Helper()
		it, err := s.store.InboxItem(ctx, id)
		if err != nil || it == nil {
			t.Fatalf("inbox item %s: %v", id, err)
		}
		return it
	}
	notify := func(from string, to, thread string) (int, map[string]any) {
		t.Helper()
		resp := postJSON(t, s.srv.URL+"/notify", map[string]any{"from_peer": from, "to_peer": to, "text": "progress: CI is running", "correlation_id": thread})
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}

	// The asker's progress lands in the ask's thread, as a notify.
	code, res := notify(string(alpha.id), "prass", cid)
	if code != http.StatusOK || res["thread_id"] != cid {
		t.Fatalf("progress notify: %d %+v", code, res)
	}
	if it := item(res["inbox_item_id"].(string)); it.CorrelationID != cid || it.Kind != "notify" {
		t.Fatalf("item filed as kind %q thread %q", it.Kind, it.CorrelationID)
	}
	if ask, ok := s.asks.Get(cid); !ok || ask.Closed {
		t.Fatalf("the progress notify touched the ask: %+v", ask)
	}

	// A peer outside the ask, or an ask the tracker does not hold, is
	// delivered unthreaded.
	for name, try := range map[string][2]string{"outsider": {string(beta.id), cid}, "unknown ask": {string(alpha.id), "ask-nope"}} {
		code, res := notify(try[0], "prass", try[1])
		if code != http.StatusOK || res["thread_id"] != nil {
			t.Fatalf("%s: %d %+v", name, code, res)
		}
		if it := item(res["inbox_item_id"].(string)); it.CorrelationID != "" || it.Kind != "notify" {
			t.Fatalf("%s: item filed as kind %q thread %q", name, it.Kind, it.CorrelationID)
		}
	}

	// Only the human's inbox threads a notify: to an agent it is refused
	// before anything is recorded or sent.
	code, res = notify(string(alpha.id), string(beta.id), cid)
	if code != http.StatusUnprocessableEntity || res["reason"] != "thread_not_human" {
		t.Fatalf("thread to an agent: %d %+v", code, res)
	}
	if msgs, _ := s.store.ListPeerMessages(ctx, state.PeerMessageFilter{PeerID: string(beta.id)}); len(msgs) != 0 {
		t.Fatalf("a refused notify left %d records for beta", len(msgs))
	}

	// MCP notify_peer says where it landed; thread=none is an unthreaded one.
	got, err := s.h.mcpNotifyPeer(ctx, s.mcpDel, mcpNotifyPeerArgs{PeerName: "prass", Message: "half way", CorrelationID: cid}, string(alpha.id))
	if err != nil || !strings.HasSuffix(mcpText(t, got), " thread="+cid) {
		t.Fatalf("mcp progress: %v %v", got, err)
	}
	got, err = s.h.mcpNotifyPeer(ctx, s.mcpDel, mcpNotifyPeerArgs{PeerName: "prass", Message: "me too", CorrelationID: cid}, string(beta.id))
	if err != nil || !strings.HasSuffix(mcpText(t, got), " thread=none") {
		t.Fatalf("mcp outsider: %v %v", got, err)
	}
	if _, err := s.h.mcpNotifyPeer(ctx, s.mcpDel, mcpNotifyPeerArgs{PeerName: string(beta.id), Message: "x", CorrelationID: cid}, string(alpha.id)); err == nil ||
		!strings.Contains(err.Error(), "only to the human") {
		t.Fatalf("mcp thread to an agent: %v", err)
	}
	if ask, _ := s.asks.Get(cid); ask.Closed {
		t.Fatal("progress notifies closed the ask")
	}
}
