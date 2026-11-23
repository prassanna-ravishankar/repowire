package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

var sweepNow = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

func newSweepStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (s *Store) mustExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (s *Store) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// ago is an ISO timestamp d before sweepNow, in the daemon's own layout.
func ago(d time.Duration) string { return sweepNow.Add(-d).Format(tsLayout) }

const day = 24 * time.Hour

var defaultPolicy = RetentionPolicy{Content: 7 * day, Records: 180 * day}

func seedEvent(t *testing.T, s *Store, id, typ, ts, payload string) {
	t.Helper()
	s.mustExec(t, `INSERT INTO events(event_id, type, timestamp, payload_json) VALUES (?, ?, ?, ?)`, id, typ, ts, payload)
}

func seedTrace(t *testing.T, s *Store, id, stage, ts, detail string) {
	t.Helper()
	s.mustExec(t, `INSERT INTO delivery_traces(id, trace_id, seq, kind, stage, status, peer_id, from_peer_id, ts, detail_json)
		VALUES (?, ?, 1, 'ask', ?, 'ok', 'repow-b', 'repow-a', ?, ?)`, id, "ask-"+id, stage, ts, detail)
}

func TestSweepStripsContentAndKeepsRecords(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	old, fresh := ago(8*day), ago(6*day)

	seedTrace(t, s, "t-old", "created", old, `{"text":"secret","from_peer":"a","to_peer":"b"}`)
	seedTrace(t, s, "t-closed", "closed", old, `{"reply":"answer","close_reason":"acked","recipient_turns":2}`)
	seedTrace(t, s, "t-route", "route_decision", old, `{"request":{"text":"secret","text_sha256":"abc"},"arm":"static"}`)
	seedTrace(t, s, "t-fresh", "created", fresh, `{"text":"recent"}`)
	seedTrace(t, s, "t-meta", "acked", old, `{"close_reason":"acked"}`)

	seedEvent(t, s, "e-ask", "ask", old, `{"text":"secret","correlation_id":"ask-1","from_peer_id":"a","to_peer_id":"b","attachments":[{"id":"x"}]}`)
	seedEvent(t, s, "e-note", "notification", old, `{"text":"secret","from":"a","to":"b"}`)
	seedEvent(t, s, "e-deleg", "ask_delegated", old, `{"note":"secret","correlation_id":"ask-1"}`)
	seedEvent(t, s, "e-chat", "chat_turn", old, `{"text":"transcript","role":"assistant"}`)
	seedEvent(t, s, "e-chat-new", "chat_turn", fresh, `{"text":"transcript"}`)
	seedEvent(t, s, "e-status", "peer_status", old, `{"status":"online"}`)

	s.mustExec(t, `INSERT INTO peer_messages(id, kind, peer_id, text, created_at) VALUES ('msg-old', 'notify', 'repow-b', 'secret', ?), ('msg-new', 'notify', 'repow-b', 'recent', ?)`, old, fresh)
	s.mustExec(t, `INSERT INTO inbox_items(id, direction, kind, trace_id, trace_kind, text, attachments_json, question_json, created_at)
		VALUES ('inb-old', 'in', 'ask', 'ask-1', 'ask', 'secret', '[{"id":"x"}]', '{"kind":"text"}', ?)`, old)

	report := sweepMatchingDryRun(t, s, defaultPolicy)

	for id, wantGone := range map[string]bool{"msg-old": true, "msg-new": false} {
		if gone := s.count(t, `SELECT COUNT(*) FROM peer_messages WHERE id = ?`, id) == 0; gone != wantGone {
			t.Errorf("peer_messages %s gone=%v, want %v", id, gone, wantGone)
		}
	}
	for id, wantGone := range map[string]bool{"e-chat": true, "e-chat-new": false} {
		if gone := s.count(t, `SELECT COUNT(*) FROM events WHERE event_id = ?`, id) == 0; gone != wantGone {
			t.Errorf("event %s gone=%v, want %v", id, gone, wantGone)
		}
	}
	// Stripped rows keep their metadata and say so.
	if n := s.count(t, `SELECT COUNT(*) FROM delivery_traces WHERE json_extract(detail_json, '$.content_expired') = 1
		AND json_type(detail_json, '$.text') IS NULL AND json_type(detail_json, '$.reply') IS NULL
		AND json_type(detail_json, '$.request.text') IS NULL`); n != 3 {
		t.Errorf("stripped traces = %d, want 3", n)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM delivery_traces WHERE json_extract(detail_json, '$.request.text_sha256') = 'abc'
		AND json_extract(detail_json, '$.close_reason') IS NULL`); n != 1 {
		t.Error("route_decision lost its metadata")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM delivery_traces WHERE id = 't-fresh' AND json_extract(detail_json, '$.text') = 'recent'`); n != 1 {
		t.Error("fresh trace text was stripped")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM delivery_traces WHERE id = 't-meta' AND json_type(detail_json, '$.content_expired') IS NULL`); n != 1 {
		t.Error("a trace with no content was rewritten")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE event_id IN ('e-ask', 'e-note', 'e-deleg') AND json_extract(payload_json, '$.content_expired') = 1
		AND json_type(payload_json, '$.text') IS NULL AND json_type(payload_json, '$.note') IS NULL AND json_type(payload_json, '$.attachments') IS NULL`); n != 3 {
		t.Errorf("stripped events = %d, want 3", n)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events WHERE event_id = 'e-status' AND json_type(payload_json, '$.content_expired') IS NULL`); n != 1 {
		t.Error("a non-message event was rewritten")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM inbox_items WHERE id = 'inb-old' AND text = '' AND attachments_json = '[]' AND question_json IS NULL`); n != 1 {
		t.Error("inbox item content not stripped")
	}

	// Ask history reads peer ids, not text: unchanged by stripping.
	refs, err := s.AskRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 { // e-ask (journal) + t-old, t-fresh (ledger "created")
		t.Fatalf("ask refs after strip = %+v", refs)
	}

	if report.Counts["peer_messages.delete"] != 1 || report.Counts["events.delete_transcripts"] != 1 || report.Counts["delivery_traces.strip"] != 3 {
		t.Fatalf("report = %+v", report.Counts)
	}

	// A second sweep finds nothing left to do.
	again, err := s.Sweep(ctx, defaultPolicy, sweepNow, false)
	if err != nil || again.Total() != 0 {
		t.Fatalf("second sweep = %+v, %v", again.Counts, err)
	}
}

