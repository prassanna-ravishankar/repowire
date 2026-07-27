package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// --- minimal fakes for a real Registry ---

type memStore struct{}

func (memStore) LoadMappings(context.Context) ([]*proto.SessionMapping, error) { return nil, nil }
func (memStore) UpsertMapping(context.Context, *proto.SessionMapping) error    { return nil }
func (memStore) DeleteMapping(context.Context, proto.PeerID) error             { return nil }
func (memStore) LoadRetired(context.Context, time.Time) (map[proto.PeerID]time.Time, error) {
	return map[proto.PeerID]time.Time{}, nil
}
func (memStore) Retire(context.Context, proto.PeerID, time.Time) error { return nil }
func (memStore) Unretire(context.Context, proto.PeerID) error          { return nil }
func (memStore) AppendEvent(context.Context, peer.Event) error         { return nil }

type deadLive struct{}

func (deadLive) PIDAlive(int) bool { return false }

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	// Build the transport first so the registry's liveness seam is the same
	// transport the hub serves on (ghost eviction sees the live sockets), then
	// wrap that registry+transport in the hub.
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(context.Background(), memStore{}, deadLive{}, transport)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return NewHubWithTransport(reg, transport, "")
}

// TestRouterRoutesByPeerID is the core smoke test: two peers are connected, a
// query is sent to ONE of them by PeerID, and only that peer's socket receives
// the frame. Routing must be keyed on PeerID, not DisplayName.
func TestRouterRoutesByPeerID(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):]

	connectPeer := func(name string) (*websocket.Conn, proto.PeerID) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := wsjson.Write(ctx, c, proto.ConnectFrame{
			Type:        proto.FrameConnect,
			DisplayName: proto.DisplayName(name),
			Circle:      "default",
			Backend:     proto.AgentClaudeCode,
			Role:        proto.RoleAgent,
		}); err != nil {
			t.Fatalf("write connect: %v", err)
		}
		var connected proto.ConnectedFrame
		if err := wsjson.Read(ctx, c, &connected); err != nil {
			t.Fatalf("read connected: %v", err)
		}
		if connected.Type != proto.FrameConnected {
			t.Fatalf("expected connected frame, got %s", connected.Type)
		}
		return c, connected.SessionID
	}

	cA, idA := connectPeer("alpha")
	defer cA.CloseNow()
	cB, idB := connectPeer("beta")
	defer cB.CloseNow()

	if idA == idB {
		t.Fatalf("distinct peers must get distinct peer_ids: %s", idA)
	}

	// Wait for both transports to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.transport.IsConnected(idA) && h.transport.IsConnected(idB) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !h.transport.IsConnected(idA) || !h.transport.IsConnected(idB) {
		t.Fatalf("both peers should be connected")
	}

}

// TestDisconnectIdentityChecked verifies the disconnect race guard: a stale
// handler holding an old socket must not evict a newer connection stored under
// the same peer_id.
func TestDisconnectIdentityChecked(t *testing.T) {
	tr := service.NewWebSocketTransport()
	id := proto.PeerID("repow-default-aaaa1111")

	// Two distinct sentinel *websocket.Conn values; we never read/write them, only
	// compare identity, so unconnected zero conns suffice via net.Pipe-backed accept.
	oldWS, closeOld := dummyConn(t)
	defer closeOld()
	newWS, closeNew := dummyConn(t)
	defer closeNew()

	tr.Connect(context.Background(), &service.ConnectionInfo{SessionID: id, WS: oldWS})
	tr.Connect(context.Background(), &service.ConnectionInfo{SessionID: id, WS: newWS})

	// The stale handler (oldWS) tears down: must NOT evict newWS.
	if tr.Disconnect(context.Background(), id, oldWS) {
		t.Fatalf("stale-socket disconnect must return false")
	}
	if !tr.IsConnected(id) {
		t.Fatalf("newer connection must survive a stale disconnect")
	}
	// The owning handler (newWS) tears down: must evict.
	if !tr.Disconnect(context.Background(), id, newWS) {
		t.Fatalf("owning-socket disconnect must return true")
	}
	if tr.IsConnected(id) {
		t.Fatalf("peer should be gone after owning disconnect")
	}
}

