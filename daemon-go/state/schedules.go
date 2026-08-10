package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// validScheduleKinds mirrors _VALID_KINDS in schedule_store.py.
var validScheduleKinds = map[string]bool{
	"ask":    true,
	"notify": true,
}

// Schedule is one row of the schedules table, as the Python SQLiteScheduleStore
// reads/writes it. fire_at, created_at, and updated_at are stored as ISO-8601
// strings (the Python store keeps them as strings, never as time.Time), so we
// preserve that representation rather than reparsing.
type Schedule struct {
	ScheduleID string  `json:"schedule_id"`
	FromPeer   string  `json:"from_peer"`
	ToPeer     string  `json:"to_peer"`
	Text       string  `json:"text"`
	FireAt     string  `json:"fire_at"` // ISO-8601 UTC
	Kind       string  `json:"kind"`    // "ask" | "notify"
	Circle     *string `json:"circle"`
	Cron       *string `json:"cron"`
	CreatedAt  string  `json:"created_at"` // ISO-8601 UTC
}

// scheduleColumns is the exact subset _upsert_schedule / _row_to_schedule touch.
// The schedules table has more columns (from_peer_id, last_fired_at, ...) that
// the Python store leaves untouched; we mirror that and never write them.
const scheduleColumns = `schedule_id, from_peer, to_peer, text, kind, circle, fire_at, cron, created_at`

// scanSchedule reads the scheduleColumns subset from a row scanner.
func scanSchedule(scan func(dest ...any) error) (*Schedule, error) {
	var (
		s      Schedule
		circle sql.NullString
		cron   sql.NullString
	)
	if err := scan(&s.ScheduleID, &s.FromPeer, &s.ToPeer, &s.Text, &s.Kind, &circle, &s.FireAt, &cron, &s.CreatedAt); err != nil {
		return nil, err
	}
	if circle.Valid {
		c := circle.String
		s.Circle = &c
	}
	if cron.Valid {
		c := cron.String
		s.Cron = &c
	}
	return &s, nil
}

// upsertSchedule mirrors SQLiteScheduleStore._upsert_schedule: INSERT OR REPLACE
// the column subset, stamping updated_at to now.
func (s *Store) upsertSchedule(ctx context.Context, sched *Schedule) error {
	const q = `INSERT OR REPLACE INTO schedules(
		schedule_id, from_peer, to_peer, text, kind, circle, fire_at, cron, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	var circle any
	if sched.Circle != nil {
		circle = *sched.Circle
	}
	var cron any
	if sched.Cron != nil {
		cron = *sched.Cron
	}
	_, err := s.db.ExecContext(ctx, q,
		sched.ScheduleID,
		sched.FromPeer,
		sched.ToPeer,
		sched.Text,
		sched.Kind,
		circle,
		sched.FireAt,
		cron,
		sched.CreatedAt,
		formatTS(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("upsert schedule %s: %w", sched.ScheduleID, err)
	}
	return nil
}

// CreateSchedule inserts a one-shot (or recurring, if cron is set) schedule and
// returns it. fireAt must be non-zero; the caller supplies the resolved fire
// time (for cron schedules the daemon computes the next fire externally, just as
// Python's create_cron delegates to next_fire_after before calling create).
// Mirrors SQLiteScheduleStore.create.
func (s *Store) CreateSchedule(
	ctx context.Context,
	fromPeer, toPeer, text string,
	fireAt time.Time,
	kind string,
	circle, cron *string,
) (*Schedule, error) {
	if kind == "" {
		kind = "notify"
	}
	if !validScheduleKinds[kind] {
		return nil, fmt.Errorf("kind must be one of [ask notify]; got %q", kind)
	}
	if fireAt.IsZero() {
		return nil, errors.New("fire_at must be set")
	}
	sched := &Schedule{
		ScheduleID: "sched-" + uuid.NewString()[:8],
		FromPeer:   fromPeer,
		ToPeer:     toPeer,
		Text:       text,
		FireAt:     fireAt.UTC().Format(time.RFC3339Nano),
		Kind:       kind,
		Circle:     circle,
		Cron:       cron,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := s.upsertSchedule(ctx, sched); err != nil {
		return nil, err
	}
	return sched, nil
}

// GetSchedule returns the schedule by id, or (nil, nil) if absent.
// Mirrors SQLiteScheduleStore.get.
func (s *Store) GetSchedule(ctx context.Context, scheduleID string) (*Schedule, error) {
	const q = `SELECT ` + scheduleColumns + ` FROM schedules WHERE schedule_id = ?`
	row := s.db.QueryRowContext(ctx, q, scheduleID)
	sched, err := scanSchedule(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get schedule %s: %w", scheduleID, err)
	}
	return sched, nil
}

// ListSchedules returns all schedules ordered by fire_at, optionally filtered to
// one from_peer. Pass fromPeer == nil for all. Mirrors SQLiteScheduleStore.list_all.
func (s *Store) ListSchedules(ctx context.Context, fromPeer *string) ([]*Schedule, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if fromPeer == nil {
		rows, err = s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY fire_at`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE from_peer = ? ORDER BY fire_at`, *fromPeer)
	}
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()

	var out []*Schedule
	for rows.Next() {
		sched, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		out = append(out, sched)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedules: %w", err)
	}
	return out, nil
}

// NextDueSchedule returns the schedule with the earliest fire_at, or (nil, nil)
// if the table is empty. Mirrors SQLiteScheduleStore.next_due.
func (s *Store) NextDueSchedule(ctx context.Context) (*Schedule, error) {
	const q = `SELECT ` + scheduleColumns + ` FROM schedules ORDER BY fire_at LIMIT 1`
	row := s.db.QueryRowContext(ctx, q)
	sched, err := scanSchedule(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("next due schedule: %w", err)
	}
	return sched, nil
}

// RescheduleNext advances a recurring schedule's fire_at. The caller supplies the
// resolved next fire time (computed from the schedule's cron externally, as
// Python's reschedule_next delegates to next_fire_after). Returns false if the
// schedule is absent or has no cron. Mirrors SQLiteScheduleStore.reschedule_next.
func (s *Store) RescheduleNext(ctx context.Context, scheduleID string, nextFireAt time.Time) (bool, error) {
	sched, err := s.GetSchedule(ctx, scheduleID)
	if err != nil {
		return false, err
	}
	if sched == nil || sched.Cron == nil {
		return false, nil
	}
	const q = `UPDATE schedules SET fire_at = ?, updated_at = ? WHERE schedule_id = ?`
	_, err = s.db.ExecContext(ctx, q,
		nextFireAt.UTC().Format(time.RFC3339Nano),
		formatTS(time.Now()),
		scheduleID,
	)
	if err != nil {
		return false, fmt.Errorf("reschedule %s: %w", scheduleID, err)
	}
	return true, nil
}

// DeleteSchedule removes a schedule, returning the deleted row (or nil if absent).
// Mirrors SQLiteScheduleStore.delete.
func (s *Store) DeleteSchedule(ctx context.Context, scheduleID string) (*Schedule, error) {
	sched, err := s.GetSchedule(ctx, scheduleID)
	if err != nil {
		return nil, err
	}
	if sched == nil {
		return nil, nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE schedule_id = ?`, scheduleID); err != nil {
		return nil, fmt.Errorf("delete schedule %s: %w", scheduleID, err)
	}
	return sched, nil
}
