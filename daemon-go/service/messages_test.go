package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

func newMessageStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestNudgeNamesTheMessageWithoutTheBody(t *testing.T) {
	msg := &state.PeerMessage{ID: "msg-1a2b3c4d", Kind: state.MessageNotify, FromName: "@reviewer",
		Text: "\n  Findings on #12\nsecret detail on line two", Attachments: []map[string]any{{"id": "att-1"}}}
	got := Nudge(msg)
	want := "[message msg-1a2b3c4d: notify from @reviewer, 44 chars, 1 attachment(s)] Findings on #12\n↳ read_message(\"msg-1a2b3c4d\")"
	if got != want {
		t.Fatalf("nudge =\n%q\nwant\n%q", got, want)
	}
	long := &state.PeerMessage{ID: "msg-x", Kind: state.MessageAsk, FromPeerID: "repow-a", Text: strings.Repeat("é", 200)}
	if line := strings.SplitN(Nudge(long), "\n", 2)[0]; !strings.HasSuffix(line, "…") || strings.Count(line, "é") != nudgePreviewRunes-1 || !strings.Contains(line, "from @repow-a, 200 chars") {
		t.Fatalf("long nudge = %q", line)
	}
}

func TestMessageDeliveryModeIsDurable(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t)
	m := NewMessages(store)
	if got := m.Mode(ctx, agentPeer("repow-b")); got != MessageDeliveryPush {
		t.Fatalf("default mode = %q", got)
	}
	if err := m.SetMode(ctx, "repow-b", "sideways"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := m.SetMode(ctx, "repow-b", MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}
	if got := m.Mode(ctx, agentPeer("repow-b")); got != MessageDeliveryPull {
		t.Fatalf("mode after set = %q", got)
	}
	// A restart builds a fresh Messages over the same store.
	if got := NewMessages(store).Mode(ctx, agentPeer("repow-b")); got != MessageDeliveryPull {
		t.Fatalf("mode after restart = %q", got)
	}
	var nilMessages *Messages
	if got := nilMessages.Mode(ctx, agentPeer("repow-b")); got != MessageDeliveryPush {
		t.Fatalf("nil Messages mode = %q", got)
	}
}

// deliveryWithMessages builds a delivery over the service fakes with a real
// message store.
func deliveryWithMessages(t *testing.T, f *fakeTransport, q *fakeQueue, peers ...*proto.Peer) (*PeerDelivery, *Messages, *state.Store) {
	t.Helper()
	store := newMessageStore(t)
	messages := NewMessages(store)
	reg := &fakeRegistry{peers: peers}
	var queue queuedDeliveryStore
	if q != nil {
		queue = q
	}
	d := NewPeerDelivery(reg, newRouterWithFake(f), f, NewAskTracker(0), queue).WithMessages(messages)
	return d, messages, store
}

func TestNotifyRecordsMessageAndStampsFrame(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta)

	res, err := d.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "full body", Attachments: []map[string]any{{"id": "att-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.MessageID, "msg-") || !strings.HasPrefix(res.DeliveryID, "notif-delivery-") {
		t.Fatalf("ids not minted: %+v", res)
	}
	frame := f.lastFrame.(map[string]any)
	if frame["message_id"] != res.MessageID || frame["text"] != "full body" || frame["delivery_id"] != res.DeliveryID || frame["attachments"] == nil {
		t.Fatalf("push frame = %#v", frame)
	}
	msg, _ := messages.Get(ctx, res.MessageID)
	if msg == nil || msg.Kind != state.MessageNotify || msg.PeerID != string(beta.PeerID) || msg.FromName != "alpha" ||
		msg.TraceID != res.DeliveryID || msg.Text != "full body" || msg.ReadAt == "" {
		t.Fatalf("record = %+v (a confirmed full push is read)", msg)
	}
}

