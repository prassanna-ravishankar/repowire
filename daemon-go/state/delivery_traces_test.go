package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// deliveryTracesDDL is the delivery_traces table copied verbatim from
// repowire/daemon/state/database.py (schema-v12), with user_version stamped so
// NewStore opens it.
const deliveryTracesDDL = `
CREATE TABLE IF NOT EXISTS delivery_traces (
    id TEXT PRIMARY KEY,
    trace_id TEXT NOT NULL,
    delivery_id TEXT,
    seq INTEGER NOT NULL,
    kind TEXT NOT NULL,
    stage TEXT NOT NULL,
    status TEXT NOT NULL,
    peer_id TEXT,
    from_peer_id TEXT,
    ts TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_delivery_traces_trace ON delivery_traces(trace_id, seq);
CREATE INDEX IF NOT EXISTS idx_delivery_traces_peer_stage ON delivery_traces(peer_id, stage, ts DESC);
CREATE INDEX IF NOT EXISTS idx_delivery_traces_ts ON delivery_traces(ts);
PRAGMA user_version=12;
`

func newTraceStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(deliveryTracesDDL); err != nil {
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

func TestRecordTraceSeqAndStagesFor(t *testing.T) {
	s := newTraceStore(t)
	ctx := context.Background()

	// Three stages for one ask trace; seq must monotonically increase 0,1,2.
	if err := s.RecordTrace(ctx, "cid-1", "ask", "created", "", "", "peer-a", "peer-b", nil); err != nil {
		t.Fatalf("record created: %v", err)
	}
	if err := s.RecordTrace(ctx, "cid-1", "ask", "routed", "", "", "peer-a", "peer-b",
		map[string]any{"transport": "ws"}); err != nil {
		t.Fatalf("record routed: %v", err)
	}
	if err := s.RecordTrace(ctx, "cid-1", "ask", "injection_failed", "fail", "", "peer-a", "peer-b",
		map[string]any{"hook_status": "rejected"}); err != nil {
		t.Fatalf("record injection_failed: %v", err)
	}
	// An unrelated trace must not perturb cid-1's seq.
	if err := s.RecordTrace(ctx, "cid-2", "notify", "created", "", "", "peer-c", "", nil); err != nil {
		t.Fatalf("record other trace: %v", err)
	}

	stages, err := s.StagesFor(ctx, "cid-1")
	if err != nil {
		t.Fatalf("StagesFor: %v", err)
	}
	if len(stages) != 3 {
		t.Fatalf("got %d stages, want 3", len(stages))
	}
	wantStages := []string{"created", "routed", "injection_failed"}
	for i, st := range stages {
		if st.Seq != i {
			t.Errorf("stage %d seq = %d, want %d", i, st.Seq, i)
		}
		if st.Stage != wantStages[i] {
			t.Errorf("stage %d = %q, want %q", i, st.Stage, wantStages[i])
		}
		// delivery_id defaults to trace_id when not supplied.
		if st.DeliveryID != "cid-1" {
			t.Errorf("stage %d delivery_id = %q, want cid-1", i, st.DeliveryID)
		}
	}

	// status defaults to "ok"; explicit "fail" preserved.
	if stages[0].Status != "ok" {
		t.Errorf("created status = %q, want ok", stages[0].Status)
	}
	if stages[2].Status != "fail" {
		t.Errorf("injection_failed status = %q, want fail", stages[2].Status)
	}
	// detail round-trips; empty detail becomes {}.
	if stages[0].Detail == nil || len(stages[0].Detail) != 0 {
		t.Errorf("created detail = %v, want empty map", stages[0].Detail)
	}
	if stages[1].Detail["transport"] != "ws" {
		t.Errorf("routed detail = %v", stages[1].Detail)
	}
	// from_peer empty stays empty (stored NULL); peer_id preserved.
	if stages[0].PeerID != "peer-a" {
		t.Errorf("peer_id = %q, want peer-a", stages[0].PeerID)
	}
}

func TestStagesForUnknownTraceEmpty(t *testing.T) {
	s := newTraceStore(t)
	stages, err := s.StagesFor(context.Background(), "nope")
	if err != nil {
		t.Fatalf("StagesFor: %v", err)
	}
	if len(stages) != 0 {
		t.Errorf("got %d stages, want 0", len(stages))
	}
}

func TestNullPeerStoredAsNull(t *testing.T) {
	s := newTraceStore(t)
	ctx := context.Background()
	if err := s.RecordTrace(ctx, "t", "notify", "created", "", "", "", "", nil); err != nil {
		t.Fatalf("RecordTrace: %v", err)
	}
	var peerID, fromPeerID sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT peer_id, from_peer_id FROM delivery_traces`).Scan(&peerID, &fromPeerID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if peerID.Valid {
		t.Errorf("empty peer_id should be NULL, got %q", peerID.String)
	}
	if fromPeerID.Valid {
		t.Errorf("empty from_peer_id should be NULL, got %q", fromPeerID.String)
	}
}

func TestLatestStagesForPeers(t *testing.T) {
	s := newTraceStore(t)
	ctx := context.Background()

	// peer-a: an older pane_injected then a newer one -> MAX(ts) wins.
	mustInsert(t, s, "tr1", "ask", "pane_injected", "peer-a", "2026-06-01T00:00:00.000Z")
	mustInsert(t, s, "tr2", "ask", "pane_injected", "peer-a", "2026-06-29T00:00:00.000Z")
	// peer-a also had a failure.
	mustInsert(t, s, "tr3", "ask", "injection_failed", "peer-a", "2026-06-15T00:00:00.000Z")
	// peer-b only a success.
	mustInsert(t, s, "tr4", "notify", "pane_injected", "peer-b", "2026-06-10T00:00:00.000Z")
	// A non-terminal stage is ignored by the default-stage query.
	mustInsert(t, s, "tr5", "ask", "routed", "peer-a", "2026-06-30T00:00:00.000Z")

	got, err := s.LatestStagesForPeers(ctx, []string{"peer-a", "peer-b"})
	if err != nil {
		t.Fatalf("LatestStagesForPeers: %v", err)
	}
	if v := got[[2]string{"peer-a", "pane_injected"}]; v != "2026-06-29T00:00:00.000Z" {
		t.Errorf("peer-a pane_injected = %q, want newest", v)
	}
	if v := got[[2]string{"peer-a", "injection_failed"}]; v != "2026-06-15T00:00:00.000Z" {
		t.Errorf("peer-a injection_failed = %q", v)
	}
	if v := got[[2]string{"peer-b", "pane_injected"}]; v != "2026-06-10T00:00:00.000Z" {
		t.Errorf("peer-b pane_injected = %q", v)
	}
	// "routed" is not a default inbound stage, so it must be absent.
	if _, ok := got[[2]string{"peer-a", "routed"}]; ok {
		t.Error("routed should not appear under default stages")
	}
	if _, ok := got[[2]string{"peer-b", "injection_failed"}]; ok {
		t.Error("peer-b never failed; key should be absent")
	}
}

func TestLatestStagesForPeersEmptyInput(t *testing.T) {
	s := newTraceStore(t)
	got, err := s.LatestStagesForPeers(context.Background(), nil)
	if err != nil {
		t.Fatalf("LatestStagesForPeers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty peer list should yield empty map, got %v", got)
	}
}

func TestLatestStagesForPeersCustomStages(t *testing.T) {
	s := newTraceStore(t)
	ctx := context.Background()
	mustInsert(t, s, "tr1", "ask", "websocket_sent", "peer-x", "2026-06-01T00:00:00.000Z")
	mustInsert(t, s, "tr2", "ask", "pane_injected", "peer-x", "2026-06-02T00:00:00.000Z")

	got, err := s.LatestStagesForPeers(ctx, []string{"peer-x"}, "websocket_sent")
	if err != nil {
		t.Fatalf("LatestStagesForPeers: %v", err)
	}
	if v := got[[2]string{"peer-x", "websocket_sent"}]; v != "2026-06-01T00:00:00.000Z" {
		t.Errorf("websocket_sent = %q", v)
	}
	if _, ok := got[[2]string{"peer-x", "pane_injected"}]; ok {
		t.Error("pane_injected should be excluded when only websocket_sent requested")
	}
}

// mustInsert writes a delivery_traces row directly with a fixed ts (bypassing
// RecordTrace's now()) so MAX(ts) ordering is deterministic.
func mustInsert(t *testing.T, s *Store, traceID, kind, stage, peerID, ts string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO delivery_traces(id, trace_id, delivery_id, seq, kind, stage, status, peer_id, ts, detail_json)
		 VALUES (?, ?, ?, 0, ?, ?, 'ok', ?, ?, '{}')`,
		traceID+"-id", traceID, traceID, kind, stage, peerID, ts,
	)
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}
}
