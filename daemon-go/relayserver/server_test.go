package relayserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func relayWebSocketURL(base, key, daemonID string) string {
	return "ws" + strings.TrimPrefix(base, "http") + "/ws/relay?api_key=" + key + "&daemon_id=" + daemonID
}

func connectDaemon(t *testing.T, serverURL, key string) *websocket.Conn {
	return connectDaemonAs(t, serverURL, key, "test-daemon")
}

func connectDaemonAs(t *testing.T, serverURL, key, daemonID string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, relayWebSocketURL(serverURL, key, daemonID), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestCrossDaemonRoundTrip(t *testing.T) {
	server := httptest.NewServer(New("").Handler())
	t.Cleanup(server.Close)
	key := "rw_test-cross-daemon-key"
	first := connectDaemonAs(t, server.URL, key, "first")
	second := connectDaemonAs(t, server.URL, key, "second")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := wsjson.Write(ctx, first, map[string]any{
		"type": "relay_query", "target_daemon_id": "second", "correlation_id": "cid-1",
	}); err != nil {
		t.Fatal(err)
	}
	var query map[string]any
	if err := wsjson.Read(ctx, second, &query); err != nil {
		t.Fatal(err)
	}
	if query["source_daemon_id"] != "first" {
		t.Fatalf("forwarded query = %v", query)
	}
	if err := wsjson.Write(ctx, second, map[string]any{
		"type": "relay_response", "target_daemon_id": "first", "correlation_id": "cid-1", "status": 200,
	}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := wsjson.Read(ctx, first, &response); err != nil {
		t.Fatal(err)
	}
	if response["source_daemon_id"] != "second" || response["correlation_id"] != "cid-1" {
		t.Fatalf("forwarded response = %v", response)
	}
}

func cookieRequest(t *testing.T, method, url, key string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "rw_token", Value: key})
	return req
}

func TestTunnelRoundTrip(t *testing.T) {
	server := httptest.NewServer(New("").Handler())
	t.Cleanup(server.Close)
	key := "rw_test-connector-key"
	conn := connectDaemon(t, server.URL, key)

	done := make(chan struct{})
	go func() {
		defer close(done)
		var request map[string]any
		if wsjson.Read(context.Background(), conn, &request) != nil {
			return
		}
		if request["type"] != "http_request" || request["path"] != "/peers" {
			t.Errorf("unexpected tunnel request: %v", request)
			return
		}
		_ = wsjson.Write(context.Background(), conn, map[string]any{
			"type": "http_response", "request_id": request["request_id"], "status": 200,
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    base64.StdEncoding.EncodeToString([]byte(`[{"display_name":"worker"}]`)),
		})
	}()

	resp, err := http.DefaultClient.Do(cookieRequest(t, http.MethodGet, server.URL+"/peers", key, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "worker") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	<-done
}

func TestEventStreamUsesStreamFrames(t *testing.T) {
	server := httptest.NewServer(New("").Handler())
	t.Cleanup(server.Close)
	key := "rw_test-stream-key"
	conn := connectDaemon(t, server.URL, key)

	go func() {
		var request map[string]any
		if wsjson.Read(context.Background(), conn, &request) != nil {
			return
		}
		id := request["request_id"]
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_start", "request_id": id, "status": 200, "headers": map[string]string{"Content-Type": "text/event-stream"}})
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_chunk", "request_id": id, "body": base64.StdEncoding.EncodeToString([]byte("data: {\"type\":\"peer_online\"}\n\n"))})
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_end", "request_id": id})
	}()

	resp, err := http.DefaultClient.Do(cookieRequest(t, http.MethodGet, server.URL+"/events/stream", key, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "peer_online") {
		t.Fatalf("stream body = %q", body)
	}
}

func TestShareLifecycleAndViewer(t *testing.T) {
	server := httptest.NewServer(New("").Handler())
	t.Cleanup(server.Close)
	key := "rw_test-share-key"
	_ = connectDaemon(t, server.URL, key)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/share", strings.NewReader(`{"peer_name":"worker","permissions":"rw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var share ShareToken
	if err := json.NewDecoder(resp.Body).Decode(&share); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || share.ShareID == "" {
		t.Fatalf("status=%d share=%+v", resp.StatusCode, share)
	}

	view, err := http.Get(server.URL + "/s/" + share.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Body.Close()
	html, _ := io.ReadAll(view.Body)
	page := string(html)
	if !strings.Contains(page, "@worker") || strings.Contains(page, "{{SHARE}}") || !strings.Contains(page, "/s/"+share.ShareID+"/ask") {
		t.Fatalf("bad viewer page: %s", page)
	}

	revoke, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/share/"+share.ShareID, nil)
	revoke.Header.Set("X-API-Key", key)
	revoked, err := http.DefaultClient.Do(revoke)
	if err != nil {
		t.Fatal(err)
	}
	_ = revoked.Body.Close()
	missing, err := http.Get(server.URL + "/s/" + share.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked share status=%d", missing.StatusCode)
	}
}

func TestTokenStoreExpiryAndStableRegistration(t *testing.T) {
	store := newTokenStore()
	first := store.register("user")
	if second := store.register("user"); second.Key != first.Key {
		t.Fatalf("registration was not stable: %q != %q", second.Key, first.Key)
	}
	share := store.createShare("user", "worker", "ro", time.Nanosecond)
	time.Sleep(time.Millisecond)
	if _, ok := store.share(share.ShareID); ok {
		t.Fatal("expired share remained valid")
	}
}

func TestTunnelPathCoversDashboardWithoutExposingMCP(t *testing.T) {
	for _, path := range []string{
		"/answer", "/circles/default/orchestrator", "/panes/orphans", "/peers/worker/timeline",
		"/reviews", "/schedules", "/sessions/rw-1/controls/resume", "/spawn/config", "/traces/trace-1", "/work",
	} {
		if !isTunnelPath(path) {
			t.Errorf("dashboard path %q is not tunneled", path)
		}
	}
	for _, path := range []string{"/mcp", "/mcp/tools", "/not-a-daemon-route"} {
		if isTunnelPath(path) {
			t.Errorf("path %q must not be tunneled", path)
		}
	}
}