func TestPullRecipientGetsNudgeAndMessageStaysUnread(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta)
	if err := messages.SetMode(ctx, beta.PeerID, MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}

	res, err := d.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "the whole body", Attachments: []map[string]any{{"id": "att-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	frame := f.lastFrame.(map[string]any)
	text, _ := frame["text"].(string)
	if !strings.HasPrefix(text, "[message "+res.MessageID+": notify from @alpha") || frame["attachments"] != nil || frame["message_id"] != res.MessageID {
		t.Fatalf("pull frame = %#v", frame)
	}
	if msg, _ := messages.Get(ctx, res.MessageID); msg.Text != "the whole body" || msg.ReadAt != "" || len(msg.Attachments) != 1 {
		t.Fatalf("pull record = %+v", msg)
	}

	// An ask nudge keeps the correlation id and the ack hint.
	ask, err := d.DeliverAsk(ctx, DeliverAskParams{FromPeer: "alpha", ToPeer: "beta", Text: "review #12?\nthe diff is long", CorrelationID: "ask-1"})
	if err != nil {
		t.Fatal(err)
	}
	frame = f.lastFrame.(map[string]any)
	text, _ = frame["text"].(string)
	if frame["correlation_id"] != "ask-1" || frame["message_id"] != ask.MessageID ||
		!strings.Contains(text, "[message "+ask.MessageID+": ask from @alpha") || !strings.Contains(text, `ack("ask-1")`) || strings.Contains(text, "the diff is long") {
		t.Fatalf("pull ask frame = %#v", frame)
	}
}

func TestQueuedNotifyCarriesMessageID(t *testing.T) {
	ctx := context.Background()
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	f := &fakeTransport{ackErr: ErrNotConnected}
	q := &fakeQueue{}
	d, messages, _ := deliveryWithMessages(t, f, q, beta)

	res, err := d.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "later"})
	if err != nil || !res.Queued() {
		t.Fatalf("queued notify: %+v %v", res, err)
	}
	if len(q.enqueued) != 1 || q.enqueued[0].Metadata["message_id"] != res.MessageID {
		t.Fatalf("queued row = %+v", q.enqueued)
	}
	if msg, _ := messages.Get(ctx, res.MessageID); msg == nil || msg.ReadAt != "" {
		t.Fatalf("a queued message is held, not read: %+v", msg)
	}
}

func TestFailedSendDiscardsMessageRecord(t *testing.T) {
	ctx := context.Background()
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	f := &fakeTransport{ackErr: ErrNotConnected}
	d, messages, _ := deliveryWithMessages(t, f, nil, beta)

	if _, err := d.DeliverAsk(ctx, DeliverAskParams{FromPeer: "alpha", ToPeer: "beta", Text: "x", CorrelationID: "ask-2"}); err == nil {
		t.Fatal("ask to a disconnected peer succeeded")
	}
	if _, err := d.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "y", NoFallbackQueue: true}); err == nil {
		t.Fatal("unqueued notify to a disconnected peer succeeded")
	}
	if got, _ := messages.List(ctx, state.PeerMessageFilter{PeerID: string(beta.PeerID)}); len(got) != 0 {
		t.Fatalf("failed sends left records: %+v", got)
	}
}

func TestReplyNotifyIsRecordedAsReply(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha)

	res, err := d.Notify(ctx, NotifyParams{FromPeer: "beta", ToPeer: "alpha", Text: "[ack #ask-3 from @beta] done", CorrelationID: "ask-3", BypassCircle: true})
	if err != nil {
		t.Fatal(err)
	}
	if msg, _ := messages.Get(ctx, res.MessageID); msg == nil || msg.Kind != state.MessageReply || msg.CorrelationID != "ask-3" {
		t.Fatalf("reply record = %+v", msg)
	}
}