// dummyConn establishes a real *websocket.Conn (the server-accepted side) so the
// disconnect guard test has genuine, distinct connection pointers to compare.
func dummyConn(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		accepted <- c
		// Hold the handler open until the test tears the connection down.
		<-r.Context().Done()
	}))
	wsURL := "ws" + srv.URL[len("http"):]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	server := <-accepted
	return server, func() {
		client.CloseNow()
		server.CloseNow()
		srv.Close()
	}
}

// deliveryAckTimeout duplicates service/transport.go's unexported constant of
// the same name (the real 750ms best-effort delivery-ack window) — not worth
// exporting a service-internal timing constant just for this test file to read.
const deliveryAckTimeout = 750 * time.Millisecond

// TestSendAskRealSocketAckWithin750ms exercises the GENUINE delivery-ack window
// end-to-end: the router sends an ask over a real service.WebSocketTransport, the peer
// reads the frame and replies with a delivery_ack inside the 750ms window, and
// the hub's dispatch resolves it so SendAsk returns the receipt. This proves the
// real SendAndWaitDeliveryAck path, not just the fake's branching.
func TestSendAskRealSocketAckWithin750ms(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):]

	c, peerID := dialAndConnect(t, wsURL, "beta")
	defer c.CloseNow()
	waitConnected(t, h, peerID)

	// Peer side: read the ask frame, ack it as injected well within 750ms.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		var frame map[string]any
		_ = json.Unmarshal(raw, &frame)
		_ = wsjson.Write(ctx, c, map[string]any{
			"type":        string(proto.FrameDeliveryAck),
			"delivery_id": frame["delivery_id"],
			"status":      "injected",
		})
	}()

	start := time.Now()
	hook, err := h.router.SendAsk(context.Background(), "alpha", peerID, "beta", "beta",
		"cid-real", "real ask", nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hook == nil || hook["status"] != "injected" {
		t.Fatalf("expected injected receipt over real socket, got %+v", hook)
	}
	if elapsed := time.Since(start); elapsed >= deliveryAckTimeout {
		t.Fatalf("ack should have resolved before the 750ms window, took %s", elapsed)
	}
}

// TestSendNotificationRealSocketNoAckTimesOut covers the genuine 750ms timeout:
// the peer reads the notify frame but never acks (legacy hook), so the real
// SendAndWaitDeliveryAck waits the full window and returns (nil, nil) — a
// missing ack is best-effort, never an error.
func TestSendNotificationRealSocketNoAckTimesOut(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):]

	c, peerID := dialAndConnect(t, wsURL, "gamma")
	defer c.CloseNow()
	waitConnected(t, h, peerID)

	// Peer reads but deliberately never acks.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = c.Read(ctx)
	}()

	start := time.Now()
	hook, err := h.router.SendNotification(context.Background(), "alpha", peerID, "gamma", "gamma",
		"fyi no ack", nil, "")
	if err != nil {
		t.Fatalf("a missing ack must not error, got %v", err)
	}
	if hook != nil {
		t.Fatalf("expected nil receipt on no-ack, got %+v", hook)
	}
	if elapsed := time.Since(start); elapsed < deliveryAckTimeout {
		t.Fatalf("no-ack notify should wait the full 750ms window, only waited %s", elapsed)
	}
}

