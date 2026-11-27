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

// recordingDaemon answers /peers and /session/update, recording each request
// in arrival order; gate, when set, holds the first request of path until it
// is closed.
type recordingDaemon struct {
	mu       sync.Mutex
	requests []map[string]any
	gatePath string
	gate     chan struct{}
	gated    bool
	reached  chan struct{}
}

func (d *recordingDaemon) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		hold := d.gate != nil && !d.gated && r.URL.Path == d.gatePath
		if hold {
			d.gated = true
		}
		d.mu.Unlock()
		if hold {
			close(d.reached)
			<-d.gate
		}
		d.mu.Lock()
		body["_path"] = r.URL.Path
		d.requests = append(d.requests, body)
		d.mu.Unlock()
		if r.URL.Path == "/peers" {
			_ = json.NewEncoder(w).Encode(map[string]any{"peer_id": "repow-1", "display_name": "registered"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "display_name": body["native_name"]})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *recordingDaemon) lastName(path string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.requests) - 1; i >= 0; i-- {
		if d.requests[i]["_path"] == path {
			name, _ := d.requests[i]["native_name"].(string)
			return name
		}
	}
	return ""
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Two renames in quick succession land in notification order: the slower
// first publication cannot overwrite the later name.
func TestThreadRenamesLandInOrder(t *testing.T) {
	d := &recordingDaemon{gatePath: "/session/update", gate: make(chan struct{}), reached: make(chan struct{})}
	srv := d.serve(t)
	b := &Bridge{ctx: context.Background(), daemonHTTP: srv.URL, threads: map[string]*threadPeer{}}
	p := &threadPeer{bridge: b, id: "thread-1", peerID: "repow-1"}
	b.threads["thread-1"] = p

	b.handleNotification("thread/name/updated", map[string]any{"threadId": "thread-1", "threadName": "A"})
	<-d.reached // A's publication is in flight
	b.handleNotification("thread/name/updated", map[string]any{"threadId": "thread-1", "threadName": "B"})
	close(d.gate)
	waitUntil(t, "B published", func() bool { return d.lastName("/session/update") == "B" })
	waitUntil(t, "publisher idle", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !p.naming
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.publishedName != "B" || p.displayName != "B" {
		t.Fatalf("published %q, display %q; want B", p.publishedName, p.displayName)
	}
}

// A rename while the thread registers is published once registration returns,
// instead of being lost behind the name the register body carried.
func TestRenameDuringRegistrationIsPublished(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubTmuxPlacement(t, tmuxPlacement{})
	d := &recordingDaemon{gatePath: "/peers", gate: make(chan struct{}), reached: make(chan struct{})}
	srv := d.serve(t)
	p := &threadPeer{bridge: &Bridge{ctx: context.Background(), daemonHTTP: srv.URL}, id: "thread-1", cwd: t.TempDir(), circle: "mesh", role: "agent", nativeName: "A"}

	done := make(chan error, 1)
	go func() { done <- p.register(context.Background()) }()
	<-d.reached // registration carrying A is in flight
	p.rename("B")
	close(d.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := d.lastName("/peers"); got != "A" {
		t.Fatalf("register carried %q, want A", got)
	}
	waitUntil(t, "B published after registration", func() bool { return d.lastName("/session/update") == "B" })
}
