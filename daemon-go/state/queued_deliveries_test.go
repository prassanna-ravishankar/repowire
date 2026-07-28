package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// queuedDeliveriesDDL is copied verbatim from repowire/daemon/state/database.py
// (the queued_deliveries table + its two indexes), plus the user_version stamp
// NewStore requires.
const queuedDeliveriesDDL = `
CREATE TABLE IF NOT EXISTS queued_deliveries (
    delivery_id TEXT PRIMARY KEY,
    peer_id TEXT NOT NULL,
    repowire_session_id TEXT,
    kind TEXT NOT NULL,
    from_peer_id TEXT,
    from_peer_name TEXT NOT NULL,
    to_peer_name TEXT NOT NULL,
    correlation_id TEXT,
    text TEXT NOT NULL,
    attachments_json TEXT NOT NULL DEFAULT '[]',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queued_deliveries_peer_created
    ON queued_deliveries(peer_id, created_at);
CREATE INDEX IF NOT EXISTS idx_queued_deliveries_expires
    ON queued_deliveries(expires_at);
PRAGMA user_version=12;
`

func newQDStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "qd.db")

	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(queuedDeliveriesDDL); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func strPtr(s string) *string { return &s }

func sampleDelivery(peerID, text string) QueuedDelivery {
	return QueuedDelivery{
		PeerID:            peerID,
		RepowireSessionID: strPtr("sess-1"),
		Kind:              DeliveryNotify,
		FromPeerID:        strPtr("from-id"),
		FromPeerName:      "alice",
		ToPeerName:        "bob",
		CorrelationID:     strPtr("cid-1"),
		Text:              text,
		Attachments:       []map[string]any{{"name": "a.txt"}},
		Metadata:          map[string]any{"k": "v"},
	}
}

func TestEnqueueAndDrainRoundTrip(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	enq, err := s.EnqueueDelivery(ctx, sampleDelivery("bob", "hello"), 3600, 10, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if enq == nil {
		t.Fatal("expected delivery, got nil")
	}
	if enq.DeliveryID == "" || enq.DeliveryID[:3] != "qd-" {
		t.Fatalf("bad delivery id %q", enq.DeliveryID)
	}

	got, err := s.DrainDeliveries(ctx, "bob", 50, now.Add(time.Second))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("drained %d, want 1", len(got))
	}
	d := got[0]
	if d.Text != "hello" || d.Kind != DeliveryNotify || d.FromPeerName != "alice" || d.ToPeerName != "bob" {
		t.Fatalf("round-trip mismatch: %+v", d)
	}
	if d.RepowireSessionID == nil || *d.RepowireSessionID != "sess-1" {
		t.Fatalf("session id mismatch: %+v", d.RepowireSessionID)
	}
	if d.FromPeerID == nil || *d.FromPeerID != "from-id" {
		t.Fatalf("from peer id mismatch: %+v", d.FromPeerID)
	}
	if d.CorrelationID == nil || *d.CorrelationID != "cid-1" {
		t.Fatalf("correlation id mismatch: %+v", d.CorrelationID)
	}
	if len(d.Attachments) != 1 || d.Attachments[0]["name"] != "a.txt" {
		t.Fatalf("attachments mismatch: %+v", d.Attachments)
	}
	if d.Metadata["k"] != "v" {
		t.Fatalf("metadata mismatch: %+v", d.Metadata)
	}

	// Drain deletes — second drain is empty.
	again, err := s.DrainDeliveries(ctx, "bob", 50, now.Add(time.Second))
	if err != nil {
		t.Fatalf("drain again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("expected drain to delete, got %d", len(again))
	}
}

func TestListDoesNotDelete(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if _, err := s.EnqueueDelivery(ctx, sampleDelivery("bob", "x"), 3600, 10, now); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < 2; i++ {
		got, err := s.ListDeliveries(ctx, "bob", 50, now.Add(time.Second))
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("list iter %d: got %d, want 1", i, len(got))
		}
	}
}

func TestExpiryEviction(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 1s TTL.
	if _, err := s.EnqueueDelivery(ctx, sampleDelivery("bob", "old"), 1, 10, now); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Past expiry: list should evict + return nothing.
	got, err := s.ListDeliveries(ctx, "bob", 50, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected expiry eviction, got %d", len(got))
	}
}

func TestEnforceCapKeepsNewest(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Insert 3 with cap 2; created_at must be strictly increasing.
	for i, txt := range []string{"first", "second", "third"} {
		when := base.Add(time.Duration(i) * time.Second)
		if _, err := s.EnqueueDelivery(ctx, sampleDelivery("bob", txt), 3600, 2, when); err != nil {
			t.Fatalf("enqueue %s: %v", txt, err)
		}
	}

	got, err := s.ListDeliveries(ctx, "bob", 50, base.Add(10*time.Second))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("cap not enforced: got %d, want 2", len(got))
	}
	// Oldest ("first") should have been dropped; newest two remain, oldest-first.
	if got[0].Text != "second" || got[1].Text != "third" {
		t.Fatalf("cap kept wrong rows: %q, %q", got[0].Text, got[1].Text)
	}
}

func TestEnqueueDisabledWhenCapOrTTLZero(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	d, err := s.EnqueueDelivery(ctx, sampleDelivery("bob", "x"), 0, 10, now)
	if err != nil || d != nil {
		t.Fatalf("ttl=0 should no-op: d=%v err=%v", d, err)
	}
	d, err = s.EnqueueDelivery(ctx, sampleDelivery("bob", "x"), 3600, 0, now)
	if err != nil || d != nil {
		t.Fatalf("cap=0 should no-op: d=%v err=%v", d, err)
	}
}

func TestDeleteDelivery(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	enq, err := s.EnqueueDelivery(ctx, sampleDelivery("bob", "x"), 3600, 10, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ok, err := s.DeleteDelivery(ctx, enq.DeliveryID)
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	ok, err = s.DeleteDelivery(ctx, enq.DeliveryID)
	if err != nil {
		t.Fatalf("second delete err: %v", err)
	}
	if ok {
		t.Fatal("second delete should report no row")
	}
}

func TestDrainNullableFieldsRoundTrip(t *testing.T) {
	s := newQDStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	d := QueuedDelivery{
		PeerID:       "bob",
		Kind:         DeliveryAsk,
		FromPeerName: "alice",
		ToPeerName:   "bob",
		Text:         "ping",
		// session/from/correlation/attachments/metadata all left nil.
	}
	if _, err := s.EnqueueDelivery(ctx, d, 3600, 10, now); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.DrainDeliveries(ctx, "bob", 50, now.Add(time.Second))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("drained %d, want 1", len(got))
	}
	r := got[0]
	if r.RepowireSessionID != nil || r.FromPeerID != nil || r.CorrelationID != nil {
		t.Fatalf("expected nil pointers, got %+v", r)
	}
	if r.Attachments == nil || len(r.Attachments) != 0 {
		t.Fatalf("attachments should be empty slice, got %+v", r.Attachments)
	}
	if r.Metadata == nil || len(r.Metadata) != 0 {
		t.Fatalf("metadata should be empty map, got %+v", r.Metadata)
	}
	if r.Kind != DeliveryAsk {
		t.Fatalf("kind mismatch: %q", r.Kind)
	}
}
