package mobile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTelegramAPI records every Bot API call and answers from script, called
// with the 1-based call number. The default answer is a sent message.
type fakeTelegramAPI struct {
	mu      sync.Mutex
	calls   []telegramCall
	script  func(n int, call telegramCall) string
	nextMsg int64
	*httptest.Server
}

type telegramCall struct {
	method  string
	payload map[string]any
}

func newFakeTelegramAPI(t *testing.T, script func(int, telegramCall) string) *fakeTelegramAPI {
	t.Helper()
	api := &fakeTelegramAPI{script: script, nextMsg: 300}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		call := telegramCall{method: r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], payload: payload}
		api.mu.Lock()
		api.calls = append(api.calls, call)
		n := len(api.calls)
		api.nextMsg++
		msg := api.nextMsg
		api.mu.Unlock()
		if api.script != nil {
			if body := api.script(n, call); body != "" {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":` + jsonInt(msg) + `}}`))
	}))
	t.Cleanup(api.Close)
	return api
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (a *fakeTelegramAPI) sent() []telegramCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []telegramCall
	for _, c := range a.calls {
		if c.method == "sendMessage" {
			out = append(out, c)
		}
	}
	return out
}

// newSurfaceBot is a Telegram bridge whose receipts are captured instead of
// written to a daemon socket.
func newSurfaceBot(t *testing.T, apiURL, daemonURL string) (*Telegram, *[]map[string]any) {
	t.Helper()
	bot := NewTelegram("SECRET-TOKEN-123", "42", NewDaemonPeer(daemonURL, "", "telegram", "/telegram", "default"))
	bot.apiBase = apiURL
	bot.retryDelay = time.Millisecond
	var mu sync.Mutex
	receipts := &[]map[string]any{}
	bot.writeFrame = func(_ context.Context, frame any) error {
		mu.Lock()
		*receipts = append(*receipts, frame.(map[string]any))
		mu.Unlock()
		return nil
	}
	return bot, receipts
}

func inboxFrame(id, kind, text, replyRef string, extra map[string]any) map[string]any {
	item := map[string]any{"id": id, "kind": kind, "from": "orchestrator", "text": text}
	for k, v := range extra {
		item[k] = v
	}
	return map[string]any{"type": "inbox_item", "delivery_id": id, "item": item, "reply_to_ref": replyRef}
}

func TestTelegramSurfaceThreadsAskAndReceiptsMessageID(t *testing.T) {
	api := newFakeTelegramAPI(t, nil)
	bot, receipts := newSurfaceBot(t, api.URL, "http://127.0.0.1:1")
	question := map[string]any{"kind": "choice", "options": []any{map[string]any{"id": "yes", "title": "Yes"}, map[string]any{"id": "no", "title": "No"}}}
	bot.handleInboxItem(context.Background(), inboxFrame("inb-1", "ask", "merge #12?", "77", map[string]any{"question": question, "correlation_id": "ask-1"}))

	sent := api.sent()
	if len(sent) != 1 {
		t.Fatalf("sendMessage calls = %+v", sent)
	}
	payload := sent[0].payload
	if !strings.HasPrefix(payload["text"].(string), "❓ @orchestrator\nmerge #12?") {
		t.Fatalf("text = %q", payload["text"])
	}
	if reply, _ := payload["reply_parameters"].(map[string]any); reply["message_id"] != float64(77) || reply["allow_sending_without_reply"] != true {
		t.Fatalf("reply_parameters = %+v", payload["reply_parameters"])
	}
	markup, _ := json.Marshal(payload["reply_markup"])
	if !strings.Contains(string(markup), `"ix:o:inb-1:0"`) || !strings.Contains(string(markup), `"ix:o:inb-1:1"`) {
		t.Fatalf("markup = %s", markup)
	}
	if len(*receipts) != 1 || (*receipts)[0]["status"] != "pushed" || (*receipts)[0]["surface_ref"] != "301" || (*receipts)[0]["delivery_id"] != "inb-1" {
		t.Fatalf("receipts = %+v", *receipts)
	}
}