func TestSweepDeletesRecordsAndSparesLiveState(t *testing.T) {
	s := newSweepStore(t)
	old, fresh := ago(181*day), ago(179*day)

	seedTrace(t, s, "t-old", "acked", old, `{}`)
	seedTrace(t, s, "t-fresh", "acked", fresh, `{}`)
	seedEvent(t, s, "e-old", "peer_status", old, `{}`)
	seedEvent(t, s, "e-fresh", "peer_status", fresh, `{}`)
	s.mustExec(t, `INSERT INTO inbox_items(id, direction, kind, trace_id, trace_kind, created_at) VALUES ('inb-old', 'in', 'notify', 'n', 'notify', ?), ('inb-new', 'in', 'notify', 'n', 'notify', ?)`, old, fresh)
	s.mustExec(t, `INSERT INTO inbox_pushes(item_id, surface, status, ts) VALUES ('inb-old', 'telegram', 'ok', ?), ('inb-new', 'telegram', 'ok', ?)`, old, fresh)
	s.mustExec(t, `INSERT INTO inbox_refs(surface, ref, item_id) VALUES ('telegram', '1', 'inb-old'), ('telegram', '2', 'inb-new')`)
	for _, w := range []struct{ id, state, completed, updated string }{
		{"job-done-old", "completed", old, old},
		{"job-failed-old-nocompleted", "failed", "", old},
		{"job-running-old", "running", "", old},
		{"job-done-new", "completed", fresh, fresh},
	} {
		var completed any
		if w.completed != "" {
			completed = w.completed
		}
		s.mustExec(t, `INSERT INTO tracked_work(work_id, state, completed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, w.id, w.state, completed, w.updated, w.updated)
	}
	s.mustExec(t, `INSERT INTO operations(operation_id, kind, state, created_at, updated_at, completed_at) VALUES
		('op-old', 'restart', 'completed', ?, ?, ?), ('op-open', 'restart', 'running', ?, ?, NULL)`, old, old, old, old, old)
	s.mustExec(t, `INSERT INTO runtime_identity_certificates(nonce, peer_id, display_name, backend, project_path, issued_at, expires_at) VALUES
		('c-old', 'p', 'n', 'claude-code', '/x', ?, ?), ('c-new', 'p', 'n', 'claude-code', '/x', ?, ?)`, old, old, fresh, fresh)

	sweepMatchingDryRun(t, s, defaultPolicy)
	remaining := func(table, key string) map[string]bool {
		rows, err := s.db.Query(`SELECT ` + key + ` FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out[id] = true
		}
		return out
	}
	for _, c := range []struct {
		table, key string
		want       []string
	}{
		{"delivery_traces", "id", []string{"t-fresh"}},
		{"events", "event_id", []string{"e-fresh"}},
		{"inbox_items", "id", []string{"inb-new"}},
		{"inbox_pushes", "item_id", []string{"inb-new"}},
		{"inbox_refs", "item_id", []string{"inb-new"}},
		{"tracked_work", "work_id", []string{"job-running-old", "job-done-new"}},
		{"operations", "operation_id", []string{"op-open"}},
		{"runtime_identity_certificates", "nonce", []string{"c-new"}},
	} {
		got := remaining(c.table, c.key)
		if len(got) != len(c.want) {
			t.Errorf("%s kept %v, want %v", c.table, got, c.want)
			continue
		}
		for _, id := range c.want {
			if !got[id] {
				t.Errorf("%s kept %v, want %v", c.table, got, c.want)
			}
		}
	}
}