// Review #523: concurrent mode changes must leave the cache agreeing with the
// store, so a restart never flips a peer's delivery.
func TestConcurrentModeChangesKeepCacheAndStoreAgreed(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t)
	m := NewMessages(store)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mode := MessageDeliveryPush
			if i%2 == 0 {
				mode = MessageDeliveryPull
			}
			if err := m.SetMode(ctx, "repow-b", mode); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if live, restarted := m.Mode(ctx, agentPeer("repow-b")), NewMessages(store).Mode(ctx, agentPeer("repow-b")); live != restarted {
		t.Fatalf("cache says %s, store says %s", live, restarted)
	}
}

// daemon.message_delivery sets the mode of every peer that has not chosen
// one; a peer's own choice, push included, still wins.
func TestDefaultModeAppliesUntilAPeerChooses(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t)
	m := NewMessages(store).WithDefaultMode(MessageDeliveryPull)
	if got := m.Mode(ctx, agentPeer("repow-new")); got != MessageDeliveryPull {
		t.Fatalf("unchosen mode under a pull default = %q", got)
	}
	if err := m.SetMode(ctx, "repow-push", MessageDeliveryPush); err != nil {
		t.Fatal(err)
	}
	if got := m.Mode(ctx, agentPeer("repow-push")); got != MessageDeliveryPush {
		t.Fatalf("a peer that chose push = %q", got)
	}
	if got := NewMessages(store).WithDefaultMode(MessageDeliveryPull).Mode(ctx, agentPeer("repow-push")); got != MessageDeliveryPush {
		t.Fatalf("a chosen push after restart = %q", got)
	}
	if got := NewMessages(store).WithDefaultMode("sideways").Mode(ctx, agentPeer("repow-new")); got != MessageDeliveryPush {
		t.Fatalf("an unknown default = %q", got)
	}
}

// agentPeer is an agent-role recipient, which can follow a nudge.
func agentPeer(id proto.PeerID) *proto.Peer {
	return &proto.Peer{PeerID: id, Role: proto.RoleAgent}
}

func TestPullDefaultNudgesAPeerThatNeverChose(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta)
	messages.WithDefaultMode(MessageDeliveryPull)

	res, err := d.Notify(ctx, NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "summary\nthe detail stays in the record"})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := f.lastFrame.(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "[message "+res.MessageID+": notify from @alpha") || strings.Contains(text, "the detail") {
		t.Fatalf("frame text under a pull default = %q", text)
	}
}

// The pull default covers only peers that can follow a nudge, and never the
// spawn seed: a service bridge cannot call read_message, and the seed is the
// task that starts a session that has not chosen anything yet.
func TestPullDefaultPushesTheSeedAndPeersThatCannotPull(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	bot := peerWith("repow-default-cccc", "bot", "default", proto.StatusOnline)
	bot.Role = proto.RoleService
	f := &fakeTransport{ackFrame: map[string]any{"status": "accepted"}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta, bot)
	messages.WithDefaultMode(MessageDeliveryPull)
	body := "do the task\nwith the detail on line two"
	sent := func(params NotifyParams) string {
		t.Helper()
		if _, err := d.Notify(ctx, params); err != nil {
			t.Fatal(err)
		}
		text, _ := f.lastFrame.(map[string]any)["text"].(string)
		return text
	}

	if text := sent(NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: body, BypassCircle: true, SkipSeedGate: true}); text != body {
		t.Fatalf("seed under a pull default = %q", text)
	}
	if text := sent(NotifyParams{FromPeer: "alpha", ToPeer: "bot", Text: body}); text != body {
		t.Fatalf("service peer under a pull default = %q", text)
	}
	if text := sent(NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: body}); !strings.HasPrefix(text, "[message ") || strings.Contains(text, "line two") {
		t.Fatalf("agent under a pull default = %q", text)
	}
	if got := messages.Mode(ctx, bot); got != MessageDeliveryPush {
		t.Fatalf("service peer's reported mode = %q", got)
	}
	// A service peer's own choice still wins.
	if err := messages.SetMode(ctx, bot.PeerID, MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}
	if got := messages.Mode(ctx, bot); got != MessageDeliveryPull {
		t.Fatalf("service peer that chose pull = %q", got)
	}
}

