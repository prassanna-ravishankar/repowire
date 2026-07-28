package state

// work_status.go provides the status()/result() dict projections the work HTTP
// routes serialize, plus the calendar status() projection and MaterializeDue.
// These mirror TrackedWork.status/result (work_store.py) and
// CalendarEntry.status / SQLiteCalendarStore.materialize_due (calendar.py). Wire
// shapes are load-bearing — dashboard/CLI/MCP clients read these keys verbatim.

import (
	"context"
	"time"
)

// Status renders the public status dict (TrackedWork.status). Keys and order of
// derivation match the Python projection so clients see identical payloads.
func (w *TrackedWork) Status() map[string]any {
	execution := mapAt(w.Request, "execution")
	schedule := mapAt(execution, "schedule")
	return map[string]any{
		"job_id":                      w.WorkID,
		"work_id":                     w.WorkID,
		"title":                       w.Title,
		"kind":                        w.Kind,
		"state":                       w.State,
		"state_reason":                strOrNil(w.StateReason),
		"phase":                       strOrNil(w.Phase),
		"progress":                    orEmptyMap(w.Progress),
		"progress_events":             orEmptyArr(w.ProgressEvents),
		"owner_peer_id":               strOrNil(w.OwnerPeerID),
		"assigned_peer_id":            strOrNil(w.AssignedPeerID),
		"repowire_session_id":         strOrNil(w.RepowireSessionID),
		"correlation_id":              strOrNil(w.CorrelationID),
		"circle":                      strOrNil(w.Circle),
		"created_by_peer_id":          strOrNil(w.CreatedByPeerID),
		"source_kind":                 strOrNil(w.SourceKind),
		"source_id":                   strOrNil(w.SourceID),
		"scope":                       strOrNil(w.Scope),
		"visibility":                  w.Visibility,
		"created_at":                  w.CreatedAt,
		"updated_at":                  w.UpdatedAt,
		"deadline_at":                 strOrNil(w.DeadlineAt),
		"expires_at":                  strOrNil(w.ExpiresAt),
		"result_summary":              strOrNil(w.ResultSummary),
		"cancel_requested":            w.CancelRequested,
		"cancel_requested_at":         strOrNil(w.CancelRequestedAt),
		"cancel_requested_by_peer_id": strOrNil(w.CancelRequestedByPeerID),
		"cancellation_reason":         strOrNil(w.CancellationReason),
		"protocol_cancel":             w.Provenance["protocol_cancel"],
		"request":                     orEmptyMap(w.Request),
		"provenance":                  orEmptyMap(w.Provenance),
		"execution":                   execution,
		"runner":                      mapAt(w.Provenance, "runner"),
		"due_at":                      schedule["due_at"],
		"links":                       mapAt(w.Provenance, "links"),
	}
}

// Result renders the public result dict (TrackedWork.result). Non-terminal work
// returns a not_ready envelope wrapping the full status.
func (w *TrackedWork) Result() map[string]any {
	if !w.Terminal() {
		return map[string]any{
			"job_id":       w.WorkID,
			"work_id":      w.WorkID,
			"result_state": "not_ready",
			"status":       w.Status(),
		}
	}
	return map[string]any{
		"job_id":       w.WorkID,
		"work_id":      w.WorkID,
		"state":        w.State,
		"summary":      strOrNil(w.ResultSummary),
		"data":         orEmptyMap(w.ResultData),
		"error":        orEmptyMap(w.Error),
		"artifacts":    orEmptyArr(w.Artifacts),
		"completed_at": strOrNil(w.CompletedAt),
		"provenance":   orEmptyMap(w.Provenance),
	}
}

// Status renders the public recurring-job dict (CalendarEntry.status).
func (e *CalendarEntry) Status() map[string]any {
	return map[string]any{
		"calendar_id":             e.CalendarID,
		"recurring_id":            e.CalendarID,
		"title":                   e.Title,
		"kind":                    e.Kind,
		"state":                   e.State,
		"cron":                    e.Cron,
		"next_due_at":             e.NextDueAt,
		"owner_peer_id":           strOrNil(e.OwnerPeerID),
		"assigned_peer_id":        strOrNil(e.AssignedPeerID),
		"circle":                  strOrNil(e.Circle),
		"created_by_peer_id":      strOrNil(e.CreatedByPeerID),
		"source_kind":             strOrNil(e.SourceKind),
		"source_id":               strOrNil(e.SourceID),
		"scope":                   strOrNil(e.Scope),
		"visibility":              e.Visibility,
		"request":                 orEmptyMap(e.Request),
		"provenance":              orEmptyMap(e.Provenance),
		"execution":               mapAt(e.Request, "execution"),
		"last_occurrence_work_id": strOrNil(e.LastOccurrenceWorkID),
		"last_materialized_at":    strOrNil(e.LastMaterializedAt),
		"created_at":              e.CreatedAt,
		"updated_at":              e.UpdatedAt,
	}
}

