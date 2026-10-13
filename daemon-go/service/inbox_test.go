package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// surfaceTransport is a fake set of inbox surfaces: it records every frame
// each peer was handed and answers delivery acks from a per-peer script.
type surfaceTransport struct {
	mu        sync.Mutex
	connected map[proto.PeerID]bool
	frames    map[proto.PeerID][]map[string]any
	receipts  map[proto.PeerID]func(frame map[string]any) map[string]any
}

func newSurfaceTransport(connected ...proto.PeerID) *surfaceTransport {
	t := &surfaceTransport{connected: map[proto.PeerID]bool{}, frames: map[proto.PeerID][]map[string]any{},
		receipts: map[proto.PeerID]func(map[string]any) map[string]any{}}
	for _, id := range connected {
		t.connected[id] = true
	}
	return t
}

func (t *surfaceTransport) record(id proto.PeerID, v any) map[string]any {
	frame, _ := v.(map[string]any)
	t.mu.Lock()
	t.frames[id] = append(t.frames[id], frame)
	t.mu.Unlock()
	return frame
}

func (t *surfaceTransport) Send(_ context.Context, id proto.PeerID, v any) error {
	t.record(id, v)
	return nil
}

func (t *surfaceTransport) SendAndWaitDeliveryAck(_ context.Context, id proto.PeerID, v any, _ time.Duration) (map[string]any, error) {
	frame := t.record(id, v)
	t.mu.Lock()
	receipt := t.receipts[id]
	t.mu.Unlock()
	if receipt == nil {
		return nil, nil
	}
	return receipt(frame), nil
}

func (t *surfaceTransport) framesFor(id proto.PeerID) []map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]map[string]any(nil), t.frames[id]...)
}

func (t *surfaceTransport) IsConnected(id proto.PeerID) bool { return t.connected[id] }
func (t *surfaceTransport) GetAllSessions() []proto.PeerID {
	var out []proto.PeerID
	for id := range t.connected {
		out = append(out, id)
	}
	return out
}
func (t *surfaceTransport) ConnectionPaneID(proto.PeerID) (string, bool) { return "", false }
func (t *surfaceTransport) Ping(context.Context, proto.PeerID, time.Duration) (map[string]any, error) {
	return nil, nil
}
func (t *surfaceTransport) ACPRoute(*proto.Peer) (*ACPRouteDecision, bool) { return nil, false }

func (r *fakeRegistry) UpdateMetadataByName(_ context.Context, identifier string, metadata map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.peers {
		if string(p.PeerID) == identifier {
			if p.Metadata == nil {
				p.Metadata = map[string]any{}
			}
			for k, v := range metadata {
				p.Metadata[k] = v
			}
			return true, nil
		}
	}
	return false, nil
}

type inboxFixture struct {
	store     *state.Store
	reg       *fakeRegistry
	transport *surfaceTransport
	asks      *AskTracker
	inbox     *Inbox
	delivery  *PeerDelivery
	human     *proto.Peer
	telegram  *proto.Peer
	slack     *proto.Peer
	alpha     *proto.Peer
	beta      *proto.Peer
}

func newInboxFixture(t *testing.T, cfg config.HumanConfig) *inboxFixture {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &inboxFixture{store: store}
	f.human = &proto.Peer{PeerID: "repow-h", DisplayName: "prass", Role: proto.RoleHuman, Status: proto.StatusOnline, Path: "/human"}
	// telegram declares the inbox capability; slack is a legacy bridge.
	f.telegram = &proto.Peer{PeerID: "repow-tg", DisplayName: "telegram", Role: proto.RoleService, Status: proto.StatusOnline, Path: "/telegram",
		Metadata: map[string]any{"capabilities": []string{proto.CapInboxSurface}}}
	f.slack = &proto.Peer{PeerID: "repow-sl", DisplayName: "slack", Role: proto.RoleService, Status: proto.StatusOnline, Path: "/slack"}
	f.alpha = peerWith("repow-a", "alpha", "1", proto.StatusOnline)
	f.beta = peerWith("repow-b", "beta", "1", proto.StatusOnline)
	f.reg = &fakeRegistry{peers: []*proto.Peer{f.human, f.telegram, f.slack, f.alpha, f.beta}}
	f.transport = newSurfaceTransport(f.telegram.PeerID, f.slack.PeerID, f.alpha.PeerID, f.beta.PeerID)
	f.transport.receipts[f.telegram.PeerID] = func(map[string]any) map[string]any {
		return map[string]any{"status": "pushed", "surface_ref": "555"}
	}
	f.asks = NewAskTracker(0)
	f.inbox = NewInbox(store, f.reg, f.transport, f.asks, cfg)
	f.inbox.SetHuman(f.human.PeerID, f.human.DisplayName)
	f.delivery = NewPeerDelivery(f.reg, NewMessageRouter(f.transport, nil), f.transport, f.asks, nil).WithInbox(f.inbox)
	t.Cleanup(f.inbox.Close)
	return f
}