// acpTransport routes every peer over ACP.
type acpTransport struct{ *fakeTransport }

func (acpTransport) ACPRoute(p *proto.Peer) (*ACPRouteDecision, bool) {
	return &ACPRouteDecision{PeerID: p.PeerID}, true
}

// An ACP session is started without MCP tools, so it has no read_message:
// it is pushed whatever the default or its recorded mode.
func TestACPRecipientIsAlwaysPushed(t *testing.T) {
	ctx := context.Background()
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	messages := NewMessages(newMessageStore(t)).WithDefaultMode(MessageDeliveryPull)
	if err := messages.SetMode(ctx, beta.PeerID, MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}
	f := &fakeTransport{}
	acp := NewPeerDelivery(&fakeRegistry{peers: []*proto.Peer{beta}}, newRouterWithFake(f), acpTransport{f}, NewAskTracker(0), nil).WithMessages(messages)
	if got := acp.MessageMode(ctx, beta); got != MessageDeliveryPush {
		t.Fatalf("ACP recipient's mode = %q", got)
	}
	ws := NewPeerDelivery(&fakeRegistry{peers: []*proto.Peer{beta}}, newRouterWithFake(f), f, NewAskTracker(0), nil).WithMessages(messages)
	if got := ws.MessageMode(ctx, beta); got != MessageDeliveryPull {
		t.Fatalf("WS recipient's mode = %q", got)
	}
}

// A broadcast is recorded once per recipient: each frame carries its own
// message_id, a pull recipient gets a nudge, and a push recipient's copy is
// read on send.
func TestBroadcastRecordsEachRecipientsCopy(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	gamma := peerWith("repow-default-cccc", "gamma", "default", proto.StatusOnline)
	f := &fakeTransport{sessions: []proto.PeerID{alpha.PeerID, beta.PeerID, gamma.PeerID}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta, gamma)
	if err := messages.SetMode(ctx, gamma.PeerID, MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}

	body := "deploy at noon\nfreeze merges until then"
	if sent, failed := d.Broadcast(ctx, "alpha", body, nil, false); len(sent) != 2 || len(failed) != 0 {
		t.Fatalf("sent %v, failed %v", sent, failed)
	}
	if _, ok := f.framesTo[alpha.PeerID]; ok {
		t.Fatal("the sender got its own broadcast")
	}
	read := func(p *proto.Peer) (map[string]any, *state.PeerMessage) {
		t.Helper()
		frame, _ := f.framesTo[p.PeerID].(map[string]any)
		id, _ := frame["message_id"].(string)
		msg, err := messages.Get(ctx, id)
		if err != nil || msg == nil {
			t.Fatalf("%s: no record for frame %#v (%v)", p.DisplayName, frame, err)
		}
		if msg.Kind != state.MessageBroadcast || msg.PeerID != string(p.PeerID) || msg.FromName != "alpha" || msg.Text != body {
			t.Fatalf("%s: record = %+v", p.DisplayName, msg)
		}
		return frame, msg
	}
	pushFrame, pushMsg := read(beta)
	if pushFrame["text"] != body || pushMsg.ReadAt == "" {
		t.Fatalf("push copy: frame %#v, read_at %q", pushFrame, pushMsg.ReadAt)
	}
	pullFrame, pullMsg := read(gamma)
	text, _ := pullFrame["text"].(string)
	if !strings.HasPrefix(text, "[message "+pullMsg.ID+": broadcast from @alpha") || strings.Contains(text, "freeze merges") || pullMsg.ReadAt != "" {
		t.Fatalf("pull copy: text %q, read_at %q", text, pullMsg.ReadAt)
	}
	if pushMsg.ID == pullMsg.ID {
		t.Fatal("recipients share one record")
	}
	// Every copy names the one broadcast (its journal event).
	if pushMsg.TraceID != "evt-broadcast" || pullMsg.TraceID != pushMsg.TraceID {
		t.Fatalf("trace ids %q / %q, want the broadcast event's", pushMsg.TraceID, pullMsg.TraceID)
	}
}

