package relayserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	cfg := ManagedConfig{AdminUsers: []string{userID(managedKey)}}
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
	if got := ManagedConfigFromEnv().AdminUsers; len(got) != 2 || got[1] != "token-b" {
		t.Fatalf("admin users = %v", got)
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

func TestManagedEventsBridgeOpensLazilyAndShares(t *testing.T) {
	f := newManagedFixture(t, true)
	if f.worker.dials.Load() != 0 {
		t.Fatal("upstream opened without a subscriber")
	}
	first, closeFirst := openEvents(t, f, managedKey)
	nextFrame(t, first, `"type":"daemons"`)
	if frame := nextFrame(t, first, `"type":"gateway"`); !strings.Contains(frame, `"connected":true`) {
		t.Fatalf("gateway frame = %q", frame)
	}
	var upstream *websocket.Conn
	select {
	case upstream = <-f.worker.sockets:
	case <-time.After(5 * time.Second):
		t.Fatal("worker events socket not opened")
	}
	req, _ := f.worker.last()
	if req.Header.Get("X-Repowire-Owner") != userID(managedKey) {
		t.Fatalf("events owner = %q", req.Header.Get("X-Repowire-Owner"))
	}

	second, closeSecond := openEvents(t, f, managedKey)
	nextFrame(t, second, `"type":"daemons"`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := upstream.Write(ctx, websocket.MessageText, []byte("{\n \"type\": \"agent\", \"agent\": {\"id\": \"a1\"}\n}")); err != nil {
		t.Fatal(err)
	}
	for _, frames := range []<-chan string{first, second} {
		if frame := nextFrame(t, frames, `"type":"agent"`); frame != "data: {\"type\":\"agent\",\"agent\":{\"id\":\"a1\"}}\n" {
			t.Fatalf("agent frame = %q", frame)
		}
	}

	f.relay.registerConn(&daemonConn{userID: userID(managedKey), daemonID: "laptop", connectedAt: time.Now()})
	if frame := nextFrame(t, first, `"type":"daemon"`); !strings.HasPrefix(frame, "event: relay\n") || !strings.Contains(frame, `"connected":true`) {
		t.Fatalf("daemon frame = %q", frame)
	}
	if f.worker.dials.Load() != 1 {
		t.Fatalf("dials = %d, want one shared upstream", f.worker.dials.Load())
	}

	closeFirst()
	closeSecond()
	select {
	case <-f.worker.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not closed after the last subscriber left")
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

func TestOwnerStreamDropsSlowSubscriber(t *testing.T) {
	e := newManagedEvents()
	e.dial = func(ctx context.Context, _ string) (*websocket.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	frames, release := e.subscribe("o")
	defer release()
	stream := e.owners["o"]
	for range managedSubscriberBuffer + 1 {
		stream.publish([]byte("data: {}\n\n"))
	}
	count := 0
	for range frames {
		count++
	}
	if count != managedSubscriberBuffer {
		t.Fatalf("delivered %d before closing, want %d", count, managedSubscriberBuffer)
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

// fakeAnalytics answers each dataset query; datasets in broken fail with a
// GraphQL error like an unknown field would.
func fakeAnalytics(t *testing.T, rows map[string]string, broken map[string]bool) (*httptest.Server, *atomic.Int32) {
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
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, meter := range costMeters {
			if !strings.Contains(req.Query, " "+meter.dataset+"(") {
				continue
			}
			if broken[meter.dataset] {
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
	analytics, calls := fakeAnalytics(t, map[string]string{
		"aiInferenceAdaptiveGroups":     `{"dimensions":{"date":"` + today + `"},"sum":{"totalNeurons":110000}}`,
		"containersUsageAdaptiveGroups": `{"dimensions":{"date":"` + today + `"},"sum":{"cpuTimeSec":32500,"allocatedMemory":0,"allocatedDisk":0,"txBytes":0}}`,
	}, map[string]bool{"durableObjectsPeriodicGroups": true})
	f.relay.costs.configure("acct", "cf-token")
	f.relay.costs.endpoint = analytics.URL

	res, costs := f.do(t, "GET", "/api/v1/admin/costs", managedKey, "", nil)
	actual, _ := costs["actual"].(map[string]any)
	if res.StatusCode != 200 || actual == nil {
		t.Fatalf("costs = %d %v", res.StatusCode, costs)
	}
	month := actual["month"].(map[string]any)
	// 100k neurons over the daily 10k at $0.011/1k = $1.10; 10k vCPU-s over 22.5k at $2e-5 = $0.20.
	if math.Abs(month["ai_usd"].(float64)-1.10) > 1e-9 || math.Abs(month["containers_usd"].(float64)-0.20) > 1e-9 || month["base_usd"] != 5.0 {
		t.Fatalf("month = %v", month)
	}
	if unavailable := actual["unavailable"].(map[string]any); len(unavailable) != 1 || unavailable["durableObjectsPeriodicGroups"] == nil {
		t.Fatalf("unavailable = %v", unavailable)
	}
	queried := calls.Load()
	if queried != int32(len(costMeters)) {
		t.Fatalf("queries = %d", queried)
	}
	f.do(t, "GET", "/api/v1/admin/costs", managedKey, "", nil)
	if calls.Load() != queried {
		t.Fatal("cached actual refetched within the hour")
	}
	f.do(t, "GET", "/api/v1/admin/costs?refresh=1", managedKey, "", nil)
	if calls.Load() != 2*queried {
		t.Fatal("refresh=1 did not refetch")
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
