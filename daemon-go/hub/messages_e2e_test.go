package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// e2ePeer is a connected peer that acks frames the way the ws-hook does and
// calls MCP tools under its proven identity.
type e2ePeer struct {
	id     proto.PeerID
	name   string
	proof  string
	mu     sync.Mutex
	frames []map[string]any
}

func (p *e2ePeer) received() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.frames...)
}

// waitFrame returns the first received frame of typ matching keep.
func (p *e2ePeer) waitFrame(t *testing.T, typ string, keep func(map[string]any) bool) map[string]any {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		for _, f := range p.received() {
			if f["type"] == typ && keep(f) {
				return f
			}
		}
	}
	t.Fatalf("%s never received a matching %s frame; got %v", p.name, typ, p.received())
	return nil
}

func TestMessagesAndDelegationEndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t)
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.store = store
	asks := service.NewAskTracker(0)
	messages := service.NewMessages(store)
	delivery := service.NewPeerDelivery(h.reg, h.Router(), h.Transport(), asks, store).WithMessages(messages)
	h.WithReadDeps(asks, store).WithMessages(messages).WithAskLifecycle(asks, delivery, h.reg).
		WithMCP(config.MCPHTTPConfig{Enabled: true, AllowUnauthenticatedLocalhost: true}, delivery)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	connect := func(name string) *e2ePeer {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		path := "/work/" + name
		if err := wsjson.Write(dialCtx, conn, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: proto.DisplayName(name),
			Circle: "default", Backend: proto.AgentClaudeCode, Role: proto.RoleAgent, Path: &path}); err != nil {
			t.Fatal(err)
		}
		var connected proto.ConnectedFrame
		if err := wsjson.Read(dialCtx, conn, &connected); err != nil || connected.Type != proto.FrameConnected {
			t.Fatalf("connect %s: %+v %v", name, connected, err)
		}
		cert, err := store.MintBirthCertificate(ctx, string(connected.SessionID), string(connected.DisplayName), "claude-code", &path, nil, nil, nil, nil, nil, 0, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		p := &e2ePeer{id: connected.SessionID, name: string(connected.DisplayName), proof: cert.Nonce}
		go func() {
			for {
				var frame map[string]any
				if err := wsjson.Read(ctx, conn, &frame); err != nil {
					return
				}
				if typ, _ := frame["type"].(string); typ == "ask" || typ == "notify" {
					_ = wsjson.Write(ctx, conn, map[string]any{"type": "delivery_ack", "delivery_id": frame["delivery_id"], "message_type": typ, "status": "accepted"})
				}
				p.mu.Lock()
				p.frames = append(p.frames, frame)
				p.mu.Unlock()
			}
		}()
		for deadline := time.Now().Add(2 * time.Second); !h.transport.IsConnected(p.id) && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		return p
	}
	call := func(p *e2ePeer, tool string, args map[string]any) (string, bool) {
		t.Helper()
		_, env := mcpRPC(t, srv.URL+"/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args}},
			map[string]string{"X-Repowire-Peer": string(p.id), "X-Repowire-Identity-Proof": p.proof})
		var result mcpToolCallResult
		decodeResult(t, env, &result)
		if len(result.Content) != 1 {
			t.Fatalf("%s: %+v", tool, result)
		}
		return result.Content[0].Text, result.IsError
	}
	mustCall := func(p *e2ePeer, tool string, args map[string]any) string {
		t.Helper()
		text, isErr := call(p, tool, args)
		if isErr {
			t.Fatalf("%s as %s failed: %s", tool, p.name, text)
		}
		return text
	}

	alpha, beta, gamma := connect("alpha"), connect("beta"), connect("gamma")
	mustCall(gamma, "set_message_delivery", map[string]any{"mode": "pull"})

	cid := mustCall(alpha, "ask", map[string]any{"peer_name": beta.name, "query": "Review the parser change?\nThe diff touches the tokenizer."})
	toBeta := beta.waitFrame(t, "ask", func(f map[string]any) bool { return f["correlation_id"] == cid })
	if id, _ := toBeta["message_id"].(string); !strings.HasPrefix(id, "msg-") || !strings.Contains(toBeta["text"].(string), "The diff touches the tokenizer.") {
		t.Fatalf("beta's push frame = %v", toBeta)
	}

	if out := mustCall(beta, "delegate", map[string]any{"correlation_id": cid, "peer_name": gamma.name, "note": "owns the tokenizer"}); !strings.Contains(out, "to @"+gamma.name) {
		t.Fatalf("delegate result = %q", out)
	}
	toGamma := gamma.waitFrame(t, "ask", func(f map[string]any) bool { return f["correlation_id"] == cid })
	gammaMsg, _ := toGamma["message_id"].(string)
	nudge, _ := toGamma["text"].(string)
	if !strings.HasPrefix(nudge, "[message "+gammaMsg+": ask from @"+beta.name) || strings.Contains(nudge, "tokenizer.") || !strings.Contains(nudge, `ack("`+cid+`")`) {
		t.Fatalf("gamma's pull frame = %v", toGamma)
	}

	var listed MessageListResponse
	if err := json.Unmarshal([]byte(mustCall(gamma, "list_messages", map[string]any{"unread_only": true})), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Delivery != "pull" || len(listed.Messages) != 1 || listed.Messages[0].MessageID != gammaMsg || listed.Messages[0].Text != nil ||
		listed.Messages[0].Preview != "[delegated by @"+beta.name+" for @"+alpha.name+": owns the tokenizer]" {
		t.Fatalf("gamma's list = %+v", listed)
	}
	if body, _ := call(alpha, "read_message", map[string]any{"message_id": gammaMsg}); !strings.Contains(body, "No message with id") {
		t.Fatalf("alpha read gamma's message: %q", body)
	}
	body := mustCall(gamma, "read_message", map[string]any{"message_id": gammaMsg, "peek": true})
	if !strings.HasPrefix(body, `<peer-message from="@`+beta.name+`" to="@`+gamma.name+`" type="ask" correlation-id="`+cid+`" message-id="`+gammaMsg+`">`) ||
		!strings.Contains(body, "The diff touches the tokenizer.") {
		t.Fatalf("gamma's fetched body = %q", body)
	}
	if err := json.Unmarshal([]byte(mustCall(gamma, "list_messages", map[string]any{"unread_only": true})), &listed); err != nil || len(listed.Messages) != 1 {
		t.Fatalf("a peek marked the message read: %+v %v", listed, err)
	}
	mustCall(gamma, "read_message", map[string]any{"message_id": gammaMsg})
	if err := json.Unmarshal([]byte(mustCall(gamma, "list_messages", map[string]any{"unread_only": true})), &listed); err != nil || len(listed.Messages) != 0 {
		t.Fatalf("a fetch left the message unread: %+v %v", listed, err)
	}

	if out, isErr := call(beta, "ack", map[string]any{"correlation_id": cid, "message": "mine after all"}); !isErr || !strings.Contains(out, "delegated") {
		t.Fatalf("former holder's ack: %q (error=%v)", out, isErr)
	}
	mustCall(gamma, "ack", map[string]any{"correlation_id": cid, "message": "tokenizer change looks right"})
	reply := alpha.waitFrame(t, "notify", func(f map[string]any) bool { return strings.Contains(f["text"].(string), "#"+cid) })
	if reply["text"] != "[ack #"+cid+" from @"+gamma.name+" via @"+beta.name+"] tokenizer change looks right" {
		t.Fatalf("alpha's reply = %v", reply)
	}
	time.Sleep(200 * time.Millisecond)
	replies := 0
	for _, f := range alpha.received() {
		if text, _ := f["text"].(string); strings.Contains(text, "#"+cid) {
			replies++
		}
	}
	if replies != 1 {
		t.Fatalf("alpha received %d replies on #%s, want 1", replies, cid)
	}
	var alphaList MessageListResponse
	if err := json.Unmarshal([]byte(mustCall(alpha, "list_messages", nil)), &alphaList); err != nil {
		t.Fatal(err)
	}
	if len(alphaList.Messages) != 1 || alphaList.Messages[0].Kind != state.MessageReply || alphaList.Messages[0].CorrelationID != cid || alphaList.Messages[0].ReadAt == nil {
		t.Fatalf("alpha's list = %+v (a confirmed push is read)", alphaList)
	}
}

