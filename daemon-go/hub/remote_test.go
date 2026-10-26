package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// meshLink stands in for the relay between in-process test daemons: it runs
// the target's remote handler with the source named as the relay would.
type meshLink struct {
	mu      sync.Mutex
	self    string
	daemons map[string]*remoteDaemon
	extra   []string // machines listed online that never answer
	// lose, when it returns true for a path, drops the request before the
	// target runs it and reports an unknown outcome.
	lose func(path string) bool
}

func (l *meshLink) from(self string) *meshLink {
	return &meshLink{self: self, daemons: l.daemons, extra: l.extra, lose: l.lose}
}

func (l *meshLink) MachineRequest(ctx context.Context, machine, method, path string, body []byte) (int, []byte, error) {
	l.mu.Lock()
	target, lose := l.daemons[machine], l.lose
	l.mu.Unlock()
	if lose != nil && lose(path) {
		return 0, nil, errors.New("relay connection lost")
	}
	if target == nil {
		return http.StatusNotFound, []byte(`{"detail":"machine offline"}`), nil
	}
	req := httptest.NewRequestWithContext(relay.WithRemoteMachine(ctx, l.self), method, path, bytes.NewReader(body))
	w := httptest.NewRecorder()
	target.h.RemoteHandler().ServeHTTP(w, req)
	return w.Code, w.Body.Bytes(), nil
}

func (l *meshLink) Machines(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]string{}, l.extra...)
	for id := range l.daemons {
		if id != l.self {
			out = append(out, id)
		}
	}
	return out, nil
}

type remoteDaemon struct {
	id    string
	db    string
	h     *Hub
	srv   *httptest.Server
	store *state.Store
	asks  *service.AskTracker
}

func newRemoteDaemon(t *testing.T, mesh *meshLink, id, dbPath string) *remoteDaemon {
	t.Helper()
	ctx := context.Background()
	store, err := state.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(ctx, store, deadLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "")
	asks := service.NewAskTracker(0)
	delivery := service.NewPeerDelivery(reg, h.Router(), transport, asks, nil)
	h.WithReadDeps(asks, store).WithAskLifecycle(asks, delivery, reg).WithMessaging(delivery, store)
	if err := h.WithRemote(ctx, mesh.from(id), id); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	d := &remoteDaemon{id: id, db: dbPath, h: h, srv: httptest.NewServer(mux), store: store, asks: asks}
	mesh.mu.Lock()
	mesh.daemons[id] = d
	mesh.mu.Unlock()
	t.Cleanup(func() { d.close() })
	return d
}

func (d *remoteDaemon) close() {
	d.srv.Close()
	d.h.reg.Close()
	_ = d.store.Close()
}

