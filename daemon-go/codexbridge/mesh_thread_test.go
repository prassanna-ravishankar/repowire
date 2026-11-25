package codexbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeThreadReads answers every thread/read with threads[id] and records the
// params of each read.
type fakeThreadReads struct {
	mu      sync.Mutex
	threads map[string]map[string]any
	reads   []map[string]any
}

func (f *fakeThreadReads) recorded() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reads...)
}

func startThreadReads(t *testing.T, ctx context.Context, threads map[string]map[string]any) (*Bridge, *fakeThreadReads) {
	t.Helper()
	f := &fakeThreadReads{threads: threads}
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		for {
			var request map[string]any
			if wsjson.Read(ctx, conn, &request) != nil {
				return
			}
			params, _ := request["params"].(map[string]any)
			f.mu.Lock()
			f.reads = append(f.reads, params)
			thread := f.threads[stringValue(params, "threadId")]
			f.mu.Unlock()
			_ = wsjson.Write(ctx, conn, map[string]any{"id": request["id"], "result": map[string]any{"thread": thread}})
		}
	}))
	t.Cleanup(appServer.Close)
	appConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{ctx: ctx, app: appConn, pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}}
	go b.readApp(appConn)
	return b, f
}

// The runtime's own helper threads (the TUI's thread_title namer, the
// desktop app's system helpers) never become peers, and their later status
// changes are not read again (#465, #475). Conversations still register.
func TestOnlyConversationThreadsBecomePeers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	title := loadThreadFixture(t, "thread_title_ephemeral.json")
	system := loadThreadFixture(t, "thread_system_ephemeral.json")
	user := loadThreadFixture(t, "thread_user.json")
	legacy := map[string]any{"id": "thread-legacy", "cwd": "/missing/legacy", "status": map[string]any{"type": "idle"}}
	b, reads := startThreadReads(t, ctx, map[string]map[string]any{})

	other := map[string]any{"id": "thread-helper", "cwd": "/missing/helper", "threadSource": "memory_consolidation", "status": map[string]any{"type": "idle"}}
	reads.mu.Lock()
	reads.threads["thread-helper"] = other
	reads.mu.Unlock()

	// thread/started, and discovery through a read (thread/loaded/list and
	// status rediscovery both reach loadThread).
	for _, thread := range []map[string]any{title, system} {
		b.handleNotification("thread/started", map[string]any{"thread": thread})
	}
	if err := b.loadThread(ctx, "thread-helper", false); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []map[string]any{title, system, other} {
		id := stringValue(thread, "id")
		if b.thread(id) != nil || !b.isInternal(id) {
			t.Fatalf("%s thread %s became a peer or was not remembered", thread["threadSource"], id)
		}
		// Later status changes and resumed-thread rediscovery read nothing.
		if err := b.loadThread(ctx, id, false); err != nil {
			t.Fatal(err)
		}
		if err := b.loadThread(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	if got := reads.recorded(); len(got) != 1 {
		t.Fatalf("reads = %v; want only the first discovery of thread-helper", got)
	}

	subagent := loadThreadFixture(t, "thread_subagent_denied.json")
	for _, thread := range []map[string]any{user, subagent, legacy} {
		b.handleNotification("thread/started", map[string]any{"thread": thread})
		if b.thread(stringValue(thread, "id")) == nil {
			t.Fatalf("conversation thread %v did not become a peer", thread["id"])
		}
	}

	// A closed helper is forgotten, so its id is not remembered forever.
	b.handleNotification("thread/closed", map[string]any{"threadId": stringValue(title, "id")})
	if b.isInternal(stringValue(title, "id")) {
		t.Fatal("closed helper thread is still remembered")
	}
}

// An ephemeral conversation keeps no turns on the App Server, so the
// completed-turn reconcile reads it without includeTurns instead of failing
// with -32600 (#475). A persisted thread still reads its turns.
func TestReconcileReadsTurnsOnlyForPersistedThreads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ephemeral := loadThreadFixture(t, "thread_subagent_ephemeral_accepting.json")
	persisted := loadThreadFixture(t, "thread_user.json")
	b, reads := startThreadReads(t, ctx, map[string]map[string]any{
		stringValue(ephemeral, "id"): ephemeral,
		stringValue(persisted, "id"): persisted,
	})
	b.addThread(ephemeral, false)
	b.addThread(persisted, false)

	b.captureCompletedTurn(stringValue(ephemeral, "id"))
	b.captureCompletedTurn(stringValue(persisted, "id"))
	// A thread closed before its reconcile runs is not read at all.
	b.closeThread(stringValue(ephemeral, "id"))
	b.captureCompletedTurn(stringValue(ephemeral, "id"))
	got := reads.recorded()
	if len(got) != 2 {
		t.Fatalf("reads = %v", got)
	}
	if got[0]["includeTurns"] != false || got[1]["includeTurns"] != true {
		t.Fatalf("includeTurns = %v, %v; want false for the ephemeral thread, true for the persisted one", got[0]["includeTurns"], got[1]["includeTurns"])
	}
}
