package relayserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	managedKey   = "rw_managed-user-secret"
	otherKey     = "rw_other-user-secret"
	strangerKey  = "rw_minted-but-not-enabled"
	controlToken = "control-secret"
)

func userID(key string) string {
	k, _ := (&tokenStore{}).validate(key)
	return k.UserID
}

// fakeWorker records what the relay sent and serves canned control API replies.
type fakeWorker struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	dials    atomic.Int32
	sockets  chan *websocket.Conn
	closed   chan struct{}
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	f := &fakeWorker{sockets: make(chan *websocket.Conn, 4), closed: make(chan struct{}, 4)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+controlToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/events":
			f.dials.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			// Like the Worker, every events socket opens with its own snapshot.
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"snapshot","agents":[],"n":`+strconv.Itoa(int(f.dials.Load()))+`}`))
			f.sockets <- conn
			_, _, _ = conn.Read(context.Background())
			f.closed <- struct{}{}
		case "/agents/retired-one/messages":
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"error":"agent is retired"}`)
		case "/clients":
			_, _ = io.WriteString(w, `{"items":[{"kind":"daemon","machine":"laptop","daemon_id":"d1","version":"1","connected_at":"2026-10-25T00:00:00Z","connected":true}],"outbox":2}`)
		case "/admin/summary":
			_, _ = io.WriteString(w, `{"owners":1,"agents_by_state":{"idle":1}}`)
		case "/admin/usage":
			_, _ = io.WriteString(w, `{"items":[{"day":"`+time.Now().UTC().Format(time.DateOnly)+`","owner":"a","model_usd":0.5,"container_usd":0.25},{"day":"`+time.Now().UTC().Format(time.DateOnly)+`","owner":"b","model_usd":0.25,"container_usd":0}]}`)
		default:
			_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWorker) last() (*http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1], f.bodies[len(f.bodies)-1]
}

type managedFixture struct {
	relay  *Server
	http   *httptest.Server
	worker *fakeWorker
}

func newManagedFixture(t *testing.T, configured bool) *managedFixture {
	t.Helper()
	s := New(t.TempDir())
	f := &managedFixture{relay: s}
	cfg := ManagedConfig{AdminUsers: []string{userID(managedKey)}, ManagedUsers: []string{userID(otherKey)}}
	if configured {
		f.worker = newFakeWorker(t)
		cfg.URL, cfg.ControlToken = f.worker.URL, controlToken
	}
	if err := s.ConfigureManaged(cfg); err != nil {
		t.Fatal(err)
	}
	f.http = httptest.NewServer(s.Handler())
	t.Cleanup(f.http.Close)
	return f
}