// settle waits for in-flight pushes without cancelling them (Close would).
func (f *inboxFixture) settle() { f.inbox.wg.Wait() }

func (f *inboxFixture) stages(t *testing.T, traceID string) []state.TraceRow {
	t.Helper()
	rows, err := f.store.StagesFor(context.Background(), traceID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func stageNames(rows []state.TraceRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Stage)
	}
	return out
}

func TestInboxNotifyStoresAndPushesToEverySurfaceWithReceipts(t *testing.T) {
	f := newInboxFixture(t, config.HumanConfig{})
	ctx := context.Background()

	res, err := f.delivery.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "prass", Text: "secret plan: ship v2", DeliveryID: "notif-delivery-t1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport != "inbox" || !res.Delivered() || res.Reason != "inbox_stored" || res.Inbox == nil {
		t.Fatalf("notify to the human = %+v", res)
	}
	if got := strings.Join(res.Inbox.Surfaces, ","); got != "slack,telegram" {
		t.Fatalf("receipt surfaces = %q", got)
	}
	f.settle()

	tg := f.transport.framesFor(f.telegram.PeerID)
	if len(tg) != 1 || tg[0]["type"] != "inbox_item" || tg[0]["delivery_id"] != res.Inbox.ItemID {
		t.Fatalf("telegram frames = %+v", tg)
	}
	if item, _ := tg[0]["item"].(map[string]any); item["text"] != "secret plan: ship v2" || item["kind"] != "notify" || item["from"] != "alpha" {
		t.Fatalf("telegram item = %+v", tg[0]["item"])
	}
	sl := f.transport.framesFor(f.slack.PeerID)
	if len(sl) != 1 || sl[0]["type"] != "notify" || sl[0]["text"] != "secret plan: ship v2" {
		t.Fatalf("legacy slack frame = %+v", sl)
	}

	item, err := f.store.InboxItem(ctx, res.Inbox.ItemID)
	if err != nil || item == nil {
		t.Fatal(err)
	}
	if item.Kind != "notify" || item.CorrelationID != "" || len(item.Pushes) != 2 {
		t.Fatalf("stored item = %+v", item)
	}
	for _, p := range item.Pushes {
		if p.Status != state.PushPushed {
			t.Fatalf("push %+v not pushed", p)
		}
		if p.Surface == "telegram" && p.SurfaceRef != "555" {
			t.Fatalf("telegram ref not kept: %+v", p)
		}
	}

	rows := f.stages(t, "notif-delivery-t1")
	if got := strings.Join(stageNames(rows), ","); got != "inbox_stored,surface_pushed,surface_pushed" {
		t.Fatalf("stages = %s", got)
	}
	for _, r := range rows {
		raw, _ := json.Marshal(r.Detail)
		if strings.Contains(string(raw), "secret plan") {
			t.Fatalf("trace row %s carries message text: %s", r.Stage, raw)
		}
	}
	verified := map[string]any{}
	for _, r := range rows[1:] {
		verified[r.Detail["surface"].(string)] = r.Detail["verified"]
	}
	if verified["telegram"] != true || verified["slack"] != false {
		t.Fatalf("receipt proof wrong: %+v", verified)
	}
}

