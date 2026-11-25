package state

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// Every message that still carries an attachment names it: an agent's
// message, the human's inbox item, a queued delivery. Retention stripping a
// message's content drops its references with it.
func TestAttachmentReferencesAreTheMessagesStillCarryingThem(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	att := func(id string) []map[string]any {
		return []map[string]any{{"id": id, "path": "/home/x/.repowire/attachments/" + id + ".png", "filename": "shot.png"}}
	}
	for _, m := range []PeerMessage{
		{Kind: MessageAsk, PeerID: "repow-b", Text: "see this", Attachments: att("new-msg"), CreatedAt: ago(1 * day)},
		{Kind: MessageNotify, PeerID: "repow-b", Text: "and this", Attachments: att("old-msg"), CreatedAt: ago(8 * day)},
		{Kind: MessageNotify, PeerID: "repow-b", Text: "none"},
	} {
		if err := s.InsertPeerMessage(ctx, &m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertInboxItem(ctx, &InboxItem{Direction: "in", Kind: "ask", Text: "for you", CreatedAt: ago(8 * day),
		Attachments: att("old-inbox")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueDelivery(ctx, QueuedDelivery{PeerID: "repow-b", Text: "later",
		// Not an upload's shape: an object with only a path.
		Attachments: []map[string]any{{"path": "/tmp/only-path.txt"}}}, 3600, 10, sweepNow); err != nil {
		t.Fatal(err)
	}
	// A malformed column names nothing rather than failing the lookup.
	s.mustExec(t, `INSERT INTO peer_messages(id, kind, peer_id, peer_name, from_peer_id, from_name, trace_id, text, attachments_json, created_at)
		VALUES ('msg-bad', 'notify', 'repow-b', 'b', '', '', '', '', '["just-a-string", 7]', ?)`, ago(1*day))

	names := func() []string {
		refs, err := s.AttachmentReferences(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range refs {
			out = append(out, r.ID+"|"+r.Path)
		}
		sort.Strings(out)
		return out
	}
	all := []string{
		"new-msg|/home/x/.repowire/attachments/new-msg.png",
		"old-inbox|/home/x/.repowire/attachments/old-inbox.png",
		"old-msg|/home/x/.repowire/attachments/old-msg.png",
		"|/tmp/only-path.txt",
	}
	if got := names(); !reflect.DeepEqual(got, all) {
		t.Fatalf("references %q, want %q", got, all)
	}

	if _, err := s.Sweep(ctx, defaultPolicy, sweepNow); err != nil {
		t.Fatal(err)
	}
	kept := []string{all[0], all[3]}
	if got := names(); !reflect.DeepEqual(got, kept) {
		t.Fatalf("after the sweep: references %q, want %q", got, kept)
	}
}
