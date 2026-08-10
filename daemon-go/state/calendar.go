package state

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strings"
	"time"
)

// CalendarEntry is a recurring durable job template that materializes tracked
// work occurrences. It mirrors repowire/daemon/state/calendar.py:CalendarEntry
// and the calendar_entries table verbatim. Timestamps (NextDueAt, CreatedAt,
// UpdatedAt, LastMaterializedAt) are opaque ISO-8601 strings, exactly as the
// Python store stores and returns them.
type CalendarEntry struct {
	CalendarID           string
	Title                string
	Kind                 string
	State                string
	Cron                 string
	NextDueAt            string
	Visibility           string
	OwnerPeerID          *string
	AssignedPeerID       *string
	Circle               *string
	CreatedByPeerID      *string
	SourceKind           *string
	SourceID             *string
	Scope                *string
	Request              map[string]any
	Provenance           map[string]any
	LastOccurrenceWorkID *string
	LastMaterializedAt   *string
	CreatedAt            string
	UpdatedAt            string
}

// calendarColumns is the canonical column order for SELECT * round-trips.
const calendarColumns = `calendar_id, title, kind, state, cron, next_due_at,
	owner_peer_id, assigned_peer_id, circle, created_by_peer_id,
	source_kind, source_id, scope, visibility, request_json,
	provenance_json, last_occurrence_work_id, last_materialized_at,
	created_at, updated_at`

// canonicalJSON marshals to the compact, key-sorted form Python's json_dumps
// produces (sort_keys=True, separators=(",",":")). Go's encoding/json already
// sorts map keys and omits spaces, so a plain Marshal matches.
func canonicalJSON(v map[string]any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	return marshalJSON(v)
}

// calendarParseISO mirrors calendar.py _parse_iso: parse an ISO-8601 timestamp, assume
// UTC when no offset is present, return UTC.
func calendarParseISO(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", value, time.UTC); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("unparseable ISO timestamp %q", value)
}

func scanCalendarEntry(row interface{ Scan(...any) error }) (*CalendarEntry, error) {
	var (
		e            CalendarEntry
		owner        sql.NullString
		assigned     sql.NullString
		circle       sql.NullString
		createdBy    sql.NullString
		sourceKind   sql.NullString
		sourceID     sql.NullString
		scope        sql.NullString
		requestJSON  string
		provJSON     string
		lastWorkID   sql.NullString
		lastMaterial sql.NullString
	)
	if err := row.Scan(
		&e.CalendarID, &e.Title, &e.Kind, &e.State, &e.Cron, &e.NextDueAt,
		&owner, &assigned, &circle, &createdBy,
		&sourceKind, &sourceID, &scope, &e.Visibility, &requestJSON,
		&provJSON, &lastWorkID, &lastMaterial,
		&e.CreatedAt, &e.UpdatedAt,
	); err != nil {
		return nil, err
	}
	e.OwnerPeerID = nullStringPtr(owner)
	e.AssignedPeerID = nullStringPtr(assigned)
	e.Circle = nullStringPtr(circle)
	e.CreatedByPeerID = nullStringPtr(createdBy)
	e.SourceKind = nullStringPtr(sourceKind)
	e.SourceID = nullStringPtr(sourceID)
	e.Scope = nullStringPtr(scope)
	e.LastOccurrenceWorkID = nullStringPtr(lastWorkID)
	e.LastMaterializedAt = nullStringPtr(lastMaterial)
	e.Request = decodeJSONObject(requestJSON)
	e.Provenance = decodeJSONObject(provJSON)
	return &e, nil
}

