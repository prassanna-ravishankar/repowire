package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// inboxStack is the real delivery stack (WebSocket transport, registry,
// SQLite store, inbox) behind an httptest server, with no fakes in the path.
type inboxStack struct {
	h      *Hub
	srv    *httptest.Server
	store  *state.Store
	asks   *service.AskTracker
	inbox  *service.Inbox
	human  proto.PeerID
	mcpDel *service.PeerDelivery
}

func newInboxStack(t *testing.T) *inboxStack {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(context.Background(), memStore{}, deadLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "")
	asks := service.NewAskTracker(0)
	delivery := service.NewPeerDelivery(reg, h.Router(), transport, asks, nil)
	inbox := service.NewInbox(store, reg, transport, asks, config.HumanConfig{})
	humanID, humanName, err := service.RegisterHumanPeer(context.Background(), reg, "prass", "m")
	if err != nil {
		t.Fatal(err)
	}
	inbox.SetHuman(humanID, humanName)
	delivery.WithInbox(inbox)
	h.WithMessaging(delivery, store).WithAskLifecycle(asks, delivery, reg).WithInbox(inbox)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.HandleWS)
	h.messaging.Register(mux, h.requireAuth)
	h.registerAskLifecycleRoutes(mux)
	h.registerInboxRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		inbox.Close()
		_ = store.Close()
	})
	return &inboxStack{h: h, srv: srv, store: store, asks: asks, inbox: inbox, human: humanID, mcpDel: delivery}
}

// wsClient is one connected peer that records every frame it was handed.
type wsClient struct {
	t      *testing.T
	conn   *websocket.Conn
	id     proto.PeerID
	mu     sync.Mutex
	frames []map[string]any
	// receipt, when set, answers inbox_item frames (the fake surface).
	receipt func(frame map[string]any, n int) map[string]any
}

func (s *inboxStack) connect(t *testing.T, frame map[string]any, receipt func(map[string]any, int) map[string]any) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	frame["type"] = "connect"
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatal(err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, conn, &connected); err != nil || connected.Type != proto.FrameConnected {
		t.Fatalf("connect: %+v %v", connected, err)
	}
	c := &wsClient{t: t, conn: conn, id: connected.SessionID, receipt: receipt}
	go c.loop()
	t.Cleanup(func() { _ = conn.CloseNow() })
	deadline := time.Now().Add(2 * time.Second)
	for !s.h.transport.IsConnected(c.id) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return c
}

func (c *wsClient) loop() {
	ctx := context.Background()
	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, c.conn, &frame); err != nil {
			return
		}
		c.mu.Lock()
		c.frames = append(c.frames, frame)
		n := len(c.frames)
		c.mu.Unlock()
		if frame["type"] == "inbox_item" && c.receipt != nil {
			ack := c.receipt(frame, n)
			ack["type"], ack["delivery_id"] = "delivery_ack", frame["delivery_id"]
			_ = wsjson.Write(ctx, c.conn, ack)
		}
	}
}