func TestMessageHTTPRoutes(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t)
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	messages := service.NewMessages(store)
	h.WithMessages(messages)
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	path := "/work/beta"
	id, name, err := h.reg.AllocateAndRegister(ctx, peer.AllocateParams{Circle: "1", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &path})
	if err != nil {
		t.Fatal(err)
	}
	msg := messages.Record(ctx, state.PeerMessage{Kind: state.MessageNotify, PeerID: string(id), PeerName: string(name), FromName: "alpha", Text: "first line\nsecond"})
	if msg == nil {
		t.Fatal("record failed")
	}
	getJSON := func(method, url string, body any, out any) int {
		t.Helper()
		var resp *http.Response
		if method == http.MethodGet {
			resp, err = http.Get(url)
		} else {
			resp = postJSON(t, url, body)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}

	var list MessageListResponse
	if status := getJSON(http.MethodGet, srv.URL+"/messages?peer="+string(name)+"&unread=1", nil, &list); status != http.StatusOK ||
		len(list.Messages) != 1 || list.Messages[0].Preview != "first line" || list.Messages[0].Chars != 17 || list.Delivery != "push" {
		t.Fatalf("list: %d %+v", status, list)
	}
	var view MessageView
	if status := getJSON(http.MethodGet, srv.URL+"/messages/"+msg.ID, nil, &view); status != http.StatusOK || view.Text == nil || *view.Text != "first line\nsecond" || view.ReadAt != nil {
		t.Fatalf("peek: %d %+v", status, view)
	}
	if status := getJSON(http.MethodPost, srv.URL+"/messages/"+msg.ID+"/read", map[string]any{}, &view); status != http.StatusOK || view.ReadAt == nil {
		t.Fatalf("fetch: %d %+v", status, view)
	}
	otherPath := "/work/other"
	_, otherName, err := h.reg.AllocateAndRegister(ctx, peer.AllocateParams{Circle: "1", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &otherPath})
	if err != nil {
		t.Fatal(err)
	}
	if status := getJSON(http.MethodGet, srv.URL+"/messages/"+msg.ID+"?peer="+string(otherName), nil, nil); status != http.StatusNotFound {
		t.Fatalf("another peer read the message: %d", status)
	}
	if status := getJSON(http.MethodGet, srv.URL+"/messages/"+msg.ID+"?peer="+string(name), nil, nil); status != http.StatusOK {
		t.Fatalf("the recipient's scoped read: %d", status)
	}
	if status := getJSON(http.MethodGet, srv.URL+"/messages/msg-missing", nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown message: %d", status)
	}
	if status := getJSON(http.MethodGet, srv.URL+"/messages", nil, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("list without a peer: %d", status)
	}
	if status := getJSON(http.MethodPost, srv.URL+"/peers/"+string(name)+"/message-delivery", map[string]any{"mode": "sideways"}, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("bad mode: %d", status)
	}
	if status := getJSON(http.MethodPost, srv.URL+"/peers/"+string(name)+"/message-delivery", map[string]any{"mode": "pull"}, nil); status != http.StatusOK || messages.Mode(ctx, id) != "pull" {
		t.Fatalf("set pull: %d", status)
	}
}