// MaterializeDue creates a tracked-work occurrence for every active calendar
// entry whose next_due_at is at or before now, advances next_due_at via
// nextFire, and records the occurrence. nextFire computes the next firing time
// strictly after `now` for the entry's cron (the hub passes its cron parser so
// the state package stays cron-library-free). Mirrors
// SQLiteCalendarStore.materialize_due.
func (s *Store) MaterializeDue(ctx context.Context, now time.Time, nextFire func(cron string, after time.Time) (time.Time, error)) ([]*TrackedWork, error) {
	active := "active"
	entries, err := s.ListCalendarEntries(ctx, CalendarFilter{State: &active})
	if err != nil {
		return nil, err
	}
	var materialized []*TrackedWork
	for _, entry := range entries {
		due, perr := calendarParseISO(entry.NextDueAt)
		if perr != nil || due.After(now.UTC()) {
			continue
		}
		refreshed, err := s.GetCalendarEntry(ctx, entry.CalendarID)
		if err != nil {
			return nil, err
		}
		if refreshed == nil || refreshed.State != "active" {
			continue
		}
		if rd, rerr := calendarParseISO(refreshed.NextDueAt); rerr == nil && rd.After(now.UTC()) {
			continue
		}
		next, ferr := nextFire(refreshed.Cron, now.UTC())
		if ferr != nil {
			return nil, ferr
		}
		nowText := nowISO()
		prov := calendarCloneMap(refreshed.Provenance)
		prov["last_materialized_reason"] = "due"
		provJSON, jerr := canonicalJSON(prov)
		if jerr != nil {
			return nil, jerr
		}
		work := prepareCalendarOccurrence(refreshed)
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE calendar_entries
			 SET next_due_at = ?, last_occurrence_work_id = ?,
			     last_materialized_at = ?, provenance_json = ?, updated_at = ?
			 WHERE calendar_id = ? AND next_due_at = ? AND state = 'active'`,
			next.Format("2006-01-02T15:04:05.000000-07:00"), work.WorkID,
			nowText, provJSON, nowText, refreshed.CalendarID, refreshed.NextDueAt)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		claimed, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if claimed == 0 {
			_ = tx.Rollback()
			continue
		}
		if err := insertWork(ctx, tx, work); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		materialized = append(materialized, work)
	}
	return materialized, nil
}

// prepareCalendarOccurrence builds the per-fire tracked work from a calendar
// entry, threading the due_at/calendar_id/cron into request.execution.schedule
// and stamping a calendar provenance block. Mirrors _create_occurrence.
func prepareCalendarOccurrence(entry *CalendarEntry) *TrackedWork {
	request := calendarCloneMap(entry.Request)
	execution := calendarCloneMap(mapAt(request, "execution"))
	schedule := calendarCloneMap(mapAt(execution, "schedule"))
	schedule["due_at"] = entry.NextDueAt
	schedule["calendar_id"] = entry.CalendarID
	schedule["cron"] = entry.Cron
	execution["schedule"] = schedule
	request["execution"] = execution
	provenance := map[string]any{
		"calendar": map[string]any{
			"calendar_id":      entry.CalendarID,
			"cron":             entry.Cron,
			"scheduled_due_at": entry.NextDueAt,
		},
	}
	calendar := "calendar"
	return prepareWork(WorkCreate{
		Title:           entry.Title,
		Kind:            entry.Kind,
		CreatedByPeerID: entry.CreatedByPeerID,
		OwnerPeerID:     entry.OwnerPeerID,
		AssignedPeerID:  entry.AssignedPeerID,
		Circle:          entry.Circle,
		SourceKind:      &calendar,
		SourceID:        &entry.CalendarID,
		Scope:           entry.Scope,
		Visibility:      entry.Visibility,
		Request:         request,
		Provenance:      provenance,
	})
}

// mapAt returns m[key] as a map, or an empty map when absent/not-a-map.
func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func orEmptyArr(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}