func TestInboxPushFailureAndMissingReceiptAreSurfaceFailed(t *testing.T) {
	f := newInboxFixture(t, config.HumanConfig{})
	f.transport.receipts[f.telegram.PeerID] = func(map[string]any) map[string]any {
		return map[string]any{"status": "failed", "error_class": "rate_limited"}
	}
	f.transport.connected[f.slack.PeerID] = false
	ctx := context.Background()
	if _, err := f.delivery.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "prass", Text: "x", DeliveryID: "notif-delivery-f1"}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	f.transport.mu.Lock()
	f.transport.receipts[f.telegram.PeerID] = nil // never acks
	f.transport.mu.Unlock()
	if _, err := f.delivery.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "prass", Text: "y", DeliveryID: "notif-delivery-f2"}); err != nil {
		t.Fatal(err)
	}
	f.settle()
	for traceID, class := range map[string]string{"notif-delivery-f1": "rate_limited", "notif-delivery-f2": "no_receipt"} {
		rows := f.stages(t, traceID)
		last := rows[len(rows)-1]
		if last.Stage != "surface_failed" || last.Status != "fail" || last.Detail["error_class"] != class || last.Detail["surface"] != "telegram" {
			t.Fatalf("%s: last stage = %+v", traceID, last)
		}
	}
}

func TestInboxThreadsRepliesOnlyOnTheHumansOwnAsks(t *testing.T) {
	f := newInboxFixture(t, config.HumanConfig{})
	ctx := context.Background()
	mine, _ := f.asks.Register(ctx, RegisterAskParams{FromPeerID: f.human.PeerID, FromPeerName: f.human.DisplayName, ToPeerID: f.alpha.PeerID, ToPeerName: f.alpha.DisplayName, Text: "status?"})
	f.inbox.RecordOutbound(ctx, mine, f.alpha, "status?", "telegram", "100")
	foreign, _ := f.asks.Register(ctx, RegisterAskParams{FromPeerID: f.beta.PeerID, ToPeerID: f.alpha.PeerID, Text: "unrelated"})

	threaded, err := f.delivery.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "prass", Text: "[ack #" + mine + " from @alpha] green", CorrelationID: mine, ThreadText: "green"})
	if err != nil {
		t.Fatal(err)
	}
	stray, err := f.delivery.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "prass", Text: "fyi", CorrelationID: foreign, ThreadText: "fyi"})
	if err != nil {
		t.Fatal(err)
	}
	f.settle()

	item, _ := f.store.InboxItem(ctx, threaded.Inbox.ItemID)
	if item.Kind != "answer" || item.CorrelationID != mine || item.Text != "green" || item.TraceID != mine || item.TraceKind != "ask" {
		t.Fatalf("threaded reply = %+v", item)
	}
	strayItem, _ := f.store.InboxItem(ctx, stray.Inbox.ItemID)
	if strayItem.Kind != "notify" || strayItem.CorrelationID != "" {
		t.Fatalf("a foreign correlation_id must not thread: %+v", strayItem)
	}
	refs := map[string]any{}
	for _, frame := range f.transport.framesFor(f.telegram.PeerID) {
		item, _ := frame["item"].(map[string]any)
		refs[item["id"].(string)] = frame["reply_to_ref"]
	}
	if refs[threaded.Inbox.ItemID] != "100" || refs[stray.Inbox.ItemID] != "" {
		t.Fatalf("reply_to_ref = %+v", refs)
	}
	if ask, _ := f.asks.Get(foreign); ask.Closed {
		t.Fatal("a notify must never touch an ask")
	}
}

