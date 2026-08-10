package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// calendarDDL is copied verbatim from repowire/daemon/state/database.py
// (calendar_entries table + its indexes), plus the user_version stamp NewStore
// requires.
const calendarDDL = `
CREATE TABLE IF NOT EXISTS calendar_entries (
    calendar_id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    cron TEXT NOT NULL,
    next_due_at TEXT NOT NULL,
    owner_peer_id TEXT,
    assigned_peer_id TEXT,
    circle TEXT,
    created_by_peer_id TEXT,
    source_kind TEXT,
    source_id TEXT,
    scope TEXT,
    visibility TEXT NOT NULL,
    request_json TEXT NOT NULL,
    provenance_json TEXT NOT NULL,
    last_occurrence_work_id TEXT,
    last_materialized_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_calendar_entries_state_due
    ON calendar_entries(state, next_due_at);
CREATE INDEX IF NOT EXISTS idx_calendar_entries_circle_due
    ON calendar_entries(circle, next_due_at);
PRAGMA user_version=12;
`

func newCalendarStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(calendarDDL); err != nil {
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

func strptr(s string) *string { return &s }

func TestCreateGetListCalendarEntry(t *testing.T) {
	s := newCalendarStore(t)
	ctx := context.Background()

	created, err := s.CreateCalendarEntry(ctx, &CalendarEntry{
		Title:           "nightly digest",
		Kind:            "job",
		Cron:            "0 9 * * *",
		NextDueAt:       "2026-06-30T09:00:00+00:00",
		OwnerPeerID:     strptr("peer-owner"),
		CreatedByPeerID: strptr("peer-creator"),
		Circle:          strptr("default"),
		SourceKind:      strptr("manual"),
		SourceID:        strptr("src-1"),
		Scope:           strptr("repo"),
		Request:         map[string]any{"execution": map[string]any{"mode": "x"}},
		Provenance:      map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatalf("CreateCalendarEntry: %v", err)
	}
	if created.CalendarID == "" || created.CalendarID[:4] != "cal-" {
		t.Errorf("calendar_id = %q, want cal-* prefix", created.CalendarID)
	}
	if created.State != "active" {
		t.Errorf("state = %q, want active", created.State)
	}
	if created.Visibility != "circle" {
		t.Errorf("visibility = %q, want default circle", created.Visibility)
	}
	if created.CreatedAt == "" || created.CreatedAt != created.UpdatedAt {
		t.Errorf("created_at/updated_at = %q/%q", created.CreatedAt, created.UpdatedAt)
	}

	got, err := s.GetCalendarEntry(ctx, created.CalendarID)
	if err != nil {
		t.Fatalf("GetCalendarEntry: %v", err)
	}
	if got == nil {
		t.Fatal("GetCalendarEntry returned nil for existing id")
	}
	if got.Title != "nightly digest" || got.Kind != "job" || got.Cron != "0 9 * * *" {
		t.Errorf("scalar fields wrong: %+v", got)
	}
	if got.NextDueAt != "2026-06-30T09:00:00+00:00" {
		t.Errorf("next_due_at = %q", got.NextDueAt)
	}
	if got.OwnerPeerID == nil || *got.OwnerPeerID != "peer-owner" {
		t.Errorf("owner_peer_id = %v", got.OwnerPeerID)
	}
	if v, _ := got.Request["execution"].(map[string]any); v == nil || v["mode"] != "x" {
		t.Errorf("request round-trip wrong: %+v", got.Request)
	}
	if got.Provenance["k"] != "v" {
		t.Errorf("provenance round-trip wrong: %+v", got.Provenance)
	}
	if got.LastOccurrenceWorkID != nil || got.LastMaterializedAt != nil {
		t.Errorf("fresh entry should have nil last_* fields: %+v", got)
	}

	// Bare entry: nil optional pointers, nil maps -> {}.
	bare, err := s.CreateCalendarEntry(ctx, &CalendarEntry{
		Title:     "bare",
		Kind:      "job",
		Cron:      "* * * * *",
		NextDueAt: "2026-06-29T00:00:00+00:00",
	})
	if err != nil {
		t.Fatalf("CreateCalendarEntry bare: %v", err)
	}
	bareGot, err := s.GetCalendarEntry(ctx, bare.CalendarID)
	if err != nil {
		t.Fatalf("get bare: %v", err)
	}
	if bareGot.OwnerPeerID != nil || bareGot.Circle != nil || bareGot.SourceKind != nil {
		t.Errorf("bare entry nullable fields should be nil: %+v", bareGot)
	}
	if bareGot.Request == nil || len(bareGot.Request) != 0 {
		t.Errorf("nil request should round-trip to empty map, got %+v", bareGot.Request)
	}

	// Missing id -> (nil, nil).
	missing, err := s.GetCalendarEntry(ctx, "cal-doesnotexist")
	if err != nil {
		t.Fatalf("GetCalendarEntry missing: %v", err)
	}
	if missing != nil {
		t.Errorf("missing id should return nil, got %+v", missing)
	}
}

func TestListCalendarEntriesFilterAndOrder(t *testing.T) {
	s := newCalendarStore(t)
	ctx := context.Background()

	mk := func(title, nextDue, circle, owner string) {
		_, err := s.CreateCalendarEntry(ctx, &CalendarEntry{
			Title:       title,
			Kind:        "job",
			Cron:        "* * * * *",
			NextDueAt:   nextDue,
			Circle:      strptr(circle),
			OwnerPeerID: strptr(owner),
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
	}
	mk("c", "2026-06-30T12:00:00+00:00", "alpha", "o1")
	mk("a", "2026-06-30T08:00:00+00:00", "alpha", "o2")
	mk("b", "2026-06-30T10:00:00+00:00", "beta", "o1")

	all, err := s.ListCalendarEntries(ctx, CalendarFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d, want 3", len(all))
	}
	// ORDER BY next_due_at: a (08) < b (10) < c (12).
	if all[0].Title != "a" || all[1].Title != "b" || all[2].Title != "c" {
		t.Errorf("order wrong: %s %s %s", all[0].Title, all[1].Title, all[2].Title)
	}

	byCircle, err := s.ListCalendarEntries(ctx, CalendarFilter{Circle: strptr("alpha")})
	if err != nil {
		t.Fatalf("list by circle: %v", err)
	}
	if len(byCircle) != 2 {
		t.Errorf("circle=alpha got %d, want 2", len(byCircle))
	}

	byOwner, err := s.ListCalendarEntries(ctx, CalendarFilter{OwnerPeerID: strptr("o1")})
	if err != nil {
		t.Fatalf("list by owner: %v", err)
	}
	if len(byOwner) != 2 {
		t.Errorf("owner=o1 got %d, want 2", len(byOwner))
	}

	byState, err := s.ListCalendarEntries(ctx, CalendarFilter{State: strptr("active")})
	if err != nil {
		t.Fatalf("list by state: %v", err)
	}
	if len(byState) != 3 {
		t.Errorf("state=active got %d, want 3", len(byState))
	}
}

func TestCancelCalendarEntry(t *testing.T) {
	s := newCalendarStore(t)
	ctx := context.Background()

	created, err := s.CreateCalendarEntry(ctx, &CalendarEntry{
		Title:      "to cancel",
		Kind:       "job",
		Cron:       "* * * * *",
		NextDueAt:  "2026-06-30T00:00:00+00:00",
		Provenance: map[string]any{"existing": true},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	cancelled, err := s.CancelCalendarEntry(ctx, created.CalendarID, "")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled == nil {
		t.Fatal("cancel returned nil for existing entry")
	}
	if cancelled.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", cancelled.State)
	}
	if cancelled.Provenance["cancel_reason"] != "cancel_requested" {
		t.Errorf("default reason = %v, want cancel_requested", cancelled.Provenance["cancel_reason"])
	}
	if cancelled.Provenance["existing"] != true {
		t.Errorf("existing provenance lost: %+v", cancelled.Provenance)
	}
	if cancelled.UpdatedAt == created.UpdatedAt {
		t.Error("updated_at should change on cancel")
	}

	// Custom reason.
	c2, _ := s.CreateCalendarEntry(ctx, &CalendarEntry{Title: "x", Kind: "job", Cron: "* * * * *", NextDueAt: "2026-07-01T00:00:00+00:00"})
	cancelled2, err := s.CancelCalendarEntry(ctx, c2.CalendarID, "superseded")
	if err != nil {
		t.Fatalf("cancel custom: %v", err)
	}
	if cancelled2.Provenance["cancel_reason"] != "superseded" {
		t.Errorf("custom reason = %v", cancelled2.Provenance["cancel_reason"])
	}

	// Missing -> (nil, nil).
	none, err := s.CancelCalendarEntry(ctx, "cal-nope", "")
	if err != nil {
		t.Fatalf("cancel missing: %v", err)
	}
	if none != nil {
		t.Errorf("cancel missing should return nil, got %+v", none)
	}
}

func TestUpdateCalendarRuntimeBinding(t *testing.T) {
	s := newCalendarStore(t)
	ctx := context.Background()

	created, err := s.CreateCalendarEntry(ctx, &CalendarEntry{
		Title: "binder", Kind: "job", Cron: "* * * * *", NextDueAt: "2026-06-30T00:00:00+00:00",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Append 12 bindings; history must keep only the last 10.
	var last *CalendarEntry
	for i := 0; i < 12; i++ {
		last, err = s.UpdateCalendarRuntimeBinding(ctx, created.CalendarID, map[string]any{"n": float64(i)})
		if err != nil {
			t.Fatalf("update binding %d: %v", i, err)
		}
	}
	if last == nil {
		t.Fatal("update returned nil")
	}
	rb, ok := last.Provenance["runtime_binding"].(map[string]any)
	if !ok || rb["n"] != float64(11) {
		t.Errorf("runtime_binding latest = %+v", last.Provenance["runtime_binding"])
	}
	hist, ok := last.Provenance["runtime_binding_history"].([]any)
	if !ok {
		t.Fatalf("history wrong type: %T", last.Provenance["runtime_binding_history"])
	}
	if len(hist) != 10 {
		t.Errorf("history len = %d, want 10 (capped)", len(hist))
	}
	first := hist[0].(map[string]any)
	if first["n"] != float64(2) {
		t.Errorf("oldest retained binding n = %v, want 2", first["n"])
	}

	none, err := s.UpdateCalendarRuntimeBinding(ctx, "cal-nope", map[string]any{})
	if err != nil {
		t.Fatalf("update missing: %v", err)
	}
	if none != nil {
		t.Errorf("update missing should return nil, got %+v", none)
	}
}

func TestMaterializeDueClaimsOccurrenceAtomically(t *testing.T) {
	s := newTempStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 8, 0, 0, 0, time.UTC)
	entry, err := s.CreateCalendarEntry(ctx, &CalendarEntry{
		Title: "hourly", Kind: "job", Cron: "0 * * * *", NextDueAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}

	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	nextFire := func(_ string, after time.Time) (time.Time, error) {
		ready.Done()
		<-release
		return after.Add(time.Hour), nil
	}
	type result struct {
		work []*TrackedWork
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			work, err := s.MaterializeDue(ctx, now, nextFire)
			results <- result{work: work, err: err}
		}()
	}
	ready.Wait()
	close(release)

	total := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		total += len(result.work)
	}
	if total != 1 {
		t.Fatalf("materialized %d occurrences, want 1", total)
	}
	work, err := s.ListWork(ctx, WorkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 {
		t.Fatalf("stored %d occurrences, want 1", len(work))
	}
	refreshed, err := s.GetCalendarEntry(ctx, entry.CalendarID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.LastOccurrenceWorkID == nil || *refreshed.LastOccurrenceWorkID != work[0].WorkID {
		t.Fatalf("calendar occurrence = %v, work = %s", refreshed.LastOccurrenceWorkID, work[0].WorkID)
	}
}

func TestSecondsUntilNextDue(t *testing.T) {
	s := newCalendarStore(t)
	ctx := context.Background()

	now := time.Date(2026, 6, 30, 8, 0, 0, 0, time.UTC)

	// No entries -> nil.
	secs, err := s.SecondsUntilNextDue(ctx, now)
	if err != nil {
		t.Fatalf("seconds (empty): %v", err)
	}
	if secs != nil {
		t.Errorf("empty store should return nil, got %v", *secs)
	}

	// Two active entries; the soonest (in 1h) wins.
	if _, err := s.CreateCalendarEntry(ctx, &CalendarEntry{Title: "far", Kind: "job", Cron: "* * * * *", NextDueAt: "2026-06-30T12:00:00+00:00"}); err != nil {
		t.Fatalf("create far: %v", err)
	}
	if _, err := s.CreateCalendarEntry(ctx, &CalendarEntry{Title: "soon", Kind: "job", Cron: "* * * * *", NextDueAt: "2026-06-30T09:00:00+00:00"}); err != nil {
		t.Fatalf("create soon: %v", err)
	}
	secs, err = s.SecondsUntilNextDue(ctx, now)
	if err != nil {
		t.Fatalf("seconds: %v", err)
	}
	if secs == nil || *secs != 3600 {
		t.Errorf("seconds = %v, want 3600", secs)
	}

	// A past due active entry floors at 0.
	if _, err := s.CreateCalendarEntry(ctx, &CalendarEntry{Title: "overdue", Kind: "job", Cron: "* * * * *", NextDueAt: "2026-06-30T07:00:00+00:00"}); err != nil {
		t.Fatalf("create overdue: %v", err)
	}
	secs, err = s.SecondsUntilNextDue(ctx, now)
	if err != nil {
		t.Fatalf("seconds overdue: %v", err)
	}
	if secs == nil || *secs != 0 {
		t.Errorf("overdue seconds = %v, want 0", secs)
	}

	// Cancelled entries are ignored (Z suffix exercises _parse_iso replace).
	s2 := newCalendarStore(t)
	c, _ := s2.CreateCalendarEntry(ctx, &CalendarEntry{Title: "c", Kind: "job", Cron: "* * * * *", NextDueAt: "2026-06-30T09:00:00Z"})
	if _, err := s2.CancelCalendarEntry(ctx, c.CalendarID, ""); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	secs, err = s2.SecondsUntilNextDue(ctx, now)
	if err != nil {
		t.Fatalf("seconds after cancel: %v", err)
	}
	if secs != nil {
		t.Errorf("only entry cancelled -> nil, got %v", *secs)
	}
}
