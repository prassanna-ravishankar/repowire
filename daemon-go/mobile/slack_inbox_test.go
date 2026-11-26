package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSlackAPI records every Web API call and answers from script, called
// with the 1-based call number; "" means the default: a posted message with
// the next ts. It never talks to Slack.
type fakeSlackAPI struct {
	mu     sync.Mutex
	calls  []slackCall
	script func(n int, call slackCall, w http.ResponseWriter) bool
	nextTS int
	*httptest.Server
}

type slackCall struct {
	method  string
	auth    string
	payload map[string]any
}

func newFakeSlackAPI(t *testing.T, script func(int, slackCall, http.ResponseWriter) bool) *fakeSlackAPI {
	t.Helper()
	api := &fakeSlackAPI{script: script, nextTS: 100}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		call := slackCall{method: strings.TrimPrefix(r.URL.Path, "/api/"), auth: r.Header.Get("Authorization"), payload: payload}
		api.mu.Lock()
		api.calls = append(api.calls, call)
		n := len(api.calls)
		api.nextTS++
		ts := fmt.Sprintf("1712345678.%06d", api.nextTS)
		api.mu.Unlock()
		if api.script != nil && api.script(n, call, w) {
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"channel":"C1","ts":%q}`, ts)
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *fakeSlackAPI) posts() []slackCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []slackCall
	for _, c := range a.calls {
		if c.method == "chat.postMessage" {
			out = append(out, c)
		}
	}
	return out
}

// newSlackSurface is a Slack bridge whose receipts are captured instead of
// written to a daemon socket.
func newSlackSurface(t *testing.T, apiURL, daemonURL string) (*Slack, *[]map[string]any) {
	t.Helper()
	bot := NewSlack("xoxb-SECRET-BOT", "xapp-SECRET-APP", "C1", NewDaemonPeer(daemonURL, "", "slack", "/slack", "default"))
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

func TestSlackDeclaresAnInboxSurfaceThatQuietHoursMayHold(t *testing.T) {
	bot := NewSlack("b", "a", "C1", NewDaemonPeer("http://127.0.0.1:1", "", "slack", "/slack", "default"))
	if !slices.Contains(bot.daemon.capabilities, inboxSurfaceCapability) || !slices.Contains(bot.daemon.capabilities, inboxHoldCapability) {
		t.Fatalf("capabilities = %v", bot.daemon.capabilities)
	}
}

// An item on an ask the daemon threads under an earlier message is posted in
// that message's Slack thread, with its choices as buttons, and receipted
// with the message ts.
func TestSlackSurfaceThreadsAskAndReceiptsTS(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	bot, receipts := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	question := map[string]any{"kind": "choice", "scope": "tool_permission", "options": []any{map[string]any{"id": "yes", "title": "Yes"}, map[string]any{"id": "no", "title": "No"}}}
	bot.handleInboxItem(context.Background(), inboxFrame("inb-1", "ask", "merge #12 <now>?", "1712345678.000050", map[string]any{"question": question, "correlation_id": "ask-1"}))

	posts := api.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %+v", posts)
	}
	p := posts[0]
	if p.auth != "Bearer xoxb-SECRET-BOT" || p.payload["channel"] != "C1" || p.payload["thread_ts"] != "1712345678.000050" {
		t.Fatalf("post = %+v", p)
	}
	if !strings.HasPrefix(p.payload["text"].(string), ":question: *@orchestrator*\nmerge #12 &lt;now&gt;?") {
		t.Fatalf("text = %q (escaped for mrkdwn)", p.payload["text"])
	}
	blocks, _ := json.Marshal(p.payload["blocks"])
	for _, want := range []string{`"ix:o:inb-1:0"`, `"ix:o:inb-1:1"`, `"deny:ask-1"`} {
		if !strings.Contains(string(blocks), want) {
			t.Fatalf("blocks missing %s: %s", want, blocks)
		}
	}
	if len(*receipts) != 1 || (*receipts)[0]["status"] != "pushed" || (*receipts)[0]["surface_ref"] != "1712345678.000101" || (*receipts)[0]["delivery_id"] != "inb-1" {
		t.Fatalf("receipts = %+v", *receipts)
	}
}

// A new item starts its own thread; a long one is split, and every chunk
// after the first goes into the first chunk's thread. The receipt lists
// every chunk, so a reply in any of their threads reaches the item.
func TestSlackSurfaceSplitsLongItemsIntoTheirOwnThread(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	bot, receipts := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-2", "notify", strings.Repeat("line of the report\n", 400), "", nil))
	posts := api.posts()
	if len(posts) < 2 {
		t.Fatalf("a %d-char item was not split: %d posts", 400*19, len(posts))
	}
	if _, threaded := posts[0].payload["thread_ts"]; threaded {
		t.Fatal("a new item's first message must start its own thread")
	}
	for _, p := range posts[1:] {
		if p.payload["thread_ts"] != "1712345678.000101" {
			t.Fatalf("a later chunk is not in the first chunk's thread: %+v", p.payload["thread_ts"])
		}
	}
	refs, _ := (*receipts)[0]["surface_refs"].([]string)
	if len(refs) != len(posts) || (*receipts)[0]["surface_ref"] != refs[0] {
		t.Fatalf("receipt = %+v", (*receipts)[0])
	}
}

// A rate limit (HTTP 429 with Retry-After, or Slack's "ratelimited") is
// retried; a rejection (channel_not_found) fails at once with its class.
func TestSlackSurfaceRetriesRateLimitAndClassifiesRejection(t *testing.T) {
	api := newFakeSlackAPI(t, func(n int, _ slackCall, w http.ResponseWriter) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		if n == 2 {
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			return true
		}
		return false
	})
	bot, receipts := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-3", "notify", "ok", "", nil))
	if len(api.posts()) != 3 || (*receipts)[0]["status"] != "pushed" {
		t.Fatalf("posts %d, receipt %+v; want two retries then a push", len(api.posts()), (*receipts)[0])
	}

	rejecting := newFakeSlackAPI(t, func(_ int, _ slackCall, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		return true
	})
	bot2, receipts2 := newSlackSurface(t, rejecting.URL, "http://127.0.0.1:1")
	bot2.handleInboxItem(context.Background(), inboxFrame("inb-4", "notify", "x", "", nil))
	if len(rejecting.posts()) != 1 || (*receipts2)[0]["status"] != "failed" || (*receipts2)[0]["error_class"] != "rejected" {
		t.Fatalf("posts %d, receipt %+v; want one attempt, failed, rejected", len(rejecting.posts()), (*receipts2)[0])
	}
}

// A response lost after the request was sent is not retried (Slack may have
// posted it); it fails as network.
func TestSlackLostResponseIsNotRetried(t *testing.T) {
	api := newFakeSlackAPI(t, func(n int, _ slackCall, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`not json`))
		return true
	})
	bot, receipts := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-9", "notify", "x", "", nil))
	if len(api.posts()) != 1 || (*receipts)[0]["status"] != "failed" || (*receipts)[0]["error_class"] != "network" {
		t.Fatalf("posts %d, receipt %+v; want one attempt, failed, network", len(api.posts()), (*receipts)[0])
	}
}

// A question's last chunk is one Block Kit section, which holds at most
// 3000 characters, so chunks stay under it.
func TestSlackLongQuestionFitsASection(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	bot, _ := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	question := map[string]any{"kind": "acknowledge"}
	bot.handleInboxItem(context.Background(), inboxFrame("inb-10", "ask", strings.Repeat("x", 3200), "", map[string]any{"question": question}))
	for _, p := range api.posts() {
		blocks, _ := p.payload["blocks"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			text, _ := block["text"].(map[string]any)
			if s, _ := text["text"].(string); len([]rune(s)) > 3000 {
				t.Fatalf("a section of %d characters", len([]rune(s)))
			}
		}
	}
}

// A network failure is receipted as network, and no error text carries a
// token (they travel in a header, and redact covers both). A refused
// connection never reached Slack, so it is the one network failure retried.
func TestSlackNetworkErrorsNeverCarryATokenAndFullQueueIsOverloaded(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	api.Close()
	bot, receipts := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	err := bot.botAPI(context.Background(), "chat.postMessage", map[string]any{}, nil)
	var se *slackError
	if err == nil || strings.Contains(err.Error(), "SECRET") || slackErrorClass(err) != "network" || !errors.As(err, &se) || !se.unsent {
		t.Fatalf("err = %v (want network, unsent)", err)
	}
	bot.inbox = make(chan map[string]any) // no worker: the queue is always full
	if err := bot.onDaemon(context.Background(), inboxFrame("inb-5", "notify", "x", "", nil)); err != nil {
		t.Fatal(err)
	}
	if len(*receipts) != 1 || (*receipts)[0]["status"] != "failed" || (*receipts)[0]["error_class"] != "overloaded" {
		t.Fatalf("receipts = %+v", *receipts)
	}
}

func threadReply(threadTS string) map[string]any {
	return map[string]any{"type": "message", "channel": "C1", "user": "U1", "text": "yes, merge", "ts": "1712345678.000900", "thread_ts": threadTS}
}

// A reply in an inbox item's thread answers that item through the inbox, and
// nothing else is sent.
func TestSlackThreadReplyRepliesThroughTheInbox(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusOK)
	bot, _ := newSlackSurface(t, api.URL, daemon.URL)
	if err := bot.onEvent(context.Background(), threadReply("1712345678.000101")); err != nil {
		t.Fatal(err)
	}
	req := nextAction(requests)
	if req.path != "/inbox/reply" || req.payload["surface"] != "slack" || req.payload["surface_ref"] != "1712345678.000101" ||
		req.payload["ref"] != "1712345678.000101" || req.payload["text"] != "yes, merge" {
		t.Fatalf("reply request = %+v", req)
	}
	for len(requests) > 0 {
		if extra := <-requests; extra.path == "/ask" {
			t.Fatalf("a handled reply must not also send an ask: %+v", extra)
		}
	} // A reply also sent to the channel is still a reply.
	broadcast := threadReply("1712345678.000101")
	broadcast["subtype"] = "thread_broadcast"
	if err := bot.onEvent(context.Background(), broadcast); err != nil {
		t.Fatal(err)
	}
	if req := nextAction(requests); req.path != "/inbox/reply" {
		t.Fatalf("thread_broadcast reply = %+v", req)
	}
}

// A reply in a thread that is no inbox item's (404) is an ordinary message:
// it goes to the current conversation as an ask.
func TestSlackThreadReplyElsewhereFallsBackToAnAsk(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusNotFound)
	bot, _ := newSlackSurface(t, api.URL, daemon.URL)
	bot.setTarget("repow-1-orch")
	if err := bot.onEvent(context.Background(), threadReply("1712345678.000009")); err != nil {
		t.Fatal(err)
	}
	if req := nextAction(requests); req.path != "/inbox/reply" {
		t.Fatalf("first request = %+v", req)
	}
	ask := nextAction(requests)
	origin, _ := ask.payload["origin"].(map[string]any)
	if ask.path != "/ask" || ask.payload["to_peer"] != "repow-1-orch" || origin["surface"] != "slack" || origin["ref"] != "1712345678.000009" {
		t.Fatalf("fallback = %+v (origin must be the thread root)", ask)
	}
}

// A top-level channel message is an ask whose origin is its own ts, so the
// agent's answer threads under it.
func TestSlackChannelAskCarriesItsOrigin(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusOK)
	bot, _ := newSlackSurface(t, api.URL, daemon.URL)
	bot.setTarget("repow-1-orch")
	msg := map[string]any{"type": "message", "channel": "C1", "user": "U1", "text": "status?", "ts": "1712345678.000700"}
	if err := bot.onEvent(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	ask := nextAction(requests)
	origin, _ := ask.payload["origin"].(map[string]any)
	if ask.path != "/ask" || origin["surface"] != "slack" || origin["ref"] != "1712345678.000700" {
		t.Fatalf("ask = %+v", ask)
	}
}

// Buttons answer through the inbox (a choice by its option id, an Ack with
// no text), and Deny answers the approval as denied; the outcome is posted
// in the item's thread.
func TestSlackInboxButtons(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	daemon, requests := fakeInboxDaemon(t, http.StatusOK)
	bot, _ := newSlackSurface(t, api.URL, daemon.URL)
	question := map[string]any{"kind": "choice", "options": []any{map[string]any{"id": "yes"}, map[string]any{"id": "no"}}}
	bot.handleInboxItem(context.Background(), inboxFrame("inb-6", "ask", "merge?", "", map[string]any{"question": question}))
	interaction := func(value string) map[string]any {
		return map[string]any{"actions": []any{map[string]any{"value": value}}, "message": map[string]any{"ts": "1712345678.000101"}}
	}
	if err := bot.onInteraction(context.Background(), interaction("ix:o:inb-6:1")); err != nil {
		t.Fatal(err)
	}
	if req := nextAction(requests); req.path != "/inbox/inb-6/reply" || req.payload["option_id"] != "no" || req.payload["surface"] != "slack" {
		t.Fatalf("choice = %+v", req)
	}
	if err := bot.onInteraction(context.Background(), interaction("ix:a:inb-7")); err != nil {
		t.Fatal(err)
	}
	if req := nextAction(requests); req.path != "/inbox/inb-7/reply" || req.payload["text"] != "" {
		t.Fatalf("ack = %+v", req)
	}
	if err := bot.onInteraction(context.Background(), interaction("deny:ask-9")); err != nil {
		t.Fatal(err)
	}
	if req := nextAction(requests); req.path != "/answer" || req.payload["correlation_id"] != "ask-9" || req.payload["outcome"] != "denied" {
		t.Fatalf("deny = %+v", req)
	}
	// Outcomes go to the item's thread.
	for _, p := range api.posts()[1:] {
		if p.payload["thread_ts"] != "1712345678.000101" {
			t.Fatalf("an outcome was posted outside the item's thread: %+v", p.payload)
		}
	}
}

// A plain notify renders as a message without buttons; the legacy notify
// frame path is unchanged.
func TestSlackPlainItemHasNoButtons(t *testing.T) {
	api := newFakeSlackAPI(t, nil)
	bot, _ := newSlackSurface(t, api.URL, "http://127.0.0.1:1")
	bot.handleInboxItem(context.Background(), inboxFrame("inb-8", "notify", "build is green", "", nil))
	if posts := api.posts(); len(posts) != 1 || posts[0].payload["blocks"] != nil {
		t.Fatalf("posts = %+v", posts)
	}
}