func TestSweepZeroKeepsForeverAndRecordsBoundContent(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	ancient := ago(1000 * day)
	seedEvent(t, s, "e-chat", "chat_turn", ancient, `{"text":"x"}`)
	seedTrace(t, s, "t-old", "created", ancient, `{"text":"x"}`)

	report, err := s.Sweep(ctx, RetentionPolicy{}, sweepNow, false)
	if err != nil || report.Total() != 0 {
		t.Fatalf("keep-forever sweep changed %+v (%v)", report.Counts, err)
	}

	// Content kept forever, records 30 days: content goes with its record.
	recent := ago(40 * day)
	seedEvent(t, s, "e-chat-40", "chat_turn", recent, `{"text":"x"}`)
	if _, err := s.Sweep(ctx, RetentionPolicy{Records: 30 * day}, sweepNow, false); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events`); n != 0 {
		t.Fatalf("events left = %d", n)
	}
}

func TestSweepCutoffBoundaryAndTimestampFormats(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	cutoff := sweepNow.Add(-7 * day)
	for id, ts := range map[string]string{
		"older-z":      cutoff.Add(-time.Second).Format(tsLayout),
		"older-offset": cutoff.Add(-time.Second).Format("2006-01-02T15:04:05.000000+00:00"),
		"newer-z":      cutoff.Add(time.Second).Format(tsLayout),
		"newer-offset": cutoff.Add(time.Second).Format("2006-01-02T15:04:05.000000+00:00"),
	} {
		seedEvent(t, s, id, "chat_turn", ts, `{}`)
	}
	if _, err := s.Sweep(ctx, defaultPolicy, sweepNow, false); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"older-z": 0, "older-offset": 0, "newer-z": 1, "newer-offset": 1} {
		if got := s.count(t, `SELECT COUNT(*) FROM events WHERE event_id = ?`, id); got != want {
			t.Errorf("%s count = %d, want %d", id, got, want)
		}
	}
}

func TestSweepBatchesAndDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	old := ago(8 * day)
	const n = sweepBatch*2 + 7
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO events(event_id, type, timestamp, payload_json) VALUES (?, 'notification', ?, '{"text":"x"}')`, "e-"+time.Duration(i).String(), old); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	dry, err := s.Sweep(ctx, defaultPolicy, sweepNow, true)
	if err != nil || dry.Counts["events.strip"] != n {
		t.Fatalf("dry run = %+v, %v", dry.Counts, err)
	}
	if left := s.count(t, `SELECT COUNT(*) FROM events WHERE json_type(payload_json, '$.text') IS NOT NULL`); left != n {
		t.Fatalf("dry run changed rows: %d left with text", left)
	}

	report, err := s.Sweep(ctx, defaultPolicy, sweepNow, false)
	if err != nil || report.Counts["events.strip"] != n {
		t.Fatalf("sweep = %+v, %v", report.Counts, err)
	}
	if left := s.count(t, `SELECT COUNT(*) FROM events WHERE json_type(payload_json, '$.text') IS NOT NULL`); left != 0 {
		t.Fatalf("%d rows kept their text", left)
	}
}

