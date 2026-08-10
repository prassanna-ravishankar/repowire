package relay

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// wsURL converts an httptest http:// URL to the ws:// base the client dials.
func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

// fakeLocal is a stand-in for the local daemon HTTP surface the relay client
// forwards tunneled requests to.
func fakeLocal(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// startRelay runs a fake relay WS server. accept is invoked with the upgraded
// connection for each daemon connection; it returns when accept returns.
func startRelay(t *testing.T, onConnect func(ctx context.Context, c *websocket.Conn)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth is query-params (parity with the real relay).
		if r.URL.Query().Get("api_key") == "" || r.URL.Query().Get("daemon_id") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		conns.Add(1)
		onConnect(r.Context(), c)
	}))
	t.Cleanup(s.Close)
	return s, &conns
}

func TestClient_HTTPRequestTunnel(t *testing.T) {
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Proxy headers must have been stripped before reaching us.
		if r.Header.Get("X-Forwarded-For") != "" {
			t.Errorf("x-forwarded-for was not stripped")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	got := make(chan map[string]any, 1)
	relay, _ := startRelay(t, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, map[string]any{
			"type": "http_request", "request_id": "r1", "method": "GET", "path": "/health",
			"headers": map[string]any{"X-Forwarded-For": "1.2.3.4"},
		})
		var resp map[string]any
		if err := wsjson.Read(ctx, c, &resp); err != nil {
			return
		}
		got <- resp
	})

	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case resp := <-got:
		if resp["type"] != "http_response" || resp["request_id"] != "r1" {
			t.Fatalf("bad response frame: %v", resp)
		}
		if int(resp["status"].(float64)) != 200 {
			t.Fatalf("status = %v, want 200", resp["status"])
		}
		body, _ := base64.StdEncoding.DecodeString(resp["body"].(string))
		if string(body) != `{"ok":true}` {
			t.Fatalf("body = %q, want {\"ok\":true}", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for http_response")
	}
}

func TestClient_HTTPRequestAddsDaemonAuth(t *testing.T) {
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	got := make(chan map[string]any, 1)
	relay, _ := startRelay(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "http_request", "request_id": "auth-1", "path": "/peers"})
		var response map[string]any
		if wsjson.Read(ctx, conn, &response) == nil {
			got <- response
		}
	})
	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL).WithAuthToken("secret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()
	select {
	case response := <-got:
		if int(response["status"].(float64)) != http.StatusNoContent {
			t.Fatalf("status = %v", response["status"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for authenticated response")
	}
}

func TestClient_HTTPStreamAndCancel(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/stream" || r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"ready\"}\n\n"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		once.Do(func() { close(cancelled) })
	})
	gotChunk := make(chan string, 1)
	relay, _ := startRelay(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "http_stream_request", "request_id": "stream-1", "path": "/events/stream"})
		for {
			var frame map[string]any
			if wsjson.Read(ctx, conn, &frame) != nil {
				return
			}
			if frame["type"] == "http_stream_chunk" {
				body, _ := base64.StdEncoding.DecodeString(frame["body"].(string))
				gotChunk <- string(body)
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "http_stream_cancel", "request_id": "stream-1"})
				return
			}
		}
	})
	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL).WithAuthToken("secret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()
	select {
	case chunk := <-gotChunk:
		if !strings.Contains(chunk, "ready") {
			t.Fatalf("chunk = %q", chunk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream chunk")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("stream request was not cancelled")
	}
}

func TestClient_HTTPRequestTunnelBlocksMCP(t *testing.T) {
	var localCalls atomic.Int32
	local := fakeLocal(t, func(w http.ResponseWriter, _ *http.Request) {
		localCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	got := make(chan map[string]any, 1)
	relay, _ := startRelay(t, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, map[string]any{
			"type": "http_request", "request_id": "mcp-1", "method": "POST", "path": "/mcp",
		})
		var resp map[string]any
		if wsjson.Read(ctx, c, &resp) == nil {
			got <- resp
		}
	})
	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()
	select {
	case resp := <-got:
		if int(resp["status"].(float64)) != http.StatusNotFound {
			t.Fatalf("status = %v, want 404", resp["status"])
		}
		if localCalls.Load() != 0 {
			t.Fatal("blocked MCP request reached local daemon")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for blocked MCP response")
	}
}

func TestClient_RelayQueryForwarded(t *testing.T) {
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answer":42}`))
	})

	got := make(chan map[string]any, 1)
	relay, _ := startRelay(t, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, map[string]any{
			"type": "relay_query", "correlation_id": "cid-9", "source_daemon_id": "other",
			"payload": map[string]any{"q": "hi"},
		})
		var resp map[string]any
		if err := wsjson.Read(ctx, c, &resp); err != nil {
			return
		}
		got <- resp
	})

	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case resp := <-got:
		if resp["type"] != "relay_response" || resp["correlation_id"] != "cid-9" ||
			resp["source_daemon_id"] != "other" || resp["target_daemon_id"] != "other" {
			t.Fatalf("bad relay_response: %v", resp)
		}
		if int(resp["status"].(float64)) != 200 {
			t.Fatalf("status = %v, want 200", resp["status"])
		}
		body, _ := resp["body"].(map[string]any)
		if body["answer"].(float64) != 42 {
			t.Fatalf("body = %v, want answer=42", resp["body"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for relay_response")
	}
}

func TestClient_PingPong(t *testing.T) {
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {})
	got := make(chan map[string]any, 1)
	relay, _ := startRelay(t, func(ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, map[string]any{"type": "ping"})
		var resp map[string]any
		if err := wsjson.Read(ctx, c, &resp); err != nil {
			return
		}
		got <- resp
	})

	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	select {
	case resp := <-got:
		if resp["type"] != "pong" {
			t.Fatalf("expected pong, got %v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pong")
	}
}

func TestClient_Reconnects(t *testing.T) {
	local := fakeLocal(t, func(w http.ResponseWriter, r *http.Request) {})
	// Each connection closes immediately; the client must reconnect (backoff 1s).
	relay, conns := startRelay(t, func(ctx context.Context, c *websocket.Conn) {
		// return immediately → close → client sees EOF → reconnect
	})

	c := NewClient(wsURL(relay.URL), "rw_test", "d1", local.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()

	deadline := time.After(6 * time.Second)
	for {
		if conns.Load() >= 2 {
			return // reconnected at least once
		}
		select {
		case <-deadline:
			t.Fatalf("expected >=2 connections (reconnect), got %d", conns.Load())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestClient_StatusAndDisabledNoop(t *testing.T) {
	// Empty api key → Start is a no-op, never running.
	c := NewClient("ws://127.0.0.1:1", "", "d1", "http://127.0.0.1:1")
	c.Start(context.Background())
	if st := c.Status(); st.Running || st.Connected {
		t.Fatalf("disabled client should not run: %+v", st)
	}
	c.Stop() // must not hang
}
