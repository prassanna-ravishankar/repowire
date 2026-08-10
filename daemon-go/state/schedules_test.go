package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// schedulesDDL is the schedules table + fire_at index, copied verbatim from
// repowire/daemon/state/database.py. It stamps user_version=12 so NewStore opens.
const schedulesDDL = `
CREATE TABLE IF NOT EXISTS schedules (
    schedule_id TEXT PRIMARY KEY,
    from_peer TEXT NOT NULL,
    from_peer_id TEXT,
    to_peer TEXT NOT NULL,
    to_peer_id TEXT,
    text TEXT NOT NULL,
    kind TEXT NOT NULL,
    circle TEXT,
    fire_at TEXT NOT NULL,
    cron TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_fired_at TEXT,
    last_outcome TEXT,
    last_error TEXT
);
CREATE INDEX IF NOT EXISTS idx_schedules_fire_at ON schedules(fire_at);
PRAGMA user_version=12;
`

func newScheduleStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")

	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(schedulesDDL); err != nil {
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

func TestCreateScheduleRoundTrip(t *testing.T) {
	s := newScheduleStore(t)
	ctx := context.Background()

	fireAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	circle := "default"
	got, err := s.CreateSchedule(ctx, "alice", "bob", "stand-up", fireAt, "ask", &circle, nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if got.ScheduleID == "" || got.ScheduleID[:6] != "sched-" {
		t.Errorf("schedule_id = %q, want sched- prefix", got.ScheduleID)
	}
	if got.Kind != "ask" || got.FromPeer != "alice" || got.ToPeer != "bob" || got.Text != "stand-up" {
		t.Errorf("fields wrong: %+v", got)
	}
	if got.Circle == nil || *got.Circle != "default" {
		t.Errorf("circle = %v, want default", got.Circle)
	}
	if got.Cron != nil {
		t.Errorf("cron should be nil, got %v", *got.Cron)
	}

	back, err := s.GetSchedule(ctx, got.ScheduleID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if back == nil {
		t.Fatal("GetSchedule returned nil for existing id")
	}
	if back.FireAt != got.FireAt || back.CreatedAt != got.CreatedAt {
		t.Errorf("round-trip mismatch: %+v vs %+v", back, got)
	}
	if back.Circle == nil || *back.Circle != "default" {
		t.Errorf("round-trip circle = %v", back.Circle)
	}
}

func TestCreateScheduleDefaultsAndValidation(t *testing.T) {
	s := newScheduleStore(t)
	ctx := context.Background()

	// Empty kind defaults to notify.
	got, err := s.CreateSchedule(ctx, "a", "b", "t", time.Now().UTC(), "", nil, nil)
	if err != nil {
		t.Fatalf("CreateSchedule default kind: %v", err)
	}
	if got.Kind != "notify" {
		t.Errorf("default kind = %q, want notify", got.Kind)
	}

	// Invalid kind rejected.
	if _, err := s.CreateSchedule(ctx, "a", "b", "t", time.Now().UTC(), "bogus", nil, nil); err == nil {
		t.Error("expected error for invalid kind")
	}

	// Zero fire_at rejected.
	if _, err := s.CreateSchedule(ctx, "a", "b", "t", time.Time{}, "notify", nil, nil); err == nil {
		t.Error("expected error for zero fire_at")
	}
}

func TestListSchedulesOrderingAndFilter(t *testing.T) {
	s := newScheduleStore(t)
	ctx := context.Background()

	late := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	early := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)

	if _, err := s.CreateSchedule(ctx, "alice", "bob", "late", late, "notify", nil, nil); err != nil {
		t.Fatalf("create late: %v", err)
	}
	if _, err := s.CreateSchedule(ctx, "alice", "bob", "early", early, "notify", nil, nil); err != nil {
		t.Fatalf("create early: %v", err)
	}
	if _, err := s.CreateSchedule(ctx, "carol", "bob", "mid", mid, "notify", nil, nil); err != nil {
		t.Fatalf("create mid: %v", err)
	}

	all, err := s.ListSchedules(ctx, nil)
	if err != nil {
		t.Fatalf("ListSchedules all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d schedules, want 3", len(all))
	}
	// Ordered by fire_at ascending.
	if all[0].Text != "early" || all[1].Text != "mid" || all[2].Text != "late" {
		t.Errorf("ordering wrong: %s, %s, %s", all[0].Text, all[1].Text, all[2].Text)
	}

	from := "alice"
	mine, err := s.ListSchedules(ctx, &from)
	if err != nil {
		t.Fatalf("ListSchedules filtered: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("got %d alice schedules, want 2", len(mine))
	}
	for _, m := range mine {
		if m.FromPeer != "alice" {
			t.Errorf("filter leaked %s", m.FromPeer)
		}
	}
}

func TestNextDueSchedule(t *testing.T) {
	s := newScheduleStore(t)
	ctx := context.Background()

	none, err := s.NextDueSchedule(ctx)
	if err != nil {
		t.Fatalf("NextDueSchedule empty: %v", err)
	}
	if none != nil {
		t.Errorf("expected nil on empty table, got %+v", none)
	}

	late := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	early := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.CreateSchedule(ctx, "a", "b", "late", late, "notify", nil, nil); err != nil {
		t.Fatalf("create late: %v", err)
	}
	if _, err := s.CreateSchedule(ctx, "a", "b", "early", early, "notify", nil, nil); err != nil {
		t.Fatalf("create early: %v", err)
	}

	due, err := s.NextDueSchedule(ctx)
	if err != nil {
		t.Fatalf("NextDueSchedule: %v", err)
	}
	if due == nil || due.Text != "early" {
		t.Errorf("next due = %+v, want early", due)
	}
}

func TestRescheduleNext(t *testing.T) {
	s := newScheduleStore(t)
	ctx := context.Background()

	// No-cron schedule cannot be rescheduled.
	oneShot, err := s.CreateSchedule(ctx, "a", "b", "once", time.Now().UTC(), "notify", nil, nil)
	if err != nil {
		t.Fatalf("create one-shot: %v", err)
	}
	ok, err := s.RescheduleNext(ctx, oneShot.ScheduleID, time.Now().UTC())
	if err != nil {
		t.Fatalf("RescheduleNext one-shot: %v", err)
	}
	if ok {
		t.Error("RescheduleNext should return false for a cron-less schedule")
	}

	// Missing schedule -> false, no error.
	ok, err = s.RescheduleNext(ctx, "sched-nope", time.Now().UTC())
	if err != nil {
		t.Fatalf("RescheduleNext missing: %v", err)
	}
	if ok {
		t.Error("RescheduleNext should return false for a missing schedule")
	}

	// Cron schedule advances fire_at.
	cron := "0 9 * * *"
	first := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	rec, err := s.CreateSchedule(ctx, "a", "b", "daily", first, "notify", nil, &cron)
	if err != nil {
		t.Fatalf("create recurring: %v", err)
	}
	next := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	ok, err = s.RescheduleNext(ctx, rec.ScheduleID, next)
	if err != nil {
		t.Fatalf("RescheduleNext cron: %v", err)
	}
	if !ok {
		t.Fatal("RescheduleNext should return true for a cron schedule")
	}
	back, err := s.GetSchedule(ctx, rec.ScheduleID)
	if err != nil {
		t.Fatalf("GetSchedule: %v", err)
	}
	if back.FireAt != next.UTC().Format(time.RFC3339Nano) {
		t.Errorf("fire_at = %q, want %q", back.FireAt, next.UTC().Format(time.RFC3339Nano))
	}
}

func TestDeleteSchedule(t *testing.T) {
	s := newScheduleStore(t)
	ctx := context.Background()

	created, err := s.CreateSchedule(ctx, "a", "b", "doomed", time.Now().UTC(), "notify", nil, nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	deleted, err := s.DeleteSchedule(ctx, created.ScheduleID)
	if err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if deleted == nil || deleted.ScheduleID != created.ScheduleID {
		t.Errorf("DeleteSchedule returned %+v, want the deleted row", deleted)
	}

	gone, err := s.GetSchedule(ctx, created.ScheduleID)
	if err != nil {
		t.Fatalf("GetSchedule after delete: %v", err)
	}
	if gone != nil {
		t.Errorf("schedule should be gone, got %+v", gone)
	}

	// Deleting a missing schedule -> nil, no error.
	missing, err := s.DeleteSchedule(ctx, "sched-nope")
	if err != nil {
		t.Fatalf("DeleteSchedule missing: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for missing delete, got %+v", missing)
	}
}