func TestSweepRemoteRecordsFollowTheProtocolWindow(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	old, live := sweepNow.Add(-RemoteWindow-time.Hour).Unix(), sweepNow.Add(-RemoteWindow+time.Hour).Unix()
	for _, id := range []struct {
		id string
		at int64
	}{{"old", old}, {"live", live}} {
		s.mustExec(t, `INSERT INTO remote_fences(id, from_machine, from_peer_id, created_at) VALUES (?, 'm', 'p', ?)`, id.id, id.at)
	}
	// Even with content kept for a day, a live remote record keeps its text:
	// it is protocol state until its window ends.
	if _, err := s.Sweep(ctx, RetentionPolicy{Content: day, Records: day}, sweepNow, false); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM remote_fences WHERE id = 'live'`); n != 1 {
		t.Fatal("live fence deleted")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM remote_fences WHERE id = 'old'`); n != 0 {
		t.Fatal("expired fence kept")
	}
}

func TestSweepDryRunMatchesTheSweepWhenRulesOverlap(t *testing.T) {
	ctx := context.Background()
	s := newSweepStore(t)
	// Past both cutoffs: the records rule deletes them, so no content rule
	// may count them too (a strip or transcript count would be work wasted
	// on rows about to go, and a dry run would overcount).
	seedEvent(t, s, "e-chat", "chat_turn", ago(200*day), `{"text":"x"}`)
	seedEvent(t, s, "e-status", "peer_status", ago(200*day), `{}`)
	seedEvent(t, s, "e-note", "notification", ago(200*day), `{"text":"x"}`)

	dry, err := s.Sweep(ctx, defaultPolicy, sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM events`); n != 3 {
		t.Fatalf("dry run left %d events, want 3", n)
	}
	real, err := s.Sweep(ctx, defaultPolicy, sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Counts) != len(real.Counts) {
		t.Fatalf("dry %v != real %v", dry.Counts, real.Counts)
	}
	for k, v := range real.Counts {
		if dry.Counts[k] != v {
			t.Fatalf("dry %v != real %v", dry.Counts, real.Counts)
		}
	}
	if real.Counts["events.delete"] != 3 || len(real.Counts) != 1 {
		t.Fatalf("real = %v", real.Counts)
	}
}

// sweepMatchingDryRun sweeps after a dry run and fails unless the dry run
// predicted every count, including rows a cascade (inbox pushes and refs)
// removes only once their item is gone.
func sweepMatchingDryRun(t *testing.T, s *Store, p RetentionPolicy) SweepReport {
	t.Helper()
	ctx := context.Background()
	dry, err := s.Sweep(ctx, p, sweepNow, true)
	if err != nil {
		t.Fatal(err)
	}
	real, err := s.Sweep(ctx, p, sweepNow, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Counts) != len(real.Counts) {
		t.Fatalf("dry %v != real %v", dry.Counts, real.Counts)
	}
	for k, v := range real.Counts {
		if dry.Counts[k] != v {
			t.Fatalf("dry %v != real %v", dry.Counts, real.Counts)
		}
	}
	return real
}

// Peer messages are pure content with no record rule, so one older than both
// cutoffs must still go (it is not left to a records rule that does not exist).
func TestSweepDeletesPeerMessagesOlderThanBothCutoffs(t *testing.T) {
	for _, p := range []RetentionPolicy{defaultPolicy, {Content: 7 * day}, {Content: 7 * day, Records: 2 * day}} {
		s := newSweepStore(t)
		s.mustExec(t, `INSERT INTO peer_messages(id, kind, peer_id, text, created_at) VALUES
			('msg-ancient', 'notify', 'repow-b', 'x', ?), ('msg-mid', 'notify', 'repow-b', 'x', ?), ('msg-new', 'notify', 'repow-b', 'x', ?)`,
			ago(400*day), ago(30*day), ago(1*day))
		sweepMatchingDryRun(t, s, p)
		if n := s.count(t, `SELECT COUNT(*) FROM peer_messages WHERE id = 'msg-new'`); n != 1 {
			t.Fatalf("%+v: a recent message was deleted", p)
		}
		if n := s.count(t, `SELECT COUNT(*) FROM peer_messages`); n != 1 {
			t.Fatalf("%+v: peer messages left = %d, want only msg-new", p, n)
		}
	}
}