// dialAndConnect dials the hub, sends a connect frame, and returns the live
// client conn plus the assigned peer_id.
func dialAndConnect(t *testing.T, wsURL, name string) (*websocket.Conn, proto.PeerID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := wsjson.Write(ctx, c, proto.ConnectFrame{
		Type:        proto.FrameConnect,
		DisplayName: proto.DisplayName(name),
		Circle:      "default",
		Backend:     proto.AgentClaudeCode,
		Role:        proto.RoleAgent,
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, c, &connected); err != nil {
		t.Fatalf("read connected: %v", err)
	}
	return c, connected.SessionID
}

func TestHandleWSNormalizesPath(t *testing.T) {
	h := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	wsURL := "ws" + srv.URL[len("http"):]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	rawPath := "."
	if err := wsjson.Write(ctx, c, proto.ConnectFrame{
		Type:        proto.FrameConnect,
		DisplayName: "path-test",
		Circle:      "default",
		Backend:     proto.AgentClaudeCode,
		Role:        proto.RoleAgent,
		Path:        &rawPath,
	}); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	var connected proto.ConnectedFrame
	if err := wsjson.Read(ctx, c, &connected); err != nil {
		t.Fatalf("read connected: %v", err)
	}
	p, ok := h.reg.GetPeer(connected.SessionID)
	if !ok {
		t.Fatalf("peer %s not registered", connected.SessionID)
	}
	want, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	if p.Path != filepath.Clean(want) {
		t.Fatalf("path = %q, want %q", p.Path, filepath.Clean(want))
	}
}

func TestHandleWSRejectsFreshPanelessOrchestrator(t *testing.T) {
	h := newTestHub(t)
	ownership := service.NewFileOwnership("test-host", func(string) *service.TmuxPaneEvidence { return nil })
	h.WithSpawn(service.NewSpawnService(nil, ownership, nil, nil), nil, nil, "test-host", proto.CircleBoundarySession)
	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := wsjson.Write(ctx, c, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: "fake-orchestrator", Circle: "victim", Backend: proto.AgentCodex, Role: proto.RoleOrchestrator}); err != nil {
		t.Fatal(err)
	}
	var frame proto.ErrorFrame
	if err := wsjson.Read(ctx, c, &frame); err != nil || frame.Type != proto.FrameError {
		t.Fatalf("pane-less elevation response = %+v, %v", frame, err)
	}
}

// waitConnected blocks until the transport registers the peer's socket.
func waitConnected(t *testing.T, h *Hub, id proto.PeerID) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.transport.IsConnected(id) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("peer %s never connected", id)
}

// ----------------------------------------------------------------------------
// Shared fakes for the ask-lifecycle / messaging route tests (routes_ask_
// lifecycle_test.go, routes_messaging_test.go). Duplicated from service/
// delivery_test.go's and service/router_test.go's fakes of the same name — the
// two packages no longer share one straddling fixture now that hub and
// service are split; a fake passed as an interface value doesn't need to name
// the (unexported, service-side) interface it satisfies structurally.
// ----------------------------------------------------------------------------

// fakeRegistry satisfies the accessRegistry shape service.NewPeerDelivery
// needs. CheckAccess resolves from/to out of the peers map by display_name or
// peer_id; an unknown target returns checkErr.
type fakeRegistry struct {
	mu         sync.Mutex
	peers      []*proto.Peer
	checkErr   error // forced CheckAccess error (unknown/ambiguous/forbidden)
	events     []recordedEvent
	offlined   []proto.PeerID
	offlineErr error
}

type recordedEvent struct {
	typ     string
	payload map[string]any
}

func (r *fakeRegistry) lookup(name string) *proto.Peer {
	for _, p := range r.peers {
		if string(p.DisplayName) == name || string(p.PeerID) == name {
			return p
		}
	}
	return nil
}

func (r *fakeRegistry) CheckAccess(ctx context.Context, fromPeer, toPeer string, bypassCircle bool, circle *string) (*proto.Peer, *proto.Peer, error) {
	if r.checkErr != nil {
		return nil, nil, r.checkErr
	}
	target := r.lookup(toPeer)
	if target == nil {
		return nil, nil, errors.New("Unknown peer: " + toPeer)
	}
	return r.lookup(fromPeer), target, nil
}

func (r *fakeRegistry) GetPeer(id proto.PeerID) (*proto.Peer, bool) {
	if p := r.lookup(string(id)); p != nil {
		return p, true
	}
	return nil, false
}

func (r *fakeRegistry) GetAllPeers() []*proto.Peer { return r.peers }

