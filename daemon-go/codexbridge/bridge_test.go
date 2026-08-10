package codexbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

func TestDeliveryUsesNativeThreadState(t *testing.T) {
	for _, test := range []struct{ name, activeTurn, want string }{
		{"active", "turn-1", "turn/steer"},
		{"idle", "", "turn/start"},
	} {
		t.Run(test.name, func(t *testing.T) { testDeliveryMethod(t, test.activeTurn, test.want) })
	}
}

func testDeliveryMethod(t *testing.T, activeTurn, want string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := make(chan string, 1)
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		var request map[string]any
		if wsjson.Read(ctx, conn, &request) == nil {
			method <- stringValue(request, "method")
			_ = wsjson.Write(ctx, conn, map[string]any{"id": request["id"], "result": map[string]any{"turnId": "turn-1"}})
		}
	}))
	defer appServer.Close()
	appConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{ctx: ctx, app: appConn, pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}}
	go b.readApp(appConn)

	meshConn := make(chan *websocket.Conn, 1)
	meshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			meshConn <- conn
		}
	}))
	defer meshServer.Close()
	meshClient, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(meshServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-meshConn
	defer serverConn.CloseNow()

	p := &threadPeer{bridge: b, id: "thread-1", activeTurn: activeTurn}
	p.inject(ctx, meshClient, map[string]any{"type": "ask", "delivery_id": "delivery-1", "text": "review this"})
	select {
	case got := <-method:
		if got != want {
			t.Fatalf("method = %q, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("App Server received no delivery")
	}
	var ack map[string]any
	if err := wsjson.Read(ctx, serverConn, &ack); err != nil {
		t.Fatal(err)
	}
	if stringValue(ack, "status") != "accepted" || stringValue(ack, "delivery_id") != "delivery-1" {
		t.Fatalf("ack = %#v", ack)
	}
}

func TestThreadHelpers(t *testing.T) {
	thread := map[string]any{"turns": []any{map[string]any{"id": "done", "status": "completed"}, map[string]any{"id": "live", "status": "inProgress"}}}
	if got := activeTurn(thread); got != "live" {
		t.Fatalf("activeTurn = %q", got)
	}
	if got := safeName("a project/name"); got != "a-project-name" {
		t.Fatalf("safeName = %q", got)
	}
}

func TestTmuxPlacementRequiresOneMatchingCodexPane(t *testing.T) {
	out := "/work/repo\tmesh\t@7\t%1\t100\n/work/other\tother\t@8\t%2\t200\n"
	processes := "100 1 zsh\n101 100 node /usr/local/bin/codex\n200 1 zsh\n"
	if got := parseTmuxPlacement(out, processes, "/work/repo", proto.CircleBoundarySession); got.circle != "mesh" || got.source != "tmux" || got.paneID != "%1" {
		t.Fatalf("placement = %+v", got)
	}
	out += "/work/repo\tother-mesh\t@9\t%3\t300\n"
	processes += "300 1 /opt/codex\n"
	if got := parseTmuxPlacement(out, processes, "/work/repo", proto.CircleBoundarySession); got.circle != "" {
		t.Fatalf("ambiguous placement should fail loud, got %+v", got)
	}
}

func TestNativeDeliveryPreservesProvenanceAndImage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	imagePath := home + "/.repowire/attachments/review.png"
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requestCh := make(chan map[string]any, 1)
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		var request map[string]any
		if wsjson.Read(ctx, conn, &request) == nil {
			requestCh <- request
			_ = wsjson.Write(ctx, conn, map[string]any{"id": request["id"], "result": map[string]any{}})
		}
	}))
	defer appServer.Close()
	appConn, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
	b := &Bridge{ctx: ctx, app: appConn, pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}}
	go b.readApp(appConn)
	p := &threadPeer{bridge: b, id: "thread-1"}
	p.inject(ctx, nil, map[string]any{
		"type": "ask", "from_peer": "reviewer", "to_peer": "owner", "correlation_id": "ask-1", "text": "review this\n↳ ack(\"ask-1\") or ack(\"ask-1\", \"reply\")",
		"attachments": []any{
			map[string]any{"path": imagePath, "filename": "review.png", "content_type": "image/png"},
			map[string]any{"path": "/etc/passwd", "filename": "unsafe.png", "content_type": "image/png"},
			map[string]any{"url": "https://example.test/image.png", "filename": "remote.png", "content_type": "image/png"},
		},
	})
	request := <-requestCh
	params, _ := request["params"].(map[string]any)
	input, _ := params["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("native input count = %d, want trusted text + one daemon-owned image", len(input))
	}
	textInput, _ := input[0].(map[string]any)
	text := stringValue(textInput, "text")
	if !strings.Contains(text, `<peer-message from="@reviewer" to="@owner" type="ask" correlation-id="ask-1">`) || !strings.Contains(text, `ack(&#34;ask-1&#34;)`) || strings.Count(text, "ack(") != 2 {
		t.Fatalf("delivery text = %q", text)
	}
	imageInput, _ := input[1].(map[string]any)
	if stringValue(imageInput, "type") != "localImage" || stringValue(imageInput, "path") != imagePath {
		t.Fatalf("image input = %#v", imageInput)
	}
}

