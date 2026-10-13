package state

import (
	"context"
	"path/filepath"
	"testing"
)

func newInboxStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestInboxItemRoundTripAndReadState(t *testing.T) {
	ctx := context.Background()
	store := newInboxStore(t)
	item := &InboxItem{Kind: "ask", TraceID: "ask-1", TraceKind: "ask", FromPeerID: "repow-1-a", FromName: "alpha",
		ToPeerID: "repow-h", ToName: "human", CorrelationID: "ask-1", Text: "ship it?",
		Question: map[string]any{"kind": "choice"}, Attachments: []map[string]any{{"id": "att-1"}}}
	if err := store.InsertInboxItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if item.ID == "" || item.CreatedAt == "" || item.Direction != InboxIn {
		t.Fatalf("defaults not minted: %+v", item)
	}
	got, err := store.InboxItem(ctx, item.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Text != "ship it?" || got.CorrelationID != "ask-1" || got.Question["kind"] != "choice" || got.Attachments[0]["id"] != "att-1" {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	unread, err := store.ListInbox(ctx, InboxFilter{UnreadOnly: true})
	if err != nil || len(unread) != 1 {
		t.Fatalf("unread = %d, %v", len(unread), err)
	}
	if changed, err := store.MarkInboxRead(ctx, item.ID, "telegram"); err != nil || !changed {
		t.Fatalf("first read: %v %v", changed, err)
	}
	if changed, _ := store.MarkInboxRead(ctx, item.ID, "cli"); changed {
		t.Fatal("second read must be a no-op: read is once, for every surface")
	}
	if unread, _ := store.ListInbox(ctx, InboxFilter{UnreadOnly: true}); len(unread) != 0 {
		t.Fatalf("read item still unread: %+v", unread)
	}
	if changed, _ := store.MarkInboxReplied(ctx, item.ID, "dashboard"); !changed {
		t.Fatal("reply not recorded")
	}
	got, _ = store.InboxItem(ctx, item.ID)
	if got.ReadSurface != "telegram" || got.ReplySurface != "dashboard" || got.RepliedAt == "" {
		t.Fatalf("read/replied surfaces wrong: %+v", got)
	}
}

func TestInboxPushesThreadRefAndLookupBySurfaceRef(t *testing.T) {
	ctx := context.Background()
	store := newInboxStore(t)
	out := &InboxItem{Direction: InboxOut, Kind: "ask", TraceID: "ask-9", TraceKind: "ask", CorrelationID: "ask-9", Text: "status?"}
	reply := &InboxItem{Kind: "answer", TraceID: "ask-9", TraceKind: "ask", CorrelationID: "ask-9", Text: "green", CreatedAt: "2099-01-01T00:00:00.000Z"}
	for _, item := range []*InboxItem{out, reply} {
		if err := store.InsertInboxItem(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordInboxPush(ctx, out.ID, InboxPush{Surface: "telegram", Status: PushPushed, SurfaceRef: "100"}); err != nil {
		t.Fatal(err)
	}
	ref, err := store.InboxThreadRef(ctx, "ask-9", "telegram")
	if err != nil || ref != "100" {
		t.Fatalf("thread ref = %q, %v; want the earliest rendered message", ref, err)
	}
	if ref, _ := store.InboxThreadRef(ctx, "ask-9", "slack"); ref != "" {
		t.Fatalf("thread never rendered on slack, got %q", ref)
	}

	// Upsert: a retry outcome replaces the earlier record for the surface.
	if err := store.RecordInboxPush(ctx, reply.ID, InboxPush{Surface: "telegram", Status: PushFailed, ErrorClass: "network"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordInboxPush(ctx, reply.ID, InboxPush{Surface: "telegram", Status: PushPushed, SurfaceRef: "101"}); err != nil {
		t.Fatal(err)
	}
	found, err := store.InboxItemBySurfaceRef(ctx, "telegram", "101")
	if err != nil || found == nil || found.ID != reply.ID {
		t.Fatalf("lookup by ref = %+v, %v", found, err)
	}
	if len(found.Pushes) != 1 || found.Pushes[0].Status != PushPushed || found.Pushes[0].ErrorClass != "" {
		t.Fatalf("push not upserted: %+v", found.Pushes)
	}
	if missing, err := store.InboxItemBySurfaceRef(ctx, "telegram", "999"); err != nil || missing != nil {
		t.Fatalf("unknown ref = %+v, %v", missing, err)
	}

	// Outbound items never show in the human's inbox listing.
	all, _ := store.ListInbox(ctx, InboxFilter{})
	if len(all) != 1 || all[0].ID != reply.ID {
		t.Fatalf("list should hold only the inbound reply: %+v", all)
	}
}
