package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPeerMessageRoundTripAndReadState(t *testing.T) {
	ctx := context.Background()
	store := newInboxStore(t)
	msg := &PeerMessage{Kind: MessageAsk, PeerID: "repow-1-b", PeerName: "bob", FromPeerID: "repow-1-a", FromName: "alice",
		CorrelationID: "ask-1", TraceID: "ask-1", Text: "review #12?",
		Attachments: []map[string]any{{"id": "att-1"}}, Question: map[string]any{"kind": "text"}}
	if err := store.InsertPeerMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg.ID, "msg-") || msg.CreatedAt == "" {
		t.Fatalf("defaults not minted: %+v", msg)
	}
	got, err := store.PeerMessage(ctx, msg.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Text != "review #12?" || got.CorrelationID != "ask-1" || got.FromName != "alice" ||
		got.Attachments[0]["id"] != "att-1" || got.Question["kind"] != "text" || got.ReadAt != "" {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	changed, err := store.MarkPeerMessageRead(ctx, msg.ID)
	if err != nil || !changed {
		t.Fatalf("first read: changed=%v err=%v", changed, err)
	}
	if changed, _ := store.MarkPeerMessageRead(ctx, msg.ID); changed {
		t.Fatal("second read changed the row")
	}
	if got, _ := store.PeerMessage(ctx, msg.ID); got.ReadAt == "" {
		t.Fatal("read_at not set")
	}
	if missing, err := store.PeerMessage(ctx, "msg-unknown"); missing != nil || err != nil {
		t.Fatalf("unknown id: %v %v", missing, err)
	}
}

func TestListPeerMessagesScopesToRecipient(t *testing.T) {
	ctx := context.Background()
	store := newInboxStore(t)
	base := time.Now().UTC()
	for i, m := range []PeerMessage{
		{Kind: MessageNotify, PeerID: "repow-b", Text: "first"},
		{Kind: MessageNotify, PeerID: "repow-c", Text: "for carol"},
		{Kind: MessageReply, PeerID: "repow-b", Text: "second"},
		{Kind: MessageAsk, PeerID: "repow-b", Text: "third"},
	} {
		m.CreatedAt = base.Add(time.Duration(i) * time.Second).Format(tsLayout)
		if err := store.InsertPeerMessage(ctx, &m); err != nil {
			t.Fatal(err)
		}
		if m.Text == "first" {
			if _, err := store.MarkPeerMessageRead(ctx, m.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	texts := func(f PeerMessageFilter) string {
		t.Helper()
		msgs, err := store.ListPeerMessages(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range msgs {
			out = append(out, m.Text)
		}
		return strings.Join(out, ",")
	}
	if got := texts(PeerMessageFilter{PeerID: "repow-b"}); got != "third,second,first" {
		t.Fatalf("all = %q", got)
	}
	if got := texts(PeerMessageFilter{PeerID: "repow-b", UnreadOnly: true}); got != "third,second" {
		t.Fatalf("unread = %q", got)
	}
	if got := texts(PeerMessageFilter{PeerID: "repow-b", Limit: 1}); got != "third" {
		t.Fatalf("limit = %q", got)
	}
	if got := texts(PeerMessageFilter{PeerID: "repow-nobody"}); got != "" {
		t.Fatalf("other peer = %q", got)
	}
}

func TestDeletePeerMessagesBefore(t *testing.T) {
	ctx := context.Background()
	store := newInboxStore(t)
	now := time.Now().UTC()
	old := &PeerMessage{Kind: MessageNotify, PeerID: "repow-b", Text: "old", CreatedAt: now.Add(-8 * 24 * time.Hour).Format(tsLayout)}
	fresh := &PeerMessage{Kind: MessageNotify, PeerID: "repow-b", Text: "fresh"}
	for _, m := range []*PeerMessage{old, fresh} {
		if err := store.InsertPeerMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.DeletePeerMessagesBefore(ctx, now.Add(-7*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, err %v", n, err)
	}
	if got, _ := store.PeerMessage(ctx, old.ID); got != nil {
		t.Fatal("old message survived")
	}
	if got, _ := store.PeerMessage(ctx, fresh.ID); got == nil {
		t.Fatal("fresh message deleted")
	}
}