func TestCompletedToolCallSummary(t *testing.T) {
	call := completedToolCall(map[string]any{"type": "mcpToolCall", "server": "repowire", "tool": "ack", "arguments": map[string]any{"correlation_id": "ask-1"}})
	if call["name"] != "repowire__ack" || !strings.Contains(call["input"], "ask-1") {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestTurnCompletedBackfillsItemsWithoutDuplicates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	posted := make(chan map[string]any, 3)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()
	b := &Bridge{ctx: context.Background(), threads: map[string]*threadPeer{}, daemonHTTP: daemon.URL}
	p := &threadPeer{
		bridge: b, id: "thread-1", cwd: cwd, peerID: "peer-1", displayName: "repo",
		toolCalls: map[string][]map[string]string{}, seenItems: map[string]map[string]bool{},
	}
	b.threads[p.id] = p
	user := map[string]any{"id": "user-1", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "check identity"}}}
	b.handleNotification("item/completed", map[string]any{"threadId": p.id, "turnId": "turn-1", "item": user})
	b.handleNotification("turn/completed", map[string]any{"threadId": p.id, "turn": map[string]any{
		"id": "turn-1", "items": []any{
			user,
			map[string]any{"id": "tool-1", "type": "mcpToolCall", "server": "repowire", "tool": "whoami", "arguments": map[string]any{}},
			map[string]any{"id": "agent-1", "type": "agentMessage", "text": "peer-1"},
		},
	}})
	first, second := <-posted, <-posted
	if first["role"] != "user" || second["role"] != "assistant" {
		t.Fatalf("posted roles = %v, %v", first["role"], second["role"])
	}
	calls, _ := second["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool calls = %#v", second["tool_calls"])
	}
	select {
	case duplicate := <-posted:
		t.Fatalf("duplicate chat item: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
	if matches, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".cache", "repowire", "handoffs", "*.json")); len(matches) != 1 {
		t.Fatalf("handoff files = %v", matches)
	}
}

func TestConfiguredProviderEnvKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	config := "model_provider = \"azure\"\n[model_providers.azure]\nenv_key = \"AZURE_TEST_KEY\"\n[model_providers.other]\nenv_key='OTHER_KEY'\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(configuredProviderEnvKeys(), ","); got != "AZURE_TEST_KEY,OTHER_KEY" {
		t.Fatalf("provider env keys = %q", got)
	}
	t.Setenv("AZURE_TEST_KEY", "present")
	t.Setenv("OTHER_KEY", "present-too")
	env := codexChildEnvironment(context.Background())
	if !containsEnv(env, "AZURE_TEST_KEY=present") {
		t.Fatal("existing provider key was not preserved")
	}
}

