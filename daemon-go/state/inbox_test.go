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
	// Every chunk a surface rendered maps back to the item; the first stays
	// the thread anchor.
	if err := store.RecordInboxPush(ctx, reply.ID, InboxPush{Surface: "telegram", Status: PushPushed, SurfaceRef: "101", Refs: []string{"102", "103"}}); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"101", "102", "103"} {
		if got, _ := store.InboxItemBySurfaceRef(ctx, "telegram", chunk); got == nil || got.ID != reply.ID {
			t.Fatalf("chunk %s does not find the item: %+v", chunk, got)
		}
	}
	if got, _ := store.InboxItemBySurfaceRef(ctx, "slack", "102"); got != nil {
		t.Fatalf("refs are per surface: %+v", got)
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

// TestListInboxBySurfaceThroughRef: the filter a surface uses to mark what it
// showed as read finds only items it pushed, at or before a ref, however many
// other unread items are newer.
func TestListInboxBySurfaceThroughRef(t *testing.T) {
	ctx := context.Background()
	store := newInboxStore(t)
	add := func(surface, status, ref string) string {
		item := &InboxItem{Kind: "notify", FromName: "alpha", Text: "x"}
		if err := store.InsertInboxItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		if surface != "" {
			if err := store.RecordInboxPush(ctx, item.ID, InboxPush{Surface: surface, Status: status, SurfaceRef: ref}); err != nil {
				t.Fatal(err)
			}
		}
		return item.ID
	}
	shown := add("telegram", PushPushed, "100")
	for n := 0; n < 5; n++ {
		add("dashboard", PushPushed, "")
	}
	add("telegram", PushFailed, "101")
	later := add("telegram", PushPushed, "205")
	add("telegram", PushPushed, "abc")

	ids := func(f InboxFilter) []string {
		items, err := store.ListInbox(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	if got := ids(InboxFilter{UnreadOnly: true, Surface: "telegram", ThroughRef: 200, Limit: 1}); len(got) != 1 || got[0] != shown {
		t.Fatalf("through 200 = %v, want only %s", got, shown)
	}
	if got := ids(InboxFilter{UnreadOnly: true, Surface: "telegram", ThroughRef: 300}); len(got) != 2 || got[0] != later {
		t.Fatalf("through 300 = %v", got)
	}
	if got := ids(InboxFilter{UnreadOnly: true, Surface: "telegram"}); len(got) != 3 {
		t.Fatalf("any telegram push = %v", got)
	}
}