// waitFrame returns the first frame matching match, waiting up to 3s.
func (c *wsClient) waitFrame(match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, f := range c.frames {
			if match(f) {
				c.mu.Unlock()
				return f
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("frame never arrived; have %+v", c.frames)
	return nil
}

func ofType(typ string) func(map[string]any) bool {
	return func(f map[string]any) bool { return f["type"] == typ }
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 300 {
		t.Fatalf("status %d: %+v", resp.StatusCode, out)
	}
	return out
}

// waitStage polls the ledger for stage on traceID (pushes are asynchronous).
func (s *inboxStack) waitStage(t *testing.T, traceID, stage string) state.TraceRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := s.store.StagesFor(context.Background(), traceID)
		for _, r := range rows {
			if r.Stage == stage {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, _ := s.store.StagesFor(context.Background(), traceID)
	t.Fatalf("%s never recorded %s; have %v", traceID, stage, rows)
	return state.TraceRow{}
}

func telegramSurface(frame map[string]any, n int) map[string]any {
	return map[string]any{"status": "pushed", "surface_ref": fmt.Sprint(1000 + n)}
}

// TestHumanInboxEndToEnd drives the whole loop on the real stack: an agent
// notifies "telegram" and the human's inbox stores it and the Telegram
// surface receipts it; an agent asks the human and a Telegram reply-to acks
// it; the human asks an agent from Telegram and the answer threads back under
// the human's own message.
func TestHumanInboxEndToEnd(t *testing.T) {
	s := newInboxStack(t)
	bridge := s.connect(t, map[string]any{"display_name": "telegram", "circle": "default", "backend": "claude-code",
		"role": "service", "path": "/telegram", "capabilities": []string{proto.CapInboxSurface}}, telegramSurface)
	agent := s.connect(t, map[string]any{"display_name": "alpha", "circle": "1", "backend": "claude-code", "role": "agent", "path": "/work/alpha"}, nil)

	// 1. notify to the legacy address lands in the inbox, with a receipt.
	notified := decodeBody(t, postJSON(t, s.srv.URL+"/notify", map[string]any{"from_peer": string(agent.id), "to_peer": "telegram", "text": "PR is up: https://example.test/1"}))
	itemID, _ := notified["inbox_item_id"].(string)
	if itemID == "" || notified["to_peer_name"] != "prass" || fmt.Sprint(notified["surfaces"]) != "[telegram]" {
		t.Fatalf("notify receipt = %+v", notified)
	}
	pushed := bridge.waitFrame(ofType("inbox_item"))
	if item, _ := pushed["item"].(map[string]any); item["text"] != "PR is up: https://example.test/1" || item["kind"] != "notify" {
		t.Fatalf("surface got %+v", pushed)
	}
	deliveryID := notified["delivery_id"].(string)
	if row := s.waitStage(t, deliveryID, "surface_pushed"); row.Detail["surface"] != "telegram" {
		t.Fatalf("surface_pushed = %+v", row)
	}
	rows, _ := s.store.StagesFor(context.Background(), deliveryID)
	for _, r := range rows {
		raw, _ := json.Marshal(r.Detail)
		if strings.Contains(string(raw), "example.test") {
			t.Fatalf("trace %s carries message text: %s", r.Stage, raw)
		}
	}
	if s.asks.OpenCount() != 0 {
		t.Fatal("a plain notify opened an ask")
	}

	// 2. MCP notify_peer goes through the same traced path.
	res, err := s.h.mcpNotifyPeer(context.Background(), s.mcpDel, mcpNotifyPeerArgs{PeerName: "telegram", Message: "via mcp"}, string(agent.id))
	if err != nil {
		t.Fatal(err)
	}
	out := mcpText(t, res)
	mcpDelivery := strings.Fields(out)[0]
	if !strings.Contains(out, "inbox_item=inb-") || !strings.Contains(out, "surfaces=telegram") {
		t.Fatalf("notify_peer result = %q", out)
	}
	s.waitStage(t, mcpDelivery, "created")
	s.waitStage(t, mcpDelivery, "inbox_stored")

	// 3. An agent asks the human; a Telegram reply-to on that message acks it.
	asked := decodeBody(t, postJSON(t, s.srv.URL+"/ask", map[string]any{"from_peer": string(agent.id), "to_peer": "prass", "text": "merge #12?"}))
	cid := asked["correlation_id"].(string)
	if inbox, _ := asked["inbox"].(map[string]any); inbox["item_id"] == "" {
		t.Fatalf("ask receipt = %+v", asked)
	}
	askFrame := bridge.waitFrame(func(f map[string]any) bool {
		item, _ := f["item"].(map[string]any)
		return item != nil && item["kind"] == "ask"
	})
	askItem := askFrame["item"].(map[string]any)
	if askItem["correlation_id"] != cid {
		t.Fatalf("ask item = %+v", askItem)
	}
	ref := s.waitStage(t, cid, "surface_pushed")
	stored, _ := s.store.InboxItem(context.Background(), ref.Detail["item_id"].(string))
	replied := decodeBody(t, postJSON(t, s.srv.URL+"/inbox/reply", map[string]any{
		"surface": "telegram", "surface_ref": stored.Pushes[0].SurfaceRef, "ref": "2001", "text": "yes, merge"}))
	if replied["action"] != "acked" || replied["correlation_id"] != cid {
		t.Fatalf("reply = %+v", replied)
	}
	agent.waitFrame(func(f map[string]any) bool {
		return f["type"] == "notify" && strings.Contains(fmt.Sprint(f["text"]), "yes, merge")
	})
	if ask, _ := s.asks.Get(cid); !ask.Closed || ask.CloseReason != "ack_with_msg" {
		t.Fatalf("ask after reply = %+v", ask)
	}
	s.waitStage(t, cid, "inbox_replied")

	// 4. The human asks the agent from Telegram; the ack threads under it.
	mine := decodeBody(t, postJSON(t, s.srv.URL+"/ask", map[string]any{"from_peer": "telegram", "to_peer": string(agent.id), "text": "status?",
		"origin": map[string]any{"surface": "telegram", "ref": "77"}}))
	myCID := mine["correlation_id"].(string)
	if ask, _ := s.asks.Get(myCID); ask.FromPeerID != s.human {
		t.Fatalf("an ask typed on telegram must come from the human, got %s", ask.FromPeerID)
	}
	decodeBody(t, postJSON(t, s.srv.URL+"/ack", map[string]any{"correlation_id": myCID, "message": "all green"}))
	answer := bridge.waitFrame(func(f map[string]any) bool {
		item, _ := f["item"].(map[string]any)
		return item != nil && item["kind"] == "answer"
	})
	if answer["reply_to_ref"] != "77" || answer["item"].(map[string]any)["text"] != "all green" {
		t.Fatalf("answer frame = %+v", answer)
	}

	// 5. Read on one surface is read everywhere.
	listed := decodeBody(t, httpGet(t, s.srv.URL+"/inbox?unread=1"))
	before := len(anyList(listed["items"]))
	decodeBody(t, postJSON(t, s.srv.URL+"/inbox/"+itemID+"/read", map[string]any{"surface": "cli"}))
	after := len(anyList(decodeBody(t, httpGet(t, s.srv.URL+"/inbox?unread=1"))["items"]))
	if after != before-1 {
		t.Fatalf("unread %d -> %d after read", before, after)
	}
	s.waitStage(t, notified["delivery_id"].(string), "inbox_read")
}

// TestHumanInboxSurfaceFailureIsRecordedNotFatal: a surface that reports a
// failed push leaves the item stored and a surface_failed trace with only the
// error class; the sender still got its receipt.
func TestHumanInboxSurfaceFailureIsRecordedNotFatal(t *testing.T) {
	s := newInboxStack(t)
	bridge := s.connect(t, map[string]any{"display_name": "telegram", "circle": "default", "backend": "claude-code",
		"role": "service", "path": "/telegram", "capabilities": []string{proto.CapInboxSurface}},
		func(map[string]any, int) map[string]any {
			return map[string]any{"status": "failed", "error_class": "network"}
		})
	agent := s.connect(t, map[string]any{"display_name": "alpha", "circle": "1", "backend": "claude-code", "role": "agent", "path": "/work/alpha"}, nil)
	body := decodeBody(t, postJSON(t, s.srv.URL+"/notify", map[string]any{"from_peer": string(agent.id), "to_peer": "prass", "text": "hello"}))
	bridge.waitFrame(ofType("inbox_item"))
	row := s.waitStage(t, body["delivery_id"].(string), "surface_failed")
	if row.Status != "fail" || row.Detail["error_class"] != "network" {
		t.Fatalf("surface_failed = %+v", row)
	}
	item, _ := s.store.InboxItem(context.Background(), body["inbox_item_id"].(string))
	if item == nil || item.Pushes[0].Status != state.PushFailed {
		t.Fatalf("item = %+v", item)
	}
}

func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func anyList(v any) []any { out, _ := v.([]any); return out }

func mcpText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("tool result = %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T", res.Content[0])
	}
	return text.Text
}