func containsEnv(env []string, value string) bool {
	for _, item := range env {
		if item == value {
			return true
		}
	}
	return false
}

func TestMeshContextUsesHistoryInjectionOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan map[string]any, 2)
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		for {
			var request map[string]any
			if wsjson.Read(ctx, conn, &request) != nil {
				return
			}
			requests <- request
			_ = wsjson.Write(ctx, conn, map[string]any{"id": request["id"], "result": map[string]any{}})
		}
	}))
	defer appServer.Close()
	appConn, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
	peerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"peers": []any{map[string]any{
			"peer_id": "repow-native", "display_name": "repo", "circle": "mesh", "backend": "codex", "role": "agent", "path": "/work/repo", "status": "online",
		}}})
	}))
	defer peerServer.Close()
	b := &Bridge{ctx: ctx, app: appConn, pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}, daemonHTTP: peerServer.URL}
	go b.readApp(appConn)
	p := &threadPeer{bridge: b, id: "thread-context", cwd: "/work/repo", circle: "mesh", circleSrc: "fallback", role: "agent", peerID: "repow-native", displayName: "repo"}
	if err := p.ensureContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.ensureContext(ctx); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if stringValue(request, "method") != "thread/inject_items" {
		t.Fatalf("method = %q", stringValue(request, "method"))
	}
	params, _ := request["params"].(map[string]any)
	items, _ := params["items"].([]any)
	item, _ := items[0].(map[string]any)
	if stringValue(item, "role") != "developer" {
		t.Fatalf("injected item = %#v", item)
	}
	select {
	case duplicate := <-requests:
		t.Fatalf("duplicate context injection: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAppServerResponseOverDefaultWebSocketLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		var request map[string]any
		if wsjson.Read(ctx, conn, &request) == nil {
			_ = wsjson.Write(ctx, conn, map[string]any{
				"id": request["id"], "result": map[string]any{"payload": strings.Repeat("x", 64<<10)},
			})
		}
	}))
	defer appServer.Close()
	appConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer appConn.CloseNow()
	b := &Bridge{ctx: ctx, app: appConn, pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}}
	go b.readApp(appConn)
	result, err := b.call(ctx, "thread/read", map[string]any{"threadId": "large"})
	if err != nil || len(result) < 64<<10 {
		t.Fatalf("large App Server response: bytes=%d err=%v", len(result), err)
	}
}

func TestAppServerReadFailureReconnectsWithoutStoppingOwnedServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connections := make(chan int32, 2)
	var accepted atomic.Int32
	appServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		n := accepted.Add(1)
		connections <- n
		for {
			var request map[string]any
			if wsjson.Read(ctx, conn, &request) != nil {
				return
			}
			id, hasID := request["id"]
			if !hasID {
				continue
			}
			result := map[string]any{}
			if stringValue(request, "method") == "thread/loaded/list" {
				result["data"] = []string{}
			}
			_ = wsjson.Write(ctx, conn, map[string]any{"id": id, "result": result})
			if n == 1 && stringValue(request, "method") == "thread/loaded/list" {
				_ = conn.Close(websocket.StatusInternalError, "test bridge failure")
				return
			}
		}
	}))
	defer appServer.Close()

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	var connects atomic.Int32
	connect := func(ctx context.Context) (*websocket.Conn, *exec.Cmd, error) {
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(appServer.URL, "http"), nil)
		if connects.Add(1) == 1 {
			return conn, child, err
		}
		return conn, nil, err
	}
	b := &Bridge{ctx: ctx, version: "test", pending: map[int64]chan rpcReply{}, threads: map[string]*threadPeer{}}
	done := make(chan error, 1)
	go func() { done <- b.runAppServer(connect) }()
	for range 2 {
		select {
		case <-connections:
		case <-time.After(2 * time.Second):
			t.Fatal("bridge did not reconnect")
		}
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("bridge failure stopped owned App Server: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not stop")
	}
	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("intentional shutdown did not stop owned App Server")
	}
}