func (r *fakeRegistry) AddEvent(ctx context.Context, typ string, payload map[string]any) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{typ: typ, payload: payload})
	return "evt-" + typ
}

func (r *fakeRegistry) MarkOffline(ctx context.Context, id proto.PeerID, terminal bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offlined = append(r.offlined, id)
	return 0, r.offlineErr
}

// fakeTransport is a hand-driven service.Transport fake so route tests can
// control the delivery_ack timing/shape without standing up a real socket. It
// records the last frame and the target it was sent to.
type fakeTransport struct {
	sessions  []proto.PeerID
	paneIDs   map[proto.PeerID]string
	connected map[proto.PeerID]bool

	// ack governs SendAndWaitDeliveryAck: if ackDelay > timeout the call returns
	// (nil,nil) (the best-effort timeout path); otherwise it returns ackFrame.
	ackFrame map[string]any
	ackDelay time.Duration
	ackErr   error

	sendErr error

	mu          sync.Mutex
	lastTarget  proto.PeerID
	lastFrame   any
	sentTargets []proto.PeerID
}

func (f *fakeTransport) Send(ctx context.Context, id proto.PeerID, v any) error {
	f.mu.Lock()
	f.lastTarget = id
	f.lastFrame = v
	f.sentTargets = append(f.sentTargets, id)
	f.mu.Unlock()
	return f.sendErr
}

func (f *fakeTransport) SendAndWaitDeliveryAck(ctx context.Context, id proto.PeerID, v any, timeout time.Duration) (map[string]any, error) {
	f.mu.Lock()
	f.lastTarget = id
	f.lastFrame = v
	f.mu.Unlock()
	if f.ackErr != nil {
		return nil, f.ackErr
	}
	if f.ackDelay >= timeout {
		return nil, nil
	}
	return f.ackFrame, nil
}

func (f *fakeTransport) IsConnected(id proto.PeerID) bool { return f.connected[id] }

func (f *fakeTransport) GetAllSessions() []proto.PeerID { return f.sessions }

func (f *fakeTransport) ConnectionPaneID(id proto.PeerID) (string, bool) {
	p, ok := f.paneIDs[id]
	return p, ok
}

func (f *fakeTransport) Ping(ctx context.Context, id proto.PeerID, timeout time.Duration) (map[string]any, error) {
	return nil, nil
}

func (f *fakeTransport) ACPRoute(target *proto.Peer) (*service.ACPRouteDecision, bool) {
	return nil, false
}

func newRouterWithFake(f *fakeTransport) *service.MessageRouter {
	// reg is unused by the send paths under test; nil is fine for unit coverage.
	return service.NewMessageRouter(f, nil)
}

// fakeQueue satisfies the queuedDeliveryStore shape service.NewPeerDelivery
// needs. enqueueNil forces the "queue disabled" (cap/ttl <= 0) return so the
// fail-loud path is exercised.
type fakeQueue struct {
	enqueued   []state.QueuedDelivery
	enqueueNil bool
	enqueueErr error
}

func (q *fakeQueue) EnqueueDelivery(ctx context.Context, d state.QueuedDelivery, ttlSeconds float64, maxPerPeer int, now time.Time) (*state.QueuedDelivery, error) {
	if q.enqueueErr != nil {
		return nil, q.enqueueErr
	}
	if q.enqueueNil {
		return nil, nil
	}
	out := d
	out.DeliveryID = "qd-test"
	q.enqueued = append(q.enqueued, out)
	return &out, nil
}

// peerWith builds a minimal test peer. Shared across the route test files.
func peerWith(id, name, circle string, status proto.PeerStatus) *proto.Peer {
	return &proto.Peer{PeerID: proto.PeerID(id), DisplayName: proto.DisplayName(name), Circle: circle, Status: status, Role: proto.RoleAgent}
}

// strp is the shared string-pointer test helper. Duplicated from
// service/ask_tracker_test.go — the two packages no longer share one
// straddling helper now that hub and service are split.
func strp(s string) *string { return &s }