func (f *managedFixture) do(t *testing.T, method, path, key, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req := cookieRequest(t, method, f.http.URL+path, key, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

func TestManagedConfigValidation(t *testing.T) {
	if err := New("").ConfigureManaged(ManagedConfig{URL: "https://worker.example"}); err == nil {
		t.Fatal("URL without control token accepted")
	}
	if err := New("").ConfigureManaged(ManagedConfig{URL: "ftp://worker.example", ControlToken: "x"}); err == nil {
		t.Fatal("non-http URL accepted")
	}
	t.Setenv("REPOWIRE_ADMIN_USERS", " token-a, ,token-b ")
	t.Setenv("REPOWIRE_MANAGED_USERS", "token-c")
	cfg := ManagedConfigFromEnv()
	if len(cfg.AdminUsers) != 2 || cfg.AdminUsers[1] != "token-b" || len(cfg.ManagedUsers) != 1 || cfg.ManagedUsers[0] != "token-c" {
		t.Fatalf("users = %v %v", cfg.AdminUsers, cfg.ManagedUsers)
	}
}

func TestManagedMeAndUnconfigured(t *testing.T) {
	f := newManagedFixture(t, false)
	if res, _ := f.do(t, "GET", "/api/v1/managed/me", "", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous me = %d", res.StatusCode)
	}
	res, me := f.do(t, "GET", "/api/v1/managed/me", managedKey, "", nil)
	if res.StatusCode != 200 || me["user_id"] != userID(managedKey) || me["admin"] != true || me["managed"] != false {
		t.Fatalf("me = %d %v", res.StatusCode, me)
	}
	res, body := f.do(t, "GET", "/api/v1/managed/agents", managedKey, "", nil)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body["error"].(string), "REPOWIRE_MANAGED_URL") {
		t.Fatalf("unconfigured agents = %d %v", res.StatusCode, body)
	}
	res, body = f.do(t, "GET", "/api/v1/managed/clients", managedKey, "", nil)
	if res.StatusCode != 200 || body["gateway_error"] == nil {
		t.Fatalf("unconfigured clients = %d %v", res.StatusCode, body)
	}
}

func TestManagedProxyNamesOwnerAndPassesStatus(t *testing.T) {
	f := newManagedFixture(t, true)
	res, body := f.do(t, "POST", "/api/v1/managed/agents/a1/messages?x=1", managedKey, `{"operationId":"web-1","text":"hi"}`, nil)
	if res.StatusCode != 200 || body["path"] != "/agents/a1/messages" {
		t.Fatalf("proxy = %d %v", res.StatusCode, body)
	}
	req, sent := f.worker.last()
	if req.Method != "POST" || req.Header.Get("X-Repowire-Owner") != userID(managedKey) || req.URL.RawQuery != "x=1" || sent != `{"operationId":"web-1","text":"hi"}` {
		t.Fatalf("worker saw %s %s owner=%q body=%s", req.Method, req.URL, req.Header.Get("X-Repowire-Owner"), sent)
	}

	// Bearer relay keys work, and Worker errors pass through.
	res, body = f.do(t, "POST", "/api/v1/managed/agents/retired-one/messages", "", `{}`, map[string]string{"Authorization": "Bearer " + otherKey})
	if res.StatusCode != http.StatusGone || body["error"] != "agent is retired" {
		t.Fatalf("retired = %d %v", res.StatusCode, body)
	}
	if req, _ := f.worker.last(); req.Header.Get("X-Repowire-Owner") != userID(otherKey) {
		t.Fatalf("bearer owner = %q", req.Header.Get("X-Repowire-Owner"))
	}

	res, _ = f.do(t, "DELETE", "/api/v1/managed/agents/a1", managedKey, "", map[string]string{"Origin": "https://evil.example"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin cookie delete = %d", res.StatusCode)
	}
	res, _ = f.do(t, "DELETE", "/api/v1/managed/agents/a1", managedKey, "", map[string]string{"Origin": f.http.URL})
	if res.StatusCode != 200 {
		t.Fatalf("same-origin delete = %d", res.StatusCode)
	}
}

func TestManagedAdminRoutes(t *testing.T) {
	f := newManagedFixture(t, true)
	f.relay.registerConn(&daemonConn{userID: userID(otherKey), daemonID: "box", connectedAt: time.Now()})
	if res, _ := f.do(t, "GET", "/api/v1/admin/summary", otherKey, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin summary = %d", res.StatusCode)
	}
	res, summary := f.do(t, "GET", "/api/v1/admin/summary", managedKey, "", nil)
	relay, _ := summary["relay"].(map[string]any)
	if res.StatusCode != 200 || summary["owners"] != float64(1) || relay["daemons_connected"] != float64(1) || relay["users_with_daemons"] != float64(1) {
		t.Fatalf("summary = %d %v", res.StatusCode, summary)
	}
	if req, _ := f.worker.last(); req.Header.Get("X-Repowire-Owner") != "" {
		t.Fatal("admin route sent an owner")
	}
	res, body := f.do(t, "GET", "/api/v1/admin/sandboxes", managedKey, "", nil)
	if res.StatusCode != 200 || body["path"] != "/admin/sandboxes" {
		t.Fatalf("sandboxes = %d %v", res.StatusCode, body)
	}

	res, costs := f.do(t, "GET", "/api/v1/admin/costs", managedKey, "", nil)
	estimate, _ := costs["estimate"].(map[string]any)
	days, _ := estimate["days"].([]any)
	if res.StatusCode != 200 || costs["actual"] != nil || len(days) != 1 || math.Abs(estimate["month_usd"].(float64)-1.0) > 1e-9 {
		t.Fatalf("costs = %d %v", res.StatusCode, costs)
	}
	if req, _ := f.worker.last(); req.URL.Query().Get("days") == "" {
		t.Fatal("costs did not request usage days")
	}
}

func TestManagedClientsAndAppRevoke(t *testing.T) {
	f := newManagedFixture(t, true)
	if err := f.relay.EnableMCP("http://127.0.0.1", filepath.Join(t.TempDir(), "oauth.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.relay.Close() })
	owner := userID(managedKey)
	expires := time.Now().Add(time.Hour)
	if err := f.relay.oauth.store.update(func(d *oauthData) error {
		d.Clients.put("c1", storedOAuthClient{oauthClient: oauthClient{ID: "c1", Name: "Claude"}, Expires: expires})
		d.Grants.put("g1", oauthGrant{ID: "g1", UserID: owner, ClientID: "c1", Scope: oauthRead, Expires: expires, Active: true})
		d.Grants.put("g2", oauthGrant{ID: "g2", UserID: owner, ClientID: "c1", Expires: expires, Revoked: true})
		d.Grants.put("g3", oauthGrant{ID: "g3", UserID: userID(otherKey), ClientID: "c1", Expires: expires, Active: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.relay.registerConn(&daemonConn{userID: owner, daemonID: "laptop", connectedAt: time.Now()})

	res, clients := f.do(t, "GET", "/api/v1/managed/clients", managedKey, "", nil)
	daemons, _ := clients["daemons"].([]any)
	gateway, _ := clients["gateway"].([]any)
	apps, _ := clients["apps"].([]any)
	if res.StatusCode != 200 || len(daemons) != 1 || len(gateway) != 1 || clients["outbox"] != float64(2) || len(apps) != 1 {
		t.Fatalf("clients = %d %v", res.StatusCode, clients)
	}
	if app := apps[0].(map[string]any); app["grant_id"] != "g1" || app["client_name"] != "Claude" || app["active"] != true {
		t.Fatalf("app = %v", app)
	}
	if relay := f.relay.relayCounts(); relay["oauth_grants_active"] != 2 {
		t.Fatalf("active grants = %v", relay)
	}

	if res, _ := f.do(t, "DELETE", "/api/v1/managed/apps/g3", managedKey, "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoking another user's grant = %d", res.StatusCode)
	}
	if res, _ := f.do(t, "DELETE", "/api/v1/managed/apps/g1", managedKey, "", nil); res.StatusCode != 200 {
		t.Fatalf("revoke = %d", res.StatusCode)
	}
	if _, clients = f.do(t, "GET", "/api/v1/managed/clients", managedKey, "", nil); len(clients["apps"].([]any)) != 0 {
		t.Fatalf("revoked grant still listed: %v", clients["apps"])
	}
}

// sseReader yields SSE frames (blank-line separated) from a live response.
func sseReader(t *testing.T, body io.Reader) <-chan string {
	t.Helper()
	frames := make(chan string, 16)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(body)
		var frame strings.Builder
		for scanner.Scan() {
			if scanner.Text() == "" {
				frames <- frame.String()
				frame.Reset()
				continue
			}
			frame.WriteString(scanner.Text() + "\n")
		}
	}()
	return frames
}

func nextFrame(t *testing.T, frames <-chan string, contains string) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("stream ended waiting for %q", contains)
			}
			if strings.Contains(frame, contains) {
				return frame
			}
		case <-timeout:
			t.Fatalf("no frame containing %q", contains)
		}
	}
}

func openEvents(t *testing.T, f *managedFixture, key string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := cookieRequest(t, "GET", f.http.URL+"/api/v1/managed/events", key, nil)
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	release := func() { cancel(); _ = res.Body.Close() }
	t.Cleanup(release)
	return sseReader(t, res.Body), release
}

func TestManagedEventsEachSubscriberGetsSnapshot(t *testing.T) {
	f := newManagedFixture(t, true)
	if f.worker.dials.Load() != 0 {
		t.Fatal("upstream opened without a subscriber")
	}
	upstream := func() *websocket.Conn {
		t.Helper()
		select {
		case conn := <-f.worker.sockets:
			return conn
		case <-time.After(5 * time.Second):
			t.Fatal("worker events socket not opened")
			return nil
		}
	}

	first, closeFirst := openEvents(t, f, managedKey)
	nextFrame(t, first, `"type":"daemons"`)
	if frame := nextFrame(t, first, `"type":"gateway"`); !strings.Contains(frame, `"connected":true`) {
		t.Fatalf("gateway frame = %q", frame)
	}
	if frame := nextFrame(t, first, `"type":"snapshot"`); frame != "data: {\"type\":\"snapshot\",\"agents\":[],\"n\":1}\n" {
		t.Fatalf("first snapshot = %q", frame)
	}
	firstUp := upstream()
	req, _ := f.worker.last()
	if req.Header.Get("X-Repowire-Owner") != userID(managedKey) {
		t.Fatalf("events owner = %q", req.Header.Get("X-Repowire-Owner"))
	}

	// A second tab, opened while the first stays connected, gets its own snapshot.
	second, closeSecond := openEvents(t, f, managedKey)
	nextFrame(t, second, `"type":"daemons"`)
	if frame := nextFrame(t, second, `"type":"snapshot"`); !strings.Contains(frame, `"n":2`) {
		t.Fatalf("second snapshot = %q", frame)
	}
	secondUp := upstream()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := firstUp.Write(ctx, websocket.MessageText, []byte("{\n \"type\": \"agent\", \"agent\": {\"id\": \"a1\"}\n}")); err != nil {
		t.Fatal(err)
	}
	if frame := nextFrame(t, first, `"type":"agent"`); frame != "data: {\"type\":\"agent\",\"agent\":{\"id\":\"a1\"}}\n" {
		t.Fatalf("agent frame = %q", frame)
	}
	if err := secondUp.Write(ctx, websocket.MessageText, []byte(`{"type":"outbox","depth":3}`)); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, second, `"type":"outbox"`)

	f.relay.registerConn(&daemonConn{userID: userID(managedKey), daemonID: "laptop", connectedAt: time.Now()})
	for _, frames := range []<-chan string{first, second} {
		if frame := nextFrame(t, frames, `"type":"daemon"`); !strings.HasPrefix(frame, "event: relay\n") || !strings.Contains(frame, `"connected":true`) {
			t.Fatalf("daemon frame = %q", frame)
		}
	}

	closeFirst()
	closeSecond()
	for range 2 {
		select {
		case <-f.worker.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream not closed after its subscriber left")
		}
	}
	f.relay.events.mu.Lock()
	defer f.relay.events.mu.Unlock()
	if len(f.relay.events.owners) != 0 {
		t.Fatalf("subscribers left registered: %v", f.relay.events.owners)
	}
}

func TestManagedEventsReportUpstreamFailure(t *testing.T) {
	f := newManagedFixture(t, true)
	f.relay.events.dial = func(context.Context, string) (*websocket.Conn, error) {
		return nil, io.ErrUnexpectedEOF
	}
	frames, closeEvents := openEvents(t, f, managedKey)
	defer closeEvents()
	if frame := nextFrame(t, frames, `"type":"gateway"`); !strings.Contains(frame, `"connected":false`) {
		t.Fatalf("gateway frame = %q", frame)
	}
}

func TestManagedEventsStalledReaderReleasesUpstream(t *testing.T) {
	previous := managedWriteTimeout
	managedWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { managedWriteTimeout = previous })
	f := newManagedFixture(t, true)

	// A client that sends the request and then never reads.
	conn, err := net.Dial("tcp", strings.TrimPrefix(f.http.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = io.WriteString(conn, "GET /api/v1/managed/events HTTP/1.1\r\nHost: relay\r\nCookie: rw_token="+managedKey+"\r\n\r\n")

	var upstream *websocket.Conn
	select {
	case upstream = <-f.worker.sockets:
	case <-time.After(5 * time.Second):
		t.Fatal("worker events socket not opened")
	}
	frame := []byte(`{"type":"agent","pad":"` + strings.Repeat("x", 1<<20) + `"}`)
	go func() {
		for upstream.Write(context.Background(), websocket.MessageText, frame) == nil {
		}
	}()
	select {
	case <-f.worker.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("stalled reader kept its Worker socket open")
	}
}

func TestDaemonEventsFollowConnectionOrder(t *testing.T) {
	s := New("")
	s.events.dial = func(ctx context.Context, _ string) (*websocket.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	sub, release := s.events.subscribe("u")
	defer release()
	for range 200 {
		old := &daemonConn{userID: "u", daemonID: "laptop", connectedAt: time.Now()}
		s.registerConn(old)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.unregisterConn(old) }()
		go func() {
			defer wg.Done()
			s.registerConn(&daemonConn{userID: "u", daemonID: "laptop", connectedAt: time.Now()})
		}()
		wg.Wait()
		var last string
		for len(sub.frames) > 0 {
			last = string(<-sub.frames)
		}
		if !strings.Contains(last, `"connected":true`) {
			t.Fatalf("last event %q contradicts the connected daemon", last)
		}
	}
}

func TestRelayFrameOverflowAsksSubscriberToReconnect(t *testing.T) {
	e := newManagedEvents()
	e.dial = func(ctx context.Context, _ string) (*websocket.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	sub, release := e.subscribe("o")
	defer release()
	for range managedSubscriberBuffer + 1 {
		e.publishDaemon("o", relayDaemon{DaemonID: "d"})
	}
	select {
	case <-sub.overflow:
	default:
		t.Fatal("full subscriber not flagged")
	}
	if len(sub.frames) != managedSubscriberBuffer {
		t.Fatalf("buffered %d, want %d", len(sub.frames), managedSubscriberBuffer)
	}
}

func TestManagedPages(t *testing.T) {
	f := newManagedFixture(t, true)
	for _, page := range []string{"managed.html", "admin.html"} {
		if err := os.WriteFile(filepath.Join(f.relay.webOut, page), []byte("<html>"+page+"</html>"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := f.do(t, "GET", "/managed", "", "", nil)
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Fatalf("anonymous /managed = %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for _, tc := range []struct {
		path, key string
		status    int
	}{{"/managed", otherKey, 200}, {"/admin", otherKey, 403}, {"/admin", managedKey, 200}} {
		req := cookieRequest(t, "GET", f.http.URL+tc.path, tc.key, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != tc.status || (tc.status == 200 && !strings.Contains(string(body), strings.TrimPrefix(tc.path, "/")+".html")) {
			t.Fatalf("%s as %s = %d %s", tc.path, tc.key, res.StatusCode, body)
		}
	}
}

// fakeAnalytics answers each dataset query. While broken is set, that dataset
// fails with a GraphQL error like an unknown field would.
func fakeAnalytics(t *testing.T, rows map[string]string, broken *atomic.Value) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"not authorized"}]}`)
			return
		}
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, meter := range costMeters {
			if !strings.Contains(req.Query, " "+meter.dataset+"(") {
				continue
			}
			if broken.Load() == meter.dataset {
				_, _ = io.WriteString(w, `{"data":null,"errors":[{"message":"unknown field"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"viewer":{"accounts":[{"rows":[`+rows[meter.dataset]+`]}]}},"errors":null}`)
			return
		}
		t.Errorf("unexpected query %s", req.Query)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func TestAdminCostsActualFromAnalytics(t *testing.T) {
	f := newManagedFixture(t, true)
	today := time.Now().UTC().Format(time.DateOnly)
	var broken atomic.Value
	broken.Store("durableObjectsPeriodicGroups")
	analytics, calls := fakeAnalytics(t, map[string]string{
		"aiInferenceAdaptiveGroups":     `{"dimensions":{"date":"` + today + `"},"sum":{"totalNeurons":110000}}`,
		"containersUsageAdaptiveGroups": `{"dimensions":{"date":"` + today + `"},"sum":{"cpuTimeSec":32500,"allocatedMemory":0,"allocatedDisk":0,"txBytes":0}}`,
		"durableObjectsInvocationsAdaptiveGroups": `{"dimensions":{"date":"` + today + `","type":"fetch"},"sum":{"requests":900000}},` +
			`{"dimensions":{"date":"` + today + `","type":"websocketMessage"},"sum":{"requests":2000000}}`,
	}, &broken)
	f.relay.costs.configure("acct", "cf-token")
	f.relay.costs.endpoint = analytics.URL
	costs := func(query string) map[string]any {
		t.Helper()
		res, body := f.do(t, "GET", "/api/v1/admin/costs"+query, managedKey, "", nil)
		actual, _ := body["actual"].(map[string]any)
		if res.StatusCode != 200 || actual == nil {
			t.Fatalf("costs = %d %v", res.StatusCode, body)
		}
		return actual
	}

	// Incomplete: reported, not cached.
	actual := costs("")
	if actual["complete"] != false || actual["unavailable"].(map[string]any)["durableObjectsPeriodicGroups"] == nil {
		t.Fatalf("incomplete actual = %v", actual)
	}
	queried := int32(len(costMeters))
	costs("")
	if calls.Load() != 2*queried {
		t.Fatal("incomplete actual was cached")
	}

	broken.Store("")
	actual = costs("")
	month := actual["month"].(map[string]any)
	// 100k neurons over the daily 10k at $0.011/1k = $1.10; 10k vCPU-s over 22.5k at $2e-5 = $0.20.
	// DO: 900k + 2M/20 WebSocket messages = 1M billable requests, inside the allowance.
	if actual["complete"] != true || math.Abs(month["ai_usd"].(float64)-1.10) > 1e-9 || math.Abs(month["containers_usd"].(float64)-0.20) > 1e-9 || month["do_usd"] != 0.0 || month["base_usd"] != 5.0 {
		t.Fatalf("month = %v", month)
	}
	if units := actual["days"].([]any)[0].(map[string]any)["units"].(map[string]any); units["do_requests"] != 1e6 || units["do_ws_messages"] != 2e6 {
		t.Fatalf("units = %v", units)
	}
	costs("")
	if calls.Load() != 3*queried {
		t.Fatal("complete actual refetched within the hour")
	}

	// A failed forced refresh keeps the last complete figures.
	broken.Store("containersUsageAdaptiveGroups")
	actual = costs("?refresh=1")
	if calls.Load() != 4*queried || actual["complete"] != true || actual["refresh_failed"].(map[string]any)["containersUsageAdaptiveGroups"] == nil {
		t.Fatalf("failed refresh = %v", actual)
	}
	if math.Abs(actual["month"].(map[string]any)["containers_usd"].(float64)-0.20) > 1e-9 {
		t.Fatalf("failed refresh replaced figures: %v", actual["month"])
	}
}

func TestCostFetchSurvivesCallerCancellation(t *testing.T) {
	var broken atomic.Value
	broken.Store("")
	analytics, _ := fakeAnalytics(t, map[string]string{}, &broken)
	c := newCostCache()
	c.configure("acct", "cf-token")
	c.endpoint = analytics.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.get(ctx, false, time.Now().UTC()); got == nil || !got.Complete {
		t.Fatalf("cancelled caller got %+v", got)
	}
}

func TestPriceActualSpreadsMonthlyAllowance(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	got := priceActual(map[string]map[string]float64{
		"2026-10-01": {"workers_requests": 6e6},
		"2026-10-02": {"workers_requests": 6e6},
		"2026-10-03": {"workers_requests": 1e6, "neurons": 5_000},
	}, map[string]string{}, now)
	want := []float64{0, 2e6 * 0.30e-6, 1e6 * 0.30e-6}
	for i, day := range got.Days {
		if math.Abs(day.WorkersUSD-want[i]) > 1e-12 || day.AIUSD != 0 {
			t.Fatalf("day %d = %+v, want workers %.6f", i, day, want[i])
		}
	}
	if math.Abs(got.Month.TotalUSD-(5+0.9)) > 1e-9 {
		t.Fatalf("month total = %v", got.Month.TotalUSD)
	}
	if math.Abs(got.ProjectedUSD-(5+0.9*31/10)) > 1e-9 {
		t.Fatalf("projected = %v", got.ProjectedUSD)
	}
}

// A managed-only user has no daemon: a key enabled for managed signs in alone
// and lands on /managed; any other key still needs a daemon.
func TestSignInWithoutADaemon(t *testing.T) {
	signIn := func(f *managedFixture, key string) *http.Response {
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := client.PostForm(f.http.URL+"/auth", url.Values{"token": {key}})
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	res := signIn(newManagedFixture(t, true), otherKey)
	if res.Header.Get("Location") != "/managed" || len(res.Cookies()) != 1 || res.Cookies()[0].Name != "rw_token" {
		t.Fatalf("managed sign-in = %d %q %v", res.StatusCode, res.Header.Get("Location"), res.Cookies())
	}
	for name, f := range map[string]*managedFixture{"not enabled": newManagedFixture(t, true), "unconfigured": newManagedFixture(t, false)} {
		key := map[string]string{"not enabled": strangerKey, "unconfigured": managedKey}[name]
		if res := signIn(f, key); res.Header.Get("Location") != "/?error=no_daemon" || len(res.Cookies()) != 0 {
			t.Fatalf("%s sign-in = %d %q", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

// Anyone can mint a key, so a key authenticates but does not authorize:
// one not enabled for managed reaches nothing on the Worker.
func TestManagedRefusesAKeyNotEnabled(t *testing.T) {
	f := newManagedFixture(t, true)
	res, me := f.do(t, "GET", "/api/v1/managed/me", strangerKey, "", nil)
	if res.StatusCode != 200 || me["managed"] != false || me["admin"] != false {
		t.Fatalf("me = %d %v", res.StatusCode, me)
	}
	for _, route := range []string{"GET /api/v1/managed/agents", "POST /api/v1/managed/agents", "GET /api/v1/managed/events", "GET /api/v1/managed/agents/a1/runs"} {
		method, path, _ := strings.Cut(route, " ")
		res, body := f.do(t, method, path, strangerKey, `{}`, nil)
		if res.StatusCode != http.StatusForbidden || body["error"] != errManagedNotEnabled.Error() {
			t.Fatalf("%s = %d %v", route, res.StatusCode, body)
		}
	}
	res, clients := f.do(t, "GET", "/api/v1/managed/clients", strangerKey, "", nil)
	if res.StatusCode != 200 || clients["gateway_error"] != errManagedNotEnabled.Error() {
		t.Fatalf("clients = %d %v", res.StatusCode, clients)
	}
	f.worker.mu.Lock()
	defer f.worker.mu.Unlock()
	if len(f.worker.requests) != 0 || f.worker.dials.Load() != 0 {
		t.Fatalf("the Worker was reached: %d requests, %d dials", len(f.worker.requests), f.worker.dials.Load())
	}
}
