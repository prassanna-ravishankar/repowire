package codexbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A Codex thread rename reaches the daemon as the peer's native name, and the
// bridge adopts the address the daemon answers with. Before registration it
// only rides the register body.
func TestThreadRenameForwardsTheNativeName(t *testing.T) {
	var mu sync.Mutex
	var updates []map[string]any
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		updates = append(updates, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "display_name": "fix-telegram-access"})
	}))
	defer daemon.Close()
	b := &Bridge{ctx: context.Background(), daemonHTTP: daemon.URL, threads: map[string]*threadPeer{}}

	unregistered := &threadPeer{bridge: b, id: "thread-0"}
	unregistered.rename("Early name")
	if unregistered.nativeName != "Early name" || len(updates) != 0 {
		t.Fatalf("an unregistered thread posted %v (name %q)", updates, unregistered.nativeName)
	}

	p := &threadPeer{bridge: b, id: "thread-1", peerID: "repow-1", nativeName: "Old"}
	b.threads["thread-1"] = p
	b.handleNotification("thread/name/updated", map[string]any{"threadId": "thread-1", "threadName": "Fix Telegram access"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(updates)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 1 || updates[0]["peer_name"] != "repow-1" || updates[0]["native_name"] != "Fix Telegram access" {
		t.Fatalf("updates = %v", updates)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.displayName != "fix-telegram-access" || p.nativeName != "Fix Telegram access" {
		t.Fatalf("thread peer = %q / %q", p.displayName, p.nativeName)
	}
}

func TestRegisterCarriesTheThreadName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var bodies []map[string]any
	daemon := paneCheckingDaemon(t, "", &bodies)
	stubTmuxPlacement(t, tmuxPlacement{})
	p := &threadPeer{bridge: &Bridge{daemonHTTP: daemon.URL}, id: "thread-1", cwd: t.TempDir(), circle: "mesh", role: "agent", nativeName: "Fix Telegram access"}
	if err := p.register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0]["native_name"] != "Fix Telegram access" {
		t.Fatalf("registrations = %#v", bodies)
	}
}