// CreateCalendarEntry inserts a new active calendar entry. The caller supplies
// nextDueAt (Python computes it via next_fire_after(cron); cron evaluation is
// out of scope for this store port). title/kind/cron/nextDueAt are required;
// the optional pointer fields and request/provenance maps may be nil.
func (s *Store) CreateCalendarEntry(ctx context.Context, e *CalendarEntry) (*CalendarEntry, error) {
	now := nowISO()
	out := *e
	out.CalendarID = newID("cal-")
	out.State = "active"
	if out.Visibility == "" {
		out.Visibility = "circle"
	}
	out.CreatedAt = now
	out.UpdatedAt = now
	out.LastOccurrenceWorkID = nil
	out.LastMaterializedAt = nil

	requestJSON, err := canonicalJSON(out.Request)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	provJSON, err := canonicalJSON(out.Provenance)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}

	const q = `INSERT INTO calendar_entries(
		calendar_id, title, kind, state, cron, next_due_at,
		owner_peer_id, assigned_peer_id, circle, created_by_peer_id,
		source_kind, source_id, scope, visibility, request_json,
		provenance_json, last_occurrence_work_id, last_materialized_at,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = s.db.ExecContext(ctx, q,
		out.CalendarID, out.Title, out.Kind, out.State, out.Cron, out.NextDueAt,
		strOrNil(out.OwnerPeerID), strOrNil(out.AssignedPeerID), strOrNil(out.Circle), strOrNil(out.CreatedByPeerID),
		strOrNil(out.SourceKind), strOrNil(out.SourceID), strOrNil(out.Scope), out.Visibility, requestJSON,
		provJSON, nil, nil,
		out.CreatedAt, out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert calendar entry %s: %w", out.CalendarID, err)
	}
	return &out, nil
}

// GetCalendarEntry returns the entry by id, or (nil, nil) if absent.
func (s *Store) GetCalendarEntry(ctx context.Context, calendarID string) (*CalendarEntry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+calendarColumns+` FROM calendar_entries WHERE calendar_id = ?`, calendarID)
	e, err := scanCalendarEntry(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get calendar entry %s: %w", calendarID, err)
	}
	return e, nil
}

// CalendarFilter selects rows for ListCalendarEntries. A nil field is not
// filtered; a non-nil field filters by exact equality (matching the Python
// keyword-arg semantics where None == no filter).
type CalendarFilter struct {
	State           *string
	OwnerPeerID     *string
	CreatedByPeerID *string
	Circle          *string
}

// ListCalendarEntries returns matching entries ordered by next_due_at.
func (s *Store) ListCalendarEntries(ctx context.Context, f CalendarFilter) ([]*CalendarEntry, error) {
	var clauses []string
	var params []any
	if f.State != nil {
		clauses = append(clauses, "state = ?")
		params = append(params, *f.State)
	}
	if f.OwnerPeerID != nil {
		clauses = append(clauses, "owner_peer_id = ?")
		params = append(params, *f.OwnerPeerID)
	}
	if f.CreatedByPeerID != nil {
		clauses = append(clauses, "created_by_peer_id = ?")
		params = append(params, *f.CreatedByPeerID)
	}
	if f.Circle != nil {
		clauses = append(clauses, "circle = ?")
		params = append(params, *f.Circle)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	q := `SELECT ` + calendarColumns + ` FROM calendar_entries ` + where + ` ORDER BY next_due_at`
	rows, err := s.db.QueryContext(ctx, q, params...)
	if err != nil {
		return nil, fmt.Errorf("list calendar entries: %w", err)
	}
	defer rows.Close()

	var out []*CalendarEntry
	for rows.Next() {
		e, err := scanCalendarEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan calendar entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate calendar entries: %w", err)
	}
	return out, nil
}

// CancelCalendarEntry transitions an entry to 'cancelled', recording the reason
// under provenance["cancel_reason"]. Returns (nil, nil) if the entry is absent.
// reason defaults to "cancel_requested" when empty.
func (s *Store) CancelCalendarEntry(ctx context.Context, calendarID, reason string) (*CalendarEntry, error) {
	if reason == "" {
		reason = "cancel_requested"
	}
	existing, err := s.GetCalendarEntry(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}
	prov := calendarCloneMap(existing.Provenance)
	prov["cancel_reason"] = reason
	provJSON, err := canonicalJSON(prov)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}
	now := nowISO()
	_, err = s.db.ExecContext(ctx,
		`UPDATE calendar_entries SET state = 'cancelled', provenance_json = ?, updated_at = ? WHERE calendar_id = ?`,
		provJSON, now, calendarID)
	if err != nil {
		return nil, fmt.Errorf("cancel calendar entry %s: %w", calendarID, err)
	}
	return s.GetCalendarEntry(ctx, calendarID)
}

// UpdateCalendarRuntimeBinding records a runtime binding under provenance,
// keeping the last 10 in provenance["runtime_binding_history"] and the latest
// in provenance["runtime_binding"]. Returns (nil, nil) if the entry is absent.
func (s *Store) UpdateCalendarRuntimeBinding(ctx context.Context, calendarID string, binding map[string]any) (*CalendarEntry, error) {
	existing, err := s.GetCalendarEntry(ctx, calendarID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}
	prov := calendarCloneMap(existing.Provenance)
	var history []any
	if h, ok := prov["runtime_binding_history"].([]any); ok {
		history = append(history, h...)
	}
	history = append(history, binding)
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	prov["runtime_binding"] = binding
	prov["runtime_binding_history"] = history
	provJSON, err := canonicalJSON(prov)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}
	now := nowISO()
	_, err = s.db.ExecContext(ctx,
		`UPDATE calendar_entries SET provenance_json = ?, updated_at = ? WHERE calendar_id = ?`,
		provJSON, now, calendarID)
	if err != nil {
		return nil, fmt.Errorf("update runtime binding %s: %w", calendarID, err)
	}
	return s.GetCalendarEntry(ctx, calendarID)
}

// SecondsUntilNextDue returns seconds until the soonest active entry's
// next_due_at relative to now, floored at 0. Returns (nil, nil) when no active
// entry exists. Pass the reference time; the Python default is datetime.now(utc).
func (s *Store) SecondsUntilNextDue(ctx context.Context, now time.Time) (*float64, error) {
	var nextDue string
	err := s.db.QueryRowContext(ctx,
		`SELECT next_due_at FROM calendar_entries WHERE state = 'active' ORDER BY next_due_at LIMIT 1`).Scan(&nextDue)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("seconds until next due: %w", err)
	}
	due, err := calendarParseISO(nextDue)
	if err != nil {
		return nil, fmt.Errorf("parse next_due_at: %w", err)
	}
	secs := due.Sub(now.UTC()).Seconds()
	if secs < 0 {
		secs = 0
	}
	return &secs, nil
}

func calendarCloneMap(m map[string]any) map[string]any {
	return maps.Clone(m)
}