// A copy whose send failed is not left behind for the recipient to list.
func TestFailedBroadcastSendDiscardsTheCopy(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	f := &fakeTransport{sessions: []proto.PeerID{beta.PeerID}, sendErr: errors.New("socket closed")}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta)
	if _, failed := d.Broadcast(ctx, "alpha", "deploy at noon", nil, false); len(failed) != 1 {
		t.Fatalf("failed = %v", failed)
	}
	if list, err := messages.List(ctx, state.PeerMessageFilter{PeerID: string(beta.PeerID)}); err != nil || len(list) != 0 {
		t.Fatalf("beta's messages after a failed send = %+v (%v)", list, err)
	}
}

// An ACP recipient whose prompt is refused is reported as failed, with the
// fanout's own failures, and its copy is discarded.
func TestFailedACPBroadcastIsReported(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	messages := NewMessages(newMessageStore(t))
	f := &fakeTransport{}
	d := NewPeerDelivery(&fakeRegistry{peers: []*proto.Peer{alpha, beta}}, newRouterWithFake(f), acpTransport{f}, NewAskTracker(0), nil).WithMessages(messages)
	sent, failed := d.Broadcast(ctx, "alpha", "deploy at noon", nil, false)
	if len(sent) != 0 || len(failed) != 1 || failed[0].PeerID != beta.PeerID {
		t.Fatalf("sent = %v, failed = %v", sent, failed)
	}
	if list, err := messages.List(ctx, state.PeerMessageFilter{PeerID: string(beta.PeerID)}); err != nil || len(list) != 0 {
		t.Fatalf("beta's messages after a refused prompt = %+v (%v)", list, err)
	}
}

// A recipient still seeding gets its copy after the seed gate, recorded and
// nudged like the rest.
func TestDeferredBroadcastCarriesItsRecord(t *testing.T) {
	ctx := context.Background()
	alpha := peerWith("repow-default-aaaa", "alpha", "default", proto.StatusOnline)
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	beta.TurnState = proto.TurnPendingFirstTurn
	f := &fakeTransport{sessions: []proto.PeerID{beta.PeerID}}
	d, messages, _ := deliveryWithMessages(t, f, nil, alpha, beta)
	if err := messages.SetMode(ctx, beta.PeerID, MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}
	if sent, _ := d.Broadcast(ctx, "alpha", "deploy at noon\nsecond line", nil, false); len(sent) != 1 {
		t.Fatalf("sent = %v", sent)
	}
	d.Close() // releases the seed gate and joins the deferred send

	frame, _ := f.framesTo[beta.PeerID].(map[string]any)
	id, _ := frame["message_id"].(string)
	text, _ := frame["text"].(string)
	if msg, _ := messages.Get(ctx, id); msg == nil || msg.Kind != state.MessageBroadcast || !strings.HasPrefix(text, "[message "+id) || strings.Contains(text, "second line") {
		t.Fatalf("deferred frame %#v, record %+v", frame, msg)
	}
}

// A send that failed because its request was cancelled still drops its record.
func TestDiscardOutlivesACancelledRequest(t *testing.T) {
	store := newMessageStore(t)
	m := NewMessages(store)
	msg := m.Record(context.Background(), state.PeerMessage{Kind: state.MessageBroadcast, PeerID: "repow-b", Text: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Discard(ctx, msg)
	if got, err := m.Get(context.Background(), msg.ID); err != nil || got != nil {
		t.Fatalf("record after a discard under a cancelled ctx = %+v (%v)", got, err)
	}
}