func TestTelegramSurfaceSplitsLongItemsAndKeepsFirstRef(t *testing.T) {
	api := newFakeTelegramAPI(t, nil)
	bot, receipts := newSurfaceBot(t, api.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-2", "notify", strings.Repeat("x", 9000), "", nil))
	sent := api.sent()
	if len(sent) != 3 {
		t.Fatalf("chunks = %d", len(sent))
	}
	for i, call := range sent {
		if n := len([]rune(call.payload["text"].(string))); n > 4096 {
			t.Fatalf("chunk %d has %d runes", i, n)
		}
		if _, has := call.payload["reply_markup"]; has != (i == len(sent)-1) {
			t.Fatalf("markup on chunk %d: %v", i, has)
		}
	}
	if (*receipts)[0]["surface_ref"] != "301" {
		t.Fatalf("ref must name the first chunk: %+v", *receipts)
	}
}

func TestTelegramSurfaceRetriesRateLimitAndClassifiesRejection(t *testing.T) {
	limited := newFakeTelegramAPI(t, func(n int, _ telegramCall) string {
		if n < 3 {
			return `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":0}}`
		}
		return ""
	})
	bot, receipts := newSurfaceBot(t, limited.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-3", "notify", "hi", "", nil))
	if len(limited.sent()) != 3 || (*receipts)[0]["status"] != "pushed" {
		t.Fatalf("rate limit not retried: calls=%d receipts=%+v", len(limited.sent()), *receipts)
	}

	rejected := newFakeTelegramAPI(t, func(int, telegramCall) string {
		return `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	})
	bot, receipts = newSurfaceBot(t, rejected.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-4", "notify", "hi", "", nil))
	if len(rejected.sent()) != 1 {
		t.Fatalf("a rejection must not be retried: %d calls", len(rejected.sent()))
	}
	if r := (*receipts)[0]; r["status"] != "failed" || r["error_class"] != "rejected" {
		t.Fatalf("receipt = %+v", r)
	}
}

func TestTelegramNetworkErrorsNeverCarryTheToken(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()
	bot, receipts := newSurfaceBot(t, dead.URL, "http://127.0.0.1:1")
	err := bot.call(context.Background(), "sendMessage", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN-123") || errorClass(err) != "network" {
		t.Fatalf("err = %v (class %s)", err, errorClass(err))
	}
	bot.handleInboxItem(context.Background(), inboxFrame("inb-5", "notify", "hi", "", nil))
	raw, _ := json.Marshal(*receipts)
	if (*receipts)[0]["error_class"] != "network" || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("receipt = %s", raw)
	}
}

func TestTelegramFullQueueReportsOverloadedInsteadOfBlocking(t *testing.T) {
	bot, receipts := newSurfaceBot(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	bot.inbox = make(chan map[string]any) // no worker, no buffer
	if err := bot.onDaemon(context.Background(), inboxFrame("inb-6", "notify", "hi", "", nil)); err != nil {
		t.Fatal(err)
	}
	if r := (*receipts)[0]; r["status"] != "failed" || r["error_class"] != "overloaded" {
		t.Fatalf("receipt = %+v", r)
	}
}

// fakeInboxDaemon answers /inbox/reply with status for reply-to lookups and
// records every request.
func fakeInboxDaemon(t *testing.T, replyStatus int) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		requests <- capturedRequest{path: r.URL.Path, payload: payload}
		switch {
		case r.URL.Path == "/inbox/reply":
			w.WriteHeader(replyStatus)
			_, _ = w.Write([]byte(`{"ok":true,"action":"acked"}`))
		case strings.HasPrefix(r.URL.Path, "/inbox/"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/ask":
			_, _ = w.Write([]byte(`{"correlation_id":"ask-9"}`))
		default:
			_, _ = w.Write([]byte(`{"peers":[]}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func replyUpdate(replyTo int64) map[string]any {
	return map[string]any{"update_id": float64(1), "message": map[string]any{
		"message_id": float64(500), "text": "yes, merge", "chat": map[string]any{"id": float64(42)},
		"reply_to_message": map[string]any{"message_id": float64(replyTo)},
	}}
}

func TestTelegramReplyToAnInboxMessageRepliesThroughTheInbox(t *testing.T) {
	api := newFakeTelegramAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusOK)
	bot, _ := newSurfaceBot(t, api.URL, daemon.URL)
	if err := bot.onUpdate(context.Background(), replyUpdate(301)); err != nil {
		t.Fatal(err)
	}
	req := <-requests
	if req.path != "/inbox/reply" || req.payload["surface_ref"] != "301" || req.payload["ref"] != "500" || req.payload["text"] != "yes, merge" {
		t.Fatalf("reply request = %+v", req)
	}
	select {
	case extra := <-requests:
		t.Fatalf("a handled reply must not also send an ask: %+v", extra)
	default:
	}
}

func TestTelegramReplyToAnythingElseFallsBackToAnAskWithOrigin(t *testing.T) {
	api := newFakeTelegramAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusNotFound)
	bot, _ := newSurfaceBot(t, api.URL, daemon.URL)
	bot.setTarget("repow-1-orch")
	if err := bot.onUpdate(context.Background(), replyUpdate(9)); err != nil {
		t.Fatal(err)
	}
	if req := <-requests; req.path != "/inbox/reply" {
		t.Fatalf("first request = %+v", req)
	}
	ask := <-requests
	origin, _ := ask.payload["origin"].(map[string]any)
	if ask.path != "/ask" || ask.payload["to_peer"] != "repow-1-orch" || origin["surface"] != "telegram" || origin["ref"] != "500" {
		t.Fatalf("fallback ask = %+v", ask)
	}
}

func TestTelegramInboxButtonsReplyAndMarkRead(t *testing.T) {
	api := newFakeTelegramAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusOK)
	bot, _ := newSurfaceBot(t, api.URL, daemon.URL)
	bot.rememberOptions("inb-7", []string{"yes", "no"})

	for data, want := range map[string]capturedRequest{
		"ix:a:inb-7":   {path: "/inbox/inb-7/reply", payload: map[string]any{"surface": "telegram", "text": ""}},
		"ix:o:inb-7:1": {path: "/inbox/inb-7/reply", payload: map[string]any{"surface": "telegram", "option_id": "no"}},
		"ix:s:inb-7":   {path: "/inbox/inb-7/read", payload: map[string]any{"surface": "telegram"}},
	} {
		if err := bot.onCallback(context.Background(), data); err != nil {
			t.Fatal(err)
		}
		got := <-requests
		gotJSON, _ := json.Marshal(got.payload)
		wantJSON, _ := json.Marshal(want.payload)
		if got.path != want.path || string(gotJSON) != string(wantJSON) {
			t.Fatalf("%s -> %s %s, want %s %s", data, got.path, gotJSON, want.path, wantJSON)
		}
	}
}
