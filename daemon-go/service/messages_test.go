package service

import (
	"context"
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
	if got := m.Mode(ctx, "repow-b"); got != MessageDeliveryPush {
		t.Fatalf("default mode = %q", got)
	}
	if err := m.SetMode(ctx, "repow-b", "sideways"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := m.SetMode(ctx, "repow-b", MessageDeliveryPull); err != nil {
		t.Fatal(err)
	}
	if got := m.Mode(ctx, "repow-b"); got != MessageDeliveryPull {
		t.Fatalf("mode after set = %q", got)
	}
	// A restart builds a fresh Messages over the same store.
	if got := NewMessages(store).Mode(ctx, "repow-b"); got != MessageDeliveryPull {
		t.Fatalf("mode after restart = %q", got)
	}
	var nilMessages *Messages
	if got := nilMessages.Mode(ctx, "repow-b"); got != MessageDeliveryPush {
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
	if live, restarted := m.Mode(ctx, "repow-b"), NewMessages(store).Mode(ctx, "repow-b"); live != restarted {
		t.Fatalf("cache says %s, store says %s", live, restarted)
	}
}
