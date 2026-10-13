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

	query := make(chan any, 1)
	go func() {
		var request map[string]any
		if wsjson.Read(context.Background(), conn, &request) != nil {
			return
		}
		query <- request["query_string"]
		id := request["request_id"]
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_start", "request_id": id, "status": 200, "headers": map[string]string{"Content-Type": "text/event-stream"}})
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_chunk", "request_id": id, "body": base64.StdEncoding.EncodeToString([]byte("data: {\"type\":\"peer_online\"}\n\n"))})
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_end", "request_id": id})
	}()

	resp, err := http.DefaultClient.Do(cookieRequest(t, http.MethodGet, server.URL+"/events/stream?surface=dashboard", key, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "peer_online") {
		t.Fatalf("stream body = %q", body)
	}
	// The owner's dashboard reaches the daemon as its inbox surface.
	if got := <-query; got != "surface=dashboard" {
		t.Fatalf("query_string = %v", got)
	}
}

func TestShareStreamNeverCarriesTheInbox(t *testing.T) {
	for _, tc := range []struct {
		event map[string]any
		want  bool
	}{
		{map[string]any{"type": "chat_turn", "peer": "worker"}, true},
		{map[string]any{"type": "inbox_item", "from": "worker", "item_id": "inb-1"}, false},
		{map[string]any{"type": "inbox_updated", "from": "worker", "item_id": "inb-1"}, false},
		{map[string]any{"type": "chat_turn", "peer": "other"}, false},
	} {
		if got := eventMentionsPeer(tc.event, "worker"); got != tc.want {
			t.Errorf("%v: shown=%v, want %v", tc.event, got, tc.want)
		}
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

func TestTokenStoreExpiryAndRegistrationIsolation(t *testing.T) {
	store := newTokenStore()
	first := store.register("user")
	if second := store.register("user"); second.Key == first.Key || second.UserID == first.UserID {
		t.Fatalf("public registration reused an identity: %q != %q", second.Key, first.Key)
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

// TestTunnelCarriesTheInbox (review of #478): the hosted dashboard reads and
// answers the inbox through the cookie tunnel, like every other daemon route.
func TestTunnelCarriesTheInbox(t *testing.T) {
	server := httptest.NewServer(New("").Handler())
	t.Cleanup(server.Close)
	key := "rw_test-inbox-key"
	conn := connectDaemon(t, server.URL, key)

	type seen struct{ method, path, query, body string }
	requests := make(chan seen, 3)
	go func() {
		for {
			var request map[string]any
			if wsjson.Read(context.Background(), conn, &request) != nil {
				return
			}
			body, _ := base64.StdEncoding.DecodeString(stringValue(request["body"]))
			requests <- seen{stringValue(request["method"]), stringValue(request["path"]), stringValue(request["query_string"]), string(body)}
			_ = wsjson.Write(context.Background(), conn, map[string]any{
				"type": "http_response", "request_id": request["request_id"], "status": 200,
				"headers": map[string]string{"Content-Type": "application/json"},
				"body":    base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)),
			})
		}
	}()

	for _, tc := range []struct{ method, url, body string }{
		{http.MethodGet, "/inbox?limit=100", ""},
		{http.MethodPost, "/inbox/inb-1/read", `{"surface":"dashboard"}`},
		{http.MethodPost, "/inbox/inb-1/reply", `{"surface":"dashboard","text":"yes"}`},
	} {
		resp, err := http.DefaultClient.Do(cookieRequest(t, tc.method, server.URL+tc.url, key, strings.NewReader(tc.body)))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d", tc.method, tc.url, resp.StatusCode)
		}
		got := <-requests
		path, query, _ := strings.Cut(tc.url, "?")
		if got.method != tc.method || got.path != path || got.query != query || got.body != tc.body {
			t.Fatalf("daemon saw %+v for %s %s", got, tc.method, tc.url)
		}
	}

	// Without the owner's cookie the tunnel sends the browser to sign in
	// rather than 404ing, and never reaches the daemon.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(server.URL + "/inbox")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || len(requests) != 0 {
		t.Fatalf("unauthenticated /inbox = %d, daemon requests %d", resp.StatusCode, len(requests))
	}
}

// TestShareStreamRouteDropsInboxEvents drives a share viewer's stream: the
// viewer's own query never reaches the daemon (no ?surface=dashboard
// spoofing), and inbox events are dropped even when split across chunks.
func TestShareStreamRouteDropsInboxEvents(t *testing.T) {
	server := httptest.NewServer(New("").Handler())
	t.Cleanup(server.Close)
	key := "rw_test-share-inbox-key"
	conn := connectDaemon(t, server.URL, key)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/share", strings.NewReader(`{"peer_name":"worker","permissions":"ro"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var share ShareToken
	_ = json.NewDecoder(resp.Body).Decode(&share)
	_ = resp.Body.Close()
	if share.ShareID == "" {
		t.Fatalf("share not created: %d", resp.StatusCode)
	}

	query := make(chan string, 1)
	go func() {
		var request map[string]any
		if wsjson.Read(context.Background(), conn, &request) != nil {
			return
		}
		query <- stringValue(request["query_string"])
		id := request["request_id"]
		chunk := func(s string) {
			_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_chunk", "request_id": id, "body": base64.StdEncoding.EncodeToString([]byte(s))})
		}
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_start", "request_id": id, "status": 200})
		chunk(`data: {"type":"inbox_item","from":"wor`)
		chunk(`ker","item_id":"inb-1"}` + "\n\n" + `data: {"type":"chat_turn","pe`)
		chunk(`er":"worker","text":"visible"}` + "\n\n" + `data: {"type":"inbox_updated","from":"worker"}` + "\n\n")
		_ = wsjson.Write(context.Background(), conn, map[string]any{"type": "http_stream_end", "request_id": id})
	}()

	stream, err := http.Get(server.URL + "/s/" + share.ShareID + "/stream?surface=dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	body, _ := io.ReadAll(stream.Body)
	select {
	case got := <-query:
		if got != "" {
			t.Fatalf("a share viewer's query reached the daemon: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the share stream never reached the daemon (status %d)", stream.StatusCode)
	}
	if !strings.Contains(string(body), "visible") || strings.Contains(string(body), "inbox_") {
		t.Fatalf("share stream body = %q", body)
	}
}