func TestInboxAskBecomesAnAskItemWithItsQuestion(t *testing.T) {
	f := newInboxFixture(t, config.HumanConfig{})
	ctx := context.Background()
	question := map[string]any{"kind": "choice", "options": []any{map[string]any{"id": "y", "title": "Yes"}}}
	cid, _ := f.asks.Register(ctx, RegisterAskParams{FromPeerID: f.alpha.PeerID, ToPeerID: f.human.PeerID, Text: "merge?", Question: question})
	res, err := f.delivery.DeliverAsk(ctx, DeliverAskParams{FromPeer: "alpha", ToPeer: "prass", Text: "merge?", CorrelationID: cid, Question: question})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport != "inbox" || res.Inbox == nil || res.HookDelivery["status"] != "inbox_stored" {
		t.Fatalf("ask to the human = %+v", res)
	}
	f.settle()
	item, _ := f.store.InboxItem(ctx, res.Inbox.ItemID)
	if item.Kind != "ask" || item.CorrelationID != cid || item.Question["kind"] != "choice" || item.TraceID != cid {
		t.Fatalf("ask item = %+v", item)
	}
	if ask, _ := f.asks.Get(cid); ask.Closed {
		t.Fatal("storing an ask must leave it open for the human's reply")
	}
}

func TestInboxBroadcastIsOptInPerInboxAndPerSurface(t *testing.T) {
	ctx := context.Background()
	off := newInboxFixture(t, config.HumanConfig{})
	sent, _ := off.delivery.Broadcast(ctx, "alpha", "deploy at noon", nil, true)
	off.settle()
	if items, _ := off.store.ListInbox(ctx, state.InboxFilter{}); len(items) != 0 {
		t.Fatalf("broadcast stored with human.broadcast off: %+v", items)
	}
	for _, name := range sent {
		if name == "prass" || name == "telegram" || name == "slack" {
			t.Fatalf("broadcast reached %s with the inbox default off", name)
		}
	}
	if frames := off.transport.framesFor(off.telegram.PeerID); len(frames) != 0 {
		t.Fatalf("telegram took a raw broadcast frame: %+v", frames)
	}

	on := newInboxFixture(t, config.HumanConfig{Broadcast: true, Surfaces: map[string]config.HumanSurfaceConfig{"telegram": {Broadcast: true}}})
	sent, _ = on.delivery.Broadcast(ctx, "alpha", "deploy at noon", nil, true)
	on.settle()
	items, _ := on.store.ListInbox(ctx, state.InboxFilter{})
	if len(items) != 1 || items[0].Kind != "broadcast" {
		t.Fatalf("opted-in broadcast not stored: %+v", items)
	}
	if !containsName(sent, "prass") {
		t.Fatalf("sent = %v, want the human", sent)
	}
	if frames := on.transport.framesFor(on.telegram.PeerID); len(frames) != 1 || frames[0]["type"] != "inbox_item" {
		t.Fatalf("opted-in telegram frames = %+v", frames)
	}
	if frames := on.transport.framesFor(on.slack.PeerID); len(frames) != 0 {
		t.Fatalf("slack did not opt in, got %+v", frames)
	}
}

func TestInboxStoreAfterCloseStoresWithoutPushing(t *testing.T) {
	f := newInboxFixture(t, config.HumanConfig{})
	f.inbox.Close()
	res, err := f.delivery.Notify(context.Background(), NotifyParams{FromPeer: "alpha", ToPeer: "prass", Text: "late"})
	if err != nil || res.Inbox == nil {
		t.Fatalf("notify after close = %+v, %v", res, err)
	}
	if frames := f.transport.framesFor(f.telegram.PeerID); len(frames) != 0 {
		t.Fatalf("pushed after close: %+v", frames)
	}
}

func TestInboxSyncPresencePublishesLiveSurfaces(t *testing.T) {
	f := newInboxFixture(t, config.HumanConfig{})
	f.transport.connected[f.slack.PeerID] = false
	f.inbox.SyncPresence(context.Background())
	if f.human.Metadata["surfaces_live"] != 1 {
		t.Fatalf("human metadata = %+v", f.human.Metadata)
	}
	surfaces, _ := f.human.Metadata["surfaces"].(map[string]any)
	if tg, _ := surfaces["telegram"].(map[string]any); tg["live"] != true {
		t.Fatalf("surfaces = %+v", surfaces)
	}
}

func containsName(names []proto.DisplayName, want string) bool {
	for _, n := range names {
		if string(n) == want {
			return true
		}
	}
	return false
}
