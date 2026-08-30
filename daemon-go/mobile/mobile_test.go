package mobile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseTargeted(t *testing.T) {
	tests := []struct {
		text, mode, target, message string
		slash                       bool
	}{
		{"/notify @worker deploy done", "notify", "worker", "deploy done", true},
		{"/fyi status", "notify", "", "status", true},
		{"notify @worker status", "notify", "worker", "status", false},
		{"@worker investigate", "ask", "worker", "investigate", false},
	}
	for _, test := range tests {
		mode, target, message, ok := parseTargeted(test.text, test.slash)
		if !ok || mode != test.mode || target != test.target || message != test.message {
			t.Fatalf("parseTargeted(%q) = %q %q %q %v", test.text, mode, target, message, ok)
		}
	}
}

type capturedRequest struct {
	path    string
	auth    string
	payload map[string]any
}

func fakeDaemon(t *testing.T) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		requests <- capturedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), payload: payload}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ask":
			_, _ = w.Write([]byte(`{"correlation_id":"cid-1"}`))
		case "/notify", "/ack", "/answer":
			_, _ = w.Write([]byte(`{"status":"sent"}`))
		case "/peers":
			_, _ = w.Write([]byte(`{"peers":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func TestTelegramRoutesHumanMessageAsAsk(t *testing.T) {
	daemonServer, requests := fakeDaemon(t)
	telegramCalls := make(chan string, 10)
	telegramAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		telegramCalls <- r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(telegramAPI.Close)
	peer := NewDaemonPeer(daemonServer.URL, "secret", "telegram", "/telegram", "default")
	bot := NewTelegram("token", "42", peer)
	bot.apiBase = telegramAPI.URL
	if err := bot.onText(context.Background(), "@worker investigate"); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.path != "/ask" || request.auth != "Bearer secret" || request.payload["to_peer"] != "worker" || request.payload["text"] != "investigate" {
		t.Fatalf("bad daemon request: %+v", request)
	}
	select {
	case call := <-telegramCalls:
		t.Fatalf("successful Telegram ask should be quiet, got API call %s", call)
	default:
	}
}

func TestTelegramAcknowledgesUpdateOnlyAfterSuccessfulReply(t *testing.T) {
	daemonServer, _ := fakeDaemon(t)
	var mu sync.Mutex
	fail := true
	telegramAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shouldFail := fail
		mu.Unlock()
		if shouldFail {
			_, _ = w.Write([]byte(`{"ok":false,"description":"temporary failure"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(telegramAPI.Close)
	// Telegram chat IDs are large enough that fmt.Sprint(float64(id)) uses
	// scientific notation. Exercise the real JSON-decoded numeric shape so a
	// valid chat is not silently ignored before command dispatch.
	bot := NewTelegram("token", "347354611", NewDaemonPeer(daemonServer.URL, "secret", "telegram", "/telegram", "default"))
	bot.apiBase = telegramAPI.URL
	update := map[string]any{
		"update_id": float64(41),
		"message":   map[string]any{"message_id": float64(7), "text": "📋 peers", "chat": map[string]any{"id": float64(347354611)}},
	}
	if err := bot.handleUpdate(context.Background(), update); err == nil {
		t.Fatal("failed Telegram reply unexpectedly acknowledged")
	}
	if bot.offset != 0 {
		t.Fatalf("offset advanced after failure: %d", bot.offset)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := bot.handleUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if bot.offset != 42 {
		t.Fatalf("offset after successful retry = %d, want 42", bot.offset)
	}
}

func TestTelegramReplyKeyboardKeepsCurrentAndRecentPeers(t *testing.T) {
	bot := NewTelegram("token", "42", NewDaemonPeer("http://localhost:8377", "", "telegram", "/telegram", "default"))
	bot.setTarget("worker")
	bot.touchRecent("reviewer")
	bot.touchRecent("worker")

	encoded, err := json.Marshal(bot.replyKeyboard())
	if err != nil {
		t.Fatal(err)
	}
	keyboard := string(encoded)
	for _, want := range []string{"✦ worker", "💬 reviewer", "📋 peers", "❌ clear"} {
		if !strings.Contains(keyboard, want) {
			t.Fatalf("reply keyboard %s does not contain %q", keyboard, want)
		}
	}
	if target, ok := keyboardTarget("💬 reviewer"); !ok || target != "reviewer" {
		t.Fatalf("keyboard target = %q, %v", target, ok)
	}
}

func TestTelegramReplyKeyboardHidesCanonicalPeerID(t *testing.T) {
	bot := NewTelegram("token", "42", NewDaemonPeer("http://localhost:8377", "", "telegram", "/telegram", "default"))
	bot.setTargetLabel("repow-0-12345678", "orchestrator-pi")

	encoded, err := json.Marshal(bot.replyKeyboard())
	if err != nil {
		t.Fatal(err)
	}
	keyboard := string(encoded)
	if !strings.Contains(keyboard, "✦ orchestrator-pi") || strings.Contains(keyboard, "repow-0-12345678") {
		t.Fatalf("reply keyboard leaked routing identity: %s", keyboard)
	}
	if got := bot.resolveTarget("orchestrator-pi"); got != "repow-0-12345678" {
		t.Fatalf("resolved keyboard target = %q", got)
	}
}

func TestTelegramSendsAgentAttachmentAsPhoto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.png")
	if err := os.WriteFile(path, []byte("fake image"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := make(chan *http.Request, 2)
	telegramAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Clone(context.Background())
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(telegramAPI.Close)
	bot := NewTelegram("token", "42", NewDaemonPeer("http://localhost:8377", "", "telegram", "/telegram", "default"))
	bot.apiBase = telegramAPI.URL
	if err := bot.onDaemon(context.Background(), map[string]any{
		"type": "notify", "from_peer": "worker", "text": "done",
		"attachments": []any{map[string]any{"path": path, "filename": "result.png", "content_type": "image/png"}},
	}); err != nil {
		t.Fatal(err)
	}
	message, photo := <-calls, <-calls
	if !strings.HasSuffix(message.URL.Path, "/sendMessage") || !strings.HasSuffix(photo.URL.Path, "/sendPhoto") || !strings.HasPrefix(photo.Header.Get("Content-Type"), "multipart/form-data;") {
		t.Fatalf("Telegram calls = %s then %s (%s)", message.URL.Path, photo.URL.Path, photo.Header.Get("Content-Type"))
	}
}

func TestSlackFiltersChannelAndUsesStickyTarget(t *testing.T) {
	daemonServer, requests := fakeDaemon(t)
	var mu sync.Mutex
	var posts []map[string]any
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		posts = append(posts, payload)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(slackAPI.Close)
	peer := NewDaemonPeer(daemonServer.URL, "secret", "slack", "/slack", "default")
	bot := NewSlack("xoxb", "xapp", "C1", peer)
	bot.apiBase = slackAPI.URL
	bot.setTarget("worker")
	if err := bot.onEvent(context.Background(), map[string]any{"type": "message", "channel": "other", "text": "ignore"}); err != nil {
		t.Fatal(err)
	}
	if err := bot.onEvent(context.Background(), map[string]any{"type": "message", "channel": "C1", "text": "investigate"}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.path != "/ask" || request.payload["to_peer"] != "worker" || request.payload["text"] != "investigate" {
		t.Fatalf("bad daemon request: %+v", request)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "worker") {
		t.Fatalf("Slack confirmations = %v", posts)
	}
}
