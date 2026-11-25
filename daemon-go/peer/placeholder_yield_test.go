package peer

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// A by-hand resume whose MCP proxy registers the process by pid before the
// SessionStart hook, after the original agent exited (#474): the proxy knows no session, so it mints a
// placeholder. The hook's registration names the session, which is bound to
// the original peer: that peer takes the process back, and the placeholder is
// removed and closed for good, so its cached certificate cannot revive it.
func TestSessionBoundPeerTakesBackAProcessFromItsPlaceholder(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		h := newSessionHarness(t)
		orig, origName := h.register(proto.AgentClaudeCode, "/p/orch", "sess-1", 100)
		if _, err := h.r.MarkOffline(context.Background(), orig, terminal); err != nil {
			t.Fatal(err)
		}
		delete(h.live.alive, 100)

		placeholder, _ := h.register(proto.AgentClaudeCode, "/p/orch", "", 200) // the MCP proxy
		if !terminal {
			// A plain offline is reclaimed by the name tuple already: no
			// placeholder is minted.
			if placeholder != orig {
				t.Fatalf("after a plain offline the proxy minted %s instead of reclaiming %s", placeholder, orig)
			}
			continue
		}
		// After the agent exited (a terminal offline retires the peer), the
		// tuple reclaim skips it and the proxy mints a placeholder.
		if placeholder == orig {
			t.Fatal("the pid-only registration found the retired original; nothing to test")
		}
		got, gotName := h.register(proto.AgentClaudeCode, "/p/orch", "sess-1", 200) // SessionStart
		if got != orig || gotName != origName {
			t.Fatalf("terminal=%v: resume got %s/%s, want %s/%s", terminal, got, gotName, orig, origName)
		}
		if _, ok := h.r.GetPeer(placeholder); ok {
			t.Fatalf("terminal=%v: placeholder %s still registered", terminal, placeholder)
		}
		h.r.mu.Lock()
		closed := h.r.retired[placeholder].Hard
		h.r.mu.Unlock()
		if !closed {
			t.Fatalf("terminal=%v: placeholder not closed for good", terminal)
		}
		if h.status(orig) == proto.StatusOffline {
			t.Fatalf("terminal=%v: original still offline", terminal)
		}
		if n := len(eventsOfType(h.store, "runtime_placeholder_dissolved")); n != 1 {
			t.Fatalf("terminal=%v: %d dissolve events", terminal, n)
		}
		// The proxy's next registration, by pid alone, now lands on the original.
		if again, _ := h.register(proto.AgentClaudeCode, "/p/orch", "", 200); again != orig {
			t.Fatalf("terminal=%v: the proxy re-registered as %s, want %s", terminal, again, orig)
		}
	}
}

// A pid holder that already carries a session keeps its process: Claude keeps
// one peer across /clear, so a new session in the same process is not a
// placeholder yielding.
func TestAPidHolderWithASessionKeepsItsProcess(t *testing.T) {
	h := newSessionHarness(t)
	other, _ := h.register(proto.AgentClaudeCode, "/p/orch", "sess-old", 400)
	h.offline(other, 400)
	holder, _ := h.register(proto.AgentClaudeCode, "/p/orch", "sess-live", 300)
	if got, _ := h.register(proto.AgentClaudeCode, "/p/orch", "sess-old", 300); got != holder {
		t.Fatalf("the live holder lost its process to %s", got)
	}
	if _, ok := h.r.GetPeer(holder); !ok {
		t.Fatal("the live holder was removed")
	}
	if n := len(eventsOfType(h.store, "runtime_placeholder_dissolved")); n != 0 {
		t.Fatalf("%d dissolve events for a holder with a session", n)
	}
}