func (d *remoteDaemon) post(t *testing.T, path string, body any) (int, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	res, err := http.Post(d.srv.URL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

type testAgent struct {
	id     proto.PeerID
	name   string // the display name the registry gave it
	conn   *websocket.Conn
	frames chan map[string]any
}

func (d *remoteDaemon) agent(t *testing.T, name, circle string, claim *proto.PeerID) *testAgent {
	t.Helper()
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(d.srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: proto.DisplayName(name), Circle: circle, Backend: proto.AgentClaudeCode, Role: proto.RoleAgent, PeerID: claim, Path: strp("/work/" + name)}); err != nil {
		t.Fatal(err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, conn, &connected); err != nil || connected.Type != proto.FrameConnected {
		t.Fatalf("connect %s: %v %+v", name, err, connected)
	}
	a := &testAgent{id: connected.SessionID, conn: conn, frames: make(chan map[string]any, 16)}
	go func() {
		for {
			var frame map[string]any
			if wsjson.Read(ctx, conn, &frame) != nil {
				return
			}
			switch frame["type"] {
			case "ping":
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "pong"})
			default:
				// Acknowledge delivery like a hook would, then surface it.
				if id, ok := frame["delivery_id"].(string); ok && id != "" {
					_ = wsjson.Write(ctx, conn, map[string]any{"type": "delivery_ack", "delivery_id": id, "status": "delivered"})
				}
				a.frames <- frame
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !d.h.transport.IsConnected(a.id) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p, ok := d.h.reg.GetPeer(a.id); ok {
		a.name = string(p.DisplayName)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return a
}

// next returns the next frame whose text contains want.
func (a *testAgent) next(t *testing.T, want string) map[string]any {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case frame := <-a.frames:
			if strings.Contains(fmt.Sprint(frame["text"]), want) {
				return frame
			}
		case <-timeout:
			t.Fatalf("no frame containing %q", want)
		}
	}
}

func (a *testAgent) quiet(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case frame := <-a.frames:
		t.Fatalf("unexpected frame %v", frame)
	case <-time.After(within):
	}
}

func newMesh() *meshLink { return &meshLink{daemons: map[string]*remoteDaemon{}} }

func twoMachines(t *testing.T) (*meshLink, *remoteDaemon, *remoteDaemon) {
	mesh := newMesh()
	dir := t.TempDir()
	return mesh, newRemoteDaemon(t, mesh, "alpha", filepath.Join(dir, "alpha.db")), newRemoteDaemon(t, mesh, "beta", filepath.Join(dir, "beta.db"))
}

func TestRemoteAskRoundTrip(t *testing.T) {
	_, alpha, beta := twoMachines(t)
	asker := alpha.agent(t, "asker", "proj", nil)
	bob := beta.agent(t, "bob", "proj", nil)
	carol := beta.agent(t, "carol", "proj", nil)

	status, sent := alpha.post(t, "/ask", map[string]any{"from_peer": string(asker.id), "to_peer": bob.name + "@beta", "text": "what is 6*7?"})
	if status != 200 || sent["error"] != nil {
		t.Fatalf("ask %d %v", status, sent)
	}
	cid := sent["correlation_id"].(string)
	frame := bob.next(t, "what is 6*7?")
	if frame["correlation_id"] != cid || frame["from_peer"] != asker.name+"@alpha" {
		t.Fatalf("bob got %v", frame)
	}

	// Only the recipient settles it, and the source's projection is not
	// settled locally.
	if status, _ := beta.post(t, "/ack", map[string]any{"correlation_id": cid, "message": "nope", "from_peer": string(carol.id)}); status != http.StatusForbidden {
		t.Fatalf("carol ack %d", status)
	}
	if status, _ := alpha.post(t, "/ack", map[string]any{"correlation_id": cid, "message": "nope", "from_peer": string(asker.id)}); status != http.StatusForbidden {
		t.Fatalf("local ack of the projection %d", status)
	}

	if status, body := beta.post(t, "/ack", map[string]any{"correlation_id": cid, "message": "42", "from_peer": string(bob.id)}); status != 200 {
		t.Fatalf("bob ack %d %v", status, body)
	}
	reply := asker.next(t, "] 42")
	if !strings.Contains(fmt.Sprint(reply["text"]), "[ack #"+cid+" from @"+bob.name+"@beta]") {
		t.Fatalf("reply %v", reply)
	}
	// The frame arrives before the delivery ack completes the bookkeeping.
	eventually(t, func() bool {
		row, _ := alpha.store.GetOutbound(context.Background(), cid)
		ask, _ := alpha.asks.Get(cid)
		return row.State == state.RemoteSettled && row.Delivered && row.Reply != nil && *row.Reply == "42" && ask != nil && ask.Closed
	})
}

func TestRemoteCircleRules(t *testing.T) {
	_, alpha, beta := twoMachines(t)
	asker := alpha.agent(t, "asker", "proj", nil)
	local := alpha.agent(t, "local", "3", nil)
	carol := beta.agent(t, "carol", "1", nil)
	dave := beta.agent(t, "dave", "other", nil)

	for _, to := range []string{carol.name + "@beta", dave.name + "@beta", "nobody@beta"} {
		if status, _ := alpha.post(t, "/ask", map[string]any{"from_peer": string(asker.id), "to_peer": to, "text": "hi"}); status != http.StatusNotFound {
			t.Fatalf("%s: %d", to, status)
		}
	}
	erin := beta.agent(t, "erin", "3", nil)
	if status, _ := alpha.post(t, "/ask", map[string]any{"from_peer": string(local.id), "to_peer": erin.name + "@beta", "text": "hi"}); status != http.StatusForbidden {
		t.Fatalf("local circle crossed: %d", status)
	}
}

// A lost settlement push is recovered by wait_on_ack reading the target.
func TestRemoteWaitRecoversALostPush(t *testing.T) {
	mesh, alpha, beta := twoMachines(t)
	asker := alpha.agent(t, "asker", "proj", nil)
	bob := beta.agent(t, "bob", "proj", nil)
	mesh.mu.Lock()
	mesh.lose = func(path string) bool { return path == "/remote/settled" }
	mesh.mu.Unlock()
	alpha.h.remote.link.(*meshLink).lose = mesh.lose
	beta.h.remote.link.(*meshLink).lose = mesh.lose

	_, sent := alpha.post(t, "/ask", map[string]any{"from_peer": string(asker.id), "to_peer": bob.name + "@beta", "text": "q"})
	cid := sent["correlation_id"].(string)
	bob.next(t, "q")
	if _, wait := alpha.post(t, "/asks/"+cid+"/wait", map[string]any{"peer_id": string(asker.id), "timeout_seconds": 0.2}); wait["status"] != "pending" {
		t.Fatalf("wait before ack %v", wait)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		beta.post(t, "/ack", map[string]any{"correlation_id": cid, "message": "late", "from_peer": string(bob.id)})
	}()
	_, wait := alpha.post(t, "/asks/"+cid+"/wait", map[string]any{"peer_id": string(asker.id), "timeout_seconds": 5})
	if wait["status"] != "resolved" || wait["reply"] != "late" || wait["responder"] != bob.name+"@beta" {
		t.Fatalf("wait %v", wait)
	}
	asker.quiet(t, 200*time.Millisecond) // the waiter took the reply; nothing is pushed
	// A lost wait response loses nothing: waiting again answers from the row.
	if _, again := alpha.post(t, "/asks/"+cid+"/wait", map[string]any{"peer_id": string(asker.id), "timeout_seconds": 0}); again["reply"] != "late" {
		t.Fatalf("second wait %v", again)
	}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The MCP ask result carries an uncertain send's warning, not just its id.
func TestAskToolResultReportsUncertainDelivery(t *testing.T) {
	detail := "delivery to bob@beta is uncertain"
	if got := askToolResult(AskResponse{CorrelationID: "ask-1", Error: &detail}); !strings.Contains(got, "ask-1 delivery_state=uncertain") || !strings.Contains(got, detail) {
		t.Fatalf("result %q", got)
	}
}

func TestAmbiguousSendIsOnlyATimeoutOrCancel(t *testing.T) {
	for err, want := range map[error]bool{
		context.DeadlineExceeded:                            true,
		fmt.Errorf("ack: %w", context.Canceled):             true,
		service.ErrNotConnected:                             false,
		errors.New("Unknown peer: x"):                       false,
		&service.DeliveryInjectionError{Status: "rejected"}: false,
	} {
		if ambiguousSend(err) != want {
			t.Errorf("ambiguousSend(%v) = %v", err, !want)
		}
	}
}

// A create whose outcome is unknown stays uncertain; the next check fences
// it on the target, and the late create then dispatches nothing.
func TestRemoteUncertainCreateIsFencedNotReplayed(t *testing.T) {
	_, alpha, beta := twoMachines(t)
	asker := alpha.agent(t, "asker", "proj", nil)
	bob := beta.agent(t, "bob", "proj", nil)
	var lost []byte
	alpha.h.remote.link.(*meshLink).lose = func(path string) bool { return path == "/remote/ask" }

	status, sent := alpha.post(t, "/ask", map[string]any{"from_peer": string(asker.id), "to_peer": bob.name + "@beta", "text": "maybe"})
	if status != 200 || sent["error"] == nil || !strings.Contains(sent["error"].(string), "uncertain") {
		t.Fatalf("uncertain ask %d %v", status, sent)
	}
	cid := sent["correlation_id"].(string)
	row, _ := alpha.store.GetOutbound(context.Background(), cid)
	if row.State != state.RemoteUncertain {
		t.Fatalf("row %+v", row)
	}
	lost, _ = json.Marshal(remoteCreateRequest{ID: cid, From: row.From, ToPeer: row.ToPeerID, Text: row.Text, ExpectedEpoch: row.StoreEpoch})

	// The asker's Stop hook lists its asks; that request reconciles.
	if res := httpGet(t, alpha.srv.URL+"/asks/pending?peer_id="+string(asker.id)); res.StatusCode != 200 {
		t.Fatalf("pending %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	asker.next(t, "not_delivered")
	eventually(t, func() bool {
		row, _ = alpha.store.GetOutbound(context.Background(), cid)
		return row.CloseReason == "not_delivered" && row.Delivered
	})

	// The lost create arrives late: it finds the fence.
	req := httptest.NewRequestWithContext(relay.WithRemoteMachine(context.Background(), "alpha"), http.MethodPost, "/remote/ask", bytes.NewReader(lost))
	w := httptest.NewRecorder()
	beta.h.RemoteHandler().ServeHTTP(w, req)
	var receipt remoteReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	if receipt.State != state.RemoteSettled || receipt.CloseReason != "not_delivered" {
		t.Fatalf("late create %d %s", w.Code, w.Body.String())
	}
	bob.quiet(t, 200*time.Millisecond)
}

// The target restarts with the ask open: the ask comes back with its age,
// and the recipient's ack still settles it on the source.
func TestRemoteTargetRestartKeepsTheAsk(t *testing.T) {
	mesh, alpha, beta := twoMachines(t)
	asker := alpha.agent(t, "asker", "proj", nil)
	bob := beta.agent(t, "bob", "proj", nil)
	_, sent := alpha.post(t, "/ask", map[string]any{"from_peer": string(asker.id), "to_peer": bob.name + "@beta", "text": "survive"})
	cid := sent["correlation_id"].(string)
	bob.next(t, "survive")
	before, _ := beta.asks.Get(cid)

	dbPath := beta.db
	_ = bob.conn.CloseNow()
	beta.close()
	beta = newRemoteDaemon(t, mesh, "beta", dbPath)
	after, ok := beta.asks.Get(cid)
	if !ok || after.Closed || !after.CreatedAt.Equal(before.CreatedAt.Truncate(time.Second)) {
		t.Fatalf("rehydrated %+v (was %v)", after, before.CreatedAt)
	}
	bob = beta.agent(t, "bob", "proj", &bob.id)
	if status, body := beta.post(t, "/ack", map[string]any{"correlation_id": cid, "message": "back", "from_peer": string(bob.id)}); status != 200 {
		t.Fatalf("ack after restart %d %v", status, body)
	}
	asker.next(t, "] back")
}

func TestRemoteDiscoveryNotifyAndBroadcast(t *testing.T) {
	mesh, alpha, beta := twoMachines(t)
	mesh.extra = []string{"gamma"}
	alpha.h.remote.link.(*meshLink).extra = mesh.extra
	asker := alpha.agent(t, "asker", "proj", nil)
	bob := beta.agent(t, "bob", "proj", nil)
	beta.agent(t, "carol", "1", nil)

	body := mcpText(t, alpha.h.mcpListPeers(context.Background(), mcpListPeersArgs{}, string(asker.id)))
	if !strings.Contains(body, bob.name+"@beta") || strings.Contains(body, "carol") || !strings.Contains(body, "# partial: gamma") {
		t.Fatalf("listing:\n%s", body)
	}

	id, delivery, err := alpha.h.remoteNotify(context.Background(), string(asker.id), bob.name, "beta", "fyi")
	if err != nil || delivery != "delivered" {
		t.Fatalf("notify %s %s %v", id, delivery, err)
	}
	bob.next(t, "fyi")

	result, err := alpha.h.mcpBroadcast(context.Background(), alpha.h.ask.delivery, mcpBroadcastArgs{Message: "all hands"}, string(asker.id))
	if err != nil || !strings.Contains(mcpText(t, result), "remote_sent=1") || !strings.Contains(mcpText(t, result), "partial=gamma") {
		t.Fatalf("broadcast %v %v", mcpText(t, result), err)
	}
	bob.next(t, "all hands")
}

func TestRemoteCreateIsIdempotentAndEpochBound(t *testing.T) {
	_, alpha, beta := twoMachines(t)
	_ = alpha
	bob := beta.agent(t, "bob", "proj", nil)
	from := state.RemotePrincipal{PeerID: "repow-x", DisplayName: "asker", Circle: "proj"}
	call := func(body remoteCreateRequest) (int, remoteReceipt) {
		encoded, _ := json.Marshal(body)
		req := httptest.NewRequestWithContext(relay.WithRemoteMachine(context.Background(), "alpha"), http.MethodPost, "/remote/notify", bytes.NewReader(encoded))
		w := httptest.NewRecorder()
		beta.h.RemoteHandler().ServeHTTP(w, req)
		var receipt remoteReceipt
		_ = json.Unmarshal(w.Body.Bytes(), &receipt)
		return w.Code, receipt
	}
	id := newRemoteID("notif")
	body := remoteCreateRequest{ID: id, From: from, ToPeer: string(bob.id), Text: "once", ExpectedEpoch: beta.h.remote.epoch}
	if status, receipt := call(body); status != 200 || receipt.State != state.RemoteDelivered {
		t.Fatalf("first %d %+v", status, receipt)
	}
	bob.next(t, "once")
	if status, receipt := call(body); status != 200 || receipt.State != state.RemoteDelivered {
		t.Fatalf("retry %d %+v", status, receipt)
	}
	bob.quiet(t, 200*time.Millisecond)

	body.ID, body.ExpectedEpoch = newRemoteID("notif"), "other-epoch"
	if status, _ := call(body); status != http.StatusConflict {
		t.Fatalf("epoch mismatch %d", status)
	}
	body.ID, body.ExpectedEpoch = fmt.Sprintf("notif-%d-%032x", time.Now().Add(-7*24*time.Hour).Unix(), 1), beta.h.remote.epoch
	if status, _ := call(body); status != http.StatusBadRequest {
		t.Fatalf("stale id %d", status)
	}
	// The handler is only for the relay: no remote machine, no service.
	encoded, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	beta.h.RemoteHandler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/remote/notify", bytes.NewReader(encoded)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("direct call %d", w.Code)
	}
}

// A notify caught mid-dispatch by a restart is never dispatched again.
func TestRemoteRestartSettlesADispatchingNotifyUncertain(t *testing.T) {
	mesh, _, beta := twoMachines(t)
	id := newRemoteID("notif")
	_, _, err := beta.store.CreateInbound(context.Background(), state.RemoteInbound{ID: id, Kind: "notify", FromMachine: "alpha", From: state.RemotePrincipal{PeerID: "p", DisplayName: "asker", Circle: "proj"}, ToPeerID: "t", Text: "x", CreatedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := beta.db
	beta.close()
	beta = newRemoteDaemon(t, mesh, "beta", dbPath)
	got, _ := beta.store.GetInbound(context.Background(), id)
	if got.State != state.RemoteSettled || got.CloseReason != "uncertain" {
		t.Fatalf("after restart %+v", got)
	}
}

// A replaced target database, or a result past retention, reads as lost,
// never as not delivered.
func TestRemoteLostResultsAreNotNonDelivery(t *testing.T) {
	_, alpha, beta := twoMachines(t)
	asker := alpha.agent(t, "asker", "proj", nil)
	bob := beta.agent(t, "bob", "proj", nil)
	from := state.RemotePrincipal{PeerID: string(asker.id), DisplayName: asker.name, Circle: "proj"}
	ctx := context.Background()
	for name, row := range map[string]state.RemoteOutbound{
		"replaced": {ID: newRemoteID("ask"), StoreEpoch: "an-older-database"},
		"expired":  {ID: fmt.Sprintf("ask-%d-%032x", time.Now().Add(-6*24*time.Hour-time.Hour).Unix(), 7), StoreEpoch: beta.h.remote.epoch},
	} {
		row.Machine, row.ToPeerID, row.ToName, row.From, row.Text, row.CreatedAt = "beta", string(bob.id), bob.name+"@beta", from, "q", time.Now().Unix()
		if err := alpha.store.InsertOutbound(ctx, row); err != nil {
			t.Fatal(err)
		}
		alpha.h.readOutbound(ctx, row, 0, false)
		got, _ := alpha.store.GetOutbound(ctx, row.ID)
		if got.CloseReason != "result_lost" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
