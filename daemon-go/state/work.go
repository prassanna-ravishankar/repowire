package state

// work.go ports repowire/daemon/state/work.py (SQLiteWorkStore) onto the existing
// state.Store. It reads/writes the schema-v12 `tracked_work` table verbatim; it
// never creates or migrates it. Method semantics match the Python store exactly.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrAttemptIDRequired and ErrStaleAttempt mirror the ValueError/RuntimeError that
// update_state() raises when a runner-managed terminal transition is mis-sequenced.
var (
	ErrAttemptIDRequired = errors.New("attempt_id is required for runner-managed job updates")
	ErrStaleAttempt      = errors.New("stale_attempt")
)

// WorkState is one of the eleven tracked-work lifecycle states (work_store.py).
type WorkState = string

// workStates mirrors WORK_STATES in work_store.py.
var workStates = map[string]struct{}{
	"queued":         {},
	"dispatching":    {},
	"delivered":      {},
	"running":        {},
	"awaiting_input": {},
	"completed":      {},
	"failed":         {},
	"cancelled":      {},
	"blocked":        {},
	"expired":        {},
	"unavailable":    {},
}

// terminalWorkStates mirrors TERMINAL_WORK_STATES in work_store.py.
var terminalWorkStates = map[string]struct{}{
	"completed":   {},
	"failed":      {},
	"cancelled":   {},
	"expired":     {},
	"unavailable": {},
}

// IsTerminalState reports whether state is a terminal work state.
func IsTerminalState(state string) bool {
	_, ok := terminalWorkStates[state]
	return ok
}

// validateWorkState returns the state if valid, else an error (work_store.validate_state).
func validateWorkState(state string) (string, error) {
	if _, ok := workStates[state]; !ok {
		keys := make([]string, 0, len(workStates))
		for k := range workStates {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "", fmt.Errorf("state must be one of %v; got %q", keys, state)
	}
	return state, nil
}

// newWorkID matches work_store.new_work_id(): "work-" + 12 hex chars.
func newWorkID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "work-" + hex.EncodeToString(b[:])
}

// TrackedWork is the daemon-owned lifecycle record (work_store.TrackedWork).
// JSON-blob columns are decoded into generic containers to preserve the Python
// store's untyped semantics.
type TrackedWork struct {
	WorkID                  string
	State                   string
	CreatedAt               string
	UpdatedAt               string
	Title                   string
	Kind                    string
	StateReason             *string
	Phase                   *string
	Progress                map[string]any
	ProgressEvents          []any
	OwnerPeerID             *string
	AssignedPeerID          *string
	RepowireSessionID       *string
	CorrelationID           *string
	Circle                  *string
	CreatedByPeerID         *string
	SourceKind              *string
	SourceID                *string
	Scope                   *string
	Visibility              string
	Request                 map[string]any
	DeadlineAt              *string
	ExpiresAt               *string
	ResultSummary           *string
	ResultData              map[string]any
	Error                   map[string]any
	Artifacts               []any
	Provenance              map[string]any
	CancelRequested         bool
	CancelRequestedAt       *string
	CancelRequestedByPeerID *string
	CancellationReason      *string
	CompletedAt             *string
}

// Terminal mirrors TrackedWork.terminal.
func (w *TrackedWork) Terminal() bool { return IsTerminalState(w.State) }

// WorkCreate carries the optional fields for CreateWork (mirrors create()'s kwargs).
type WorkCreate struct {
	Title             string
	Kind              string // defaults to "general" when empty
	CreatedByPeerID   *string
	OwnerPeerID       *string
	AssignedPeerID    *string
	RepowireSessionID *string
	CorrelationID     *string
	Circle            *string
	SourceKind        *string
	SourceID          *string
	Scope             *string
	Visibility        string // defaults to "circle" when empty
	Request           map[string]any
	DeadlineAt        *string
	ExpiresAt         *string
	Provenance        map[string]any
}

// WorkFilter narrows ListWork (mirrors list_all()'s kwargs). nil fields are unset.
type WorkFilter struct {
	State             *string
	OwnerPeerID       *string
	CreatedByPeerID   *string
	RepowireSessionID *string
	Circle            *string
}

// --- JSON column helpers, matching work_store.json_dumps/json_loads semantics. ---

// dumpJSONObject serializes a map with sorted keys and compact separators.
func dumpJSONObject(m map[string]any) string {
	if m == nil {
		return "{}"
	}
	b, err := marshalJSON(m)
	if err != nil {
		return "{}"
	}
	return b
}

// dumpJSONArray serializes a slice compactly.
func dumpJSONArray(a []any) string {
	if a == nil {
		return "[]"
	}
	b, err := marshalJSON(a)
	if err != nil {
		return "[]"
	}
	return b
}

// loadJSONObject decodes a column into a map, defaulting to empty on null/blank/non-object.
func loadJSONObject(raw sql.NullString) map[string]any {
	if !raw.Valid || raw.String == "" {
		return map[string]any{}
	}
	return decodeJSONObject(raw.String)
}

// loadJSONArray decodes a column into a slice, defaulting to empty on null/blank/non-array.
func loadJSONArray(raw sql.NullString) []any {
	if !raw.Valid || raw.String == "" {
		return []any{}
	}
	var v any
	if err := json.Unmarshal([]byte(raw.String), &v); err != nil {
		return []any{}
	}
	if a, ok := v.([]any); ok {
		return a
	}
	return []any{}
}

const workColumns = `work_id, title, kind, state, state_reason, phase, progress_json,
	progress_events_json, owner_peer_id, assigned_peer_id, repowire_session_id,
	correlation_id, circle, created_by_peer_id, source_kind, source_id, scope,
	visibility, request_json, deadline_at, expires_at, result_summary,
	result_data_json, error_json, artifacts_json, provenance_json, cancel_requested,
	cancel_requested_at, cancel_requested_by_peer_id, cancellation_reason,
	completed_at, created_at, updated_at`

// scanWork reads one tracked_work row in workColumns order.
func scanWork(row interface{ Scan(...any) error }) (*TrackedWork, error) {
	var (
		workID, state, visibility, title, kind           string
		createdAt, updatedAt                             string
		stateReason, phase                               sql.NullString
		progressJSON, progressEventsJSON                 sql.NullString
		ownerPeerID, assignedPeerID, repowireSessionID   sql.NullString
		correlationID, circle, createdByPeerID           sql.NullString
		sourceKind, sourceID, scope                      sql.NullString
		requestJSON, deadlineAt, expiresAt, resultSummry sql.NullString
		resultDataJSON, errorJSON, artifactsJSON         sql.NullString
		provenanceJSON                                   sql.NullString
		cancelRequested                                  int64
		cancelRequestedAt, cancelRequestedBy             sql.NullString
		cancellationReason, completedAt                  sql.NullString
	)
	if err := row.Scan(
		&workID, &title, &kind, &state, &stateReason, &phase, &progressJSON,
		&progressEventsJSON, &ownerPeerID, &assignedPeerID, &repowireSessionID,
		&correlationID, &circle, &createdByPeerID, &sourceKind, &sourceID, &scope,
		&visibility, &requestJSON, &deadlineAt, &expiresAt, &resultSummry,
		&resultDataJSON, &errorJSON, &artifactsJSON, &provenanceJSON, &cancelRequested,
		&cancelRequestedAt, &cancelRequestedBy, &cancellationReason,
		&completedAt, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	validated, err := validateWorkState(state)
	if err != nil {
		return nil, err
	}
	return &TrackedWork{
		WorkID:                  workID,
		Title:                   title,
		Kind:                    kind,
		State:                   validated,
		StateReason:             nullStringPtr(stateReason),
		Phase:                   nullStringPtr(phase),
		Progress:                loadJSONObject(progressJSON),
		ProgressEvents:          loadJSONArray(progressEventsJSON),
		OwnerPeerID:             nullStringPtr(ownerPeerID),
		AssignedPeerID:          nullStringPtr(assignedPeerID),
		RepowireSessionID:       nullStringPtr(repowireSessionID),
		CorrelationID:           nullStringPtr(correlationID),
		Circle:                  nullStringPtr(circle),
		CreatedByPeerID:         nullStringPtr(createdByPeerID),
		SourceKind:              nullStringPtr(sourceKind),
		SourceID:                nullStringPtr(sourceID),
		Scope:                   nullStringPtr(scope),
		Visibility:              visibility,
		Request:                 loadJSONObject(requestJSON),
		DeadlineAt:              nullStringPtr(deadlineAt),
		ExpiresAt:               nullStringPtr(expiresAt),
		ResultSummary:           nullStringPtr(resultSummry),
		ResultData:              loadJSONObject(resultDataJSON),
		Error:                   loadJSONObject(errorJSON),
		Artifacts:               loadJSONArray(artifactsJSON),
		Provenance:              loadJSONObject(provenanceJSON),
		CancelRequested:         cancelRequested != 0,
		CancelRequestedAt:       nullStringPtr(cancelRequestedAt),
		CancelRequestedByPeerID: nullStringPtr(cancelRequestedBy),
		CancellationReason:      nullStringPtr(cancellationReason),
		CompletedAt:             nullStringPtr(completedAt),
		CreatedAt:               createdAt,
		UpdatedAt:               updatedAt,
	}, nil
}

// CreateWork inserts a new tracked_work row in the "queued" state and returns it.
func (s *Store) CreateWork(ctx context.Context, in WorkCreate) (*TrackedWork, error) {
	now := nowISO()
	kind := in.Kind
	if kind == "" {
		kind = "general"
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = "circle"
	}
	w := &TrackedWork{
		WorkID:            newWorkID(),
		Title:             in.Title,
		Kind:              kind,
		State:             "queued",
		Progress:          orEmptyMap(nil),
		ProgressEvents:    []any{},
		OwnerPeerID:       in.OwnerPeerID,
		AssignedPeerID:    in.AssignedPeerID,
		RepowireSessionID: in.RepowireSessionID,
		CorrelationID:     in.CorrelationID,
		Circle:            in.Circle,
		CreatedByPeerID:   in.CreatedByPeerID,
		SourceKind:        in.SourceKind,
		SourceID:          in.SourceID,
		Scope:             in.Scope,
		Visibility:        visibility,
		Request:           orEmptyMap(in.Request),
		DeadlineAt:        in.DeadlineAt,
		ExpiresAt:         in.ExpiresAt,
		ResultData:        map[string]any{},
		Error:             map[string]any{},
		Artifacts:         []any{},
		Provenance:        orEmptyMap(in.Provenance),
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	const q = `INSERT INTO tracked_work(` + workColumns + `) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		w.WorkID, w.Title, w.Kind, w.State, strOrNil(w.StateReason), strOrNil(w.Phase),
		dumpJSONObject(w.Progress), dumpJSONArray(w.ProgressEvents),
		strOrNil(w.OwnerPeerID), strOrNil(w.AssignedPeerID), strOrNil(w.RepowireSessionID),
		strOrNil(w.CorrelationID), strOrNil(w.Circle), strOrNil(w.CreatedByPeerID),
		strOrNil(w.SourceKind), strOrNil(w.SourceID), strOrNil(w.Scope),
		w.Visibility, dumpJSONObject(w.Request), strOrNil(w.DeadlineAt), strOrNil(w.ExpiresAt),
		strOrNil(w.ResultSummary), dumpJSONObject(w.ResultData), dumpJSONObject(w.Error),
		dumpJSONArray(w.Artifacts), dumpJSONObject(w.Provenance), boolToInt(w.CancelRequested),
		strOrNil(w.CancelRequestedAt), strOrNil(w.CancelRequestedByPeerID),
		strOrNil(w.CancellationReason), strOrNil(w.CompletedAt), w.CreatedAt, w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create work %s: %w", w.WorkID, err)
	}
	return w, nil
}

// GetWork fetches one tracked_work row, returning (nil, nil) if absent.
func (s *Store) GetWork(ctx context.Context, workID string) (*TrackedWork, error) {
	const q = `SELECT ` + workColumns + ` FROM tracked_work WHERE work_id = ?`
	w, err := scanWork(s.db.QueryRowContext(ctx, q, workID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan work %s: %w", workID, err)
	}
	return w, nil
}

// ListWork returns matching rows ordered by updated_at DESC (list_all()).
func (s *Store) ListWork(ctx context.Context, f WorkFilter) ([]*TrackedWork, error) {
	var clauses []string
	var params []any
	if f.State != nil {
		if _, err := validateWorkState(*f.State); err != nil {
			return nil, err
		}
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
	if f.RepowireSessionID != nil {
		clauses = append(clauses, "repowire_session_id = ?")
		params = append(params, *f.RepowireSessionID)
	}
	if f.Circle != nil {
		clauses = append(clauses, "circle = ?")
		params = append(params, *f.Circle)
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	q := `SELECT ` + workColumns + ` FROM tracked_work ` + where + ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, params...)
	if err != nil {
		return nil, fmt.Errorf("list work: %w", err)
	}
	defer rows.Close()
	var out []*TrackedWork
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, fmt.Errorf("scan work row: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkUpdate carries the optional fields for UpdateWorkState (update_state()'s kwargs).
// nil pointer/slice/map fields are left unchanged, matching the Python "is not None" guards.
type WorkUpdate struct {
	State         string
	StateReason   *string
	Phase         *string
	Progress      map[string]any
	ProgressNote  *string
	ResultSummary *string
	ResultData    map[string]any
	Error         map[string]any
	Artifacts     []any
	Provenance    map[string]any
	AttemptID     *string
}

// UpdateWorkState applies a state transition (update_state()). It enforces the
// runner-managed attempt guard and the terminal-immutability rule, returning a
// typed error on violation. Returns (nil, nil) when the work_id is unknown.
func (s *Store) UpdateWorkState(ctx context.Context, workID string, u WorkUpdate) (*TrackedWork, error) {
	if _, err := validateWorkState(u.State); err != nil {
		return nil, err
	}
	existing, err := s.GetWork(ctx, workID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	currentAttempt := runnerCurrentAttempt(existing)
	if currentAttempt != "" && (u.State == "completed" || u.State == "failed" || u.State == "cancelled") {
		if u.AttemptID == nil || *u.AttemptID == "" {
			return nil, ErrAttemptIDRequired
		}
		if *u.AttemptID != currentAttempt {
			return nil, ErrStaleAttempt
		}
	}
	if existing.Terminal() && u.State != existing.State {
		return nil, fmt.Errorf("terminal work %s is already %s; terminal state cannot be changed", workID, existing.State)
	}

	completedAt := existing.CompletedAt
	if IsTerminalState(u.State) && completedAt == nil {
		v := nowISO()
		completedAt = &v
	}

	progressEvents := append([]any{}, existing.ProgressEvents...)
	if u.ProgressNote != nil && *u.ProgressNote != "" {
		phase := strOrNil(existing.Phase)
		if u.Phase != nil {
			phase = *u.Phase
		}
		progressEvents = append(progressEvents, map[string]any{
			"at":    nowISO(),
			"note":  *u.ProgressNote,
			"state": u.State,
			"phase": phase,
		})
	}

	progress := existing.Progress
	if u.Progress != nil {
		progress = u.Progress
	}
	resultSummary := existing.ResultSummary
	if u.ResultSummary != nil {
		resultSummary = u.ResultSummary
	}
	resultData := existing.ResultData
	if u.ResultData != nil {
		resultData = u.ResultData
	}
	errObj := existing.Error
	if u.Error != nil {
		errObj = u.Error
	}
	artifacts := existing.Artifacts
	if u.Artifacts != nil {
		artifacts = u.Artifacts
	}
	provenance := existing.Provenance
	if u.Provenance != nil {
		provenance = u.Provenance
	}

	const q = `UPDATE tracked_work SET
		state = ?, state_reason = ?, phase = ?, progress_json = ?,
		progress_events_json = ?, result_summary = ?, result_data_json = ?,
		error_json = ?, artifacts_json = ?, provenance_json = ?,
		completed_at = ?, updated_at = ?
		WHERE work_id = ?`
	_, err = s.db.ExecContext(ctx, q,
		u.State, strOrNil(u.StateReason), strOrNil(u.Phase), dumpJSONObject(progress),
		dumpJSONArray(progressEvents), strOrNil(resultSummary), dumpJSONObject(resultData),
		dumpJSONObject(errObj), dumpJSONArray(artifacts), dumpJSONObject(provenance),
		strOrNil(completedAt), nowISO(), workID,
	)
	if err != nil {
		return nil, fmt.Errorf("update work %s: %w", workID, err)
	}
	return s.GetWork(ctx, workID)
}

// CancelWork requests cancellation (cancel()). queued work transitions straight
// to cancelled; non-terminal in-flight work is flagged for cooperative cancel;
// terminal work records the request without changing state.
func (s *Store) CancelWork(ctx context.Context, workID string, requestedByPeerID *string, reason string) (*TrackedWork, error) {
	if reason == "" {
		reason = "cancel_requested"
	}
	existing, err := s.GetWork(ctx, workID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}
	requestedAt := nowISO()

	if existing.Terminal() {
		const q = `UPDATE tracked_work SET
			cancel_requested = 1,
			cancel_requested_at = COALESCE(cancel_requested_at, ?),
			cancel_requested_by_peer_id = COALESCE(cancel_requested_by_peer_id, ?),
			cancellation_reason = COALESCE(cancellation_reason, ?),
			updated_at = ?
			WHERE work_id = ?`
		if _, err := s.db.ExecContext(ctx, q, requestedAt, strOrNil(requestedByPeerID), reason, requestedAt, workID); err != nil {
			return nil, fmt.Errorf("cancel terminal work %s: %w", workID, err)
		}
		return s.GetWork(ctx, workID)
	}

	if existing.State == "queued" {
		const q = `UPDATE tracked_work SET
			state = 'cancelled', state_reason = ?, cancel_requested = 1,
			cancel_requested_at = ?, cancel_requested_by_peer_id = ?,
			cancellation_reason = ?, completed_at = ?, updated_at = ?
			WHERE work_id = ?`
		if _, err := s.db.ExecContext(ctx, q, reason, requestedAt, strOrNil(requestedByPeerID), reason, requestedAt, requestedAt, workID); err != nil {
			return nil, fmt.Errorf("cancel queued work %s: %w", workID, err)
		}
		return s.GetWork(ctx, workID)
	}

	const q = `UPDATE tracked_work SET
		state_reason = ?, cancel_requested = 1, cancel_requested_at = ?,
		cancel_requested_by_peer_id = ?, cancellation_reason = ?, updated_at = ?
		WHERE work_id = ?`
	if _, err := s.db.ExecContext(ctx, q, reason, requestedAt, strOrNil(requestedByPeerID), reason, requestedAt, workID); err != nil {
		return nil, fmt.Errorf("cancel in-flight work %s: %w", workID, err)
	}
	return s.GetWork(ctx, workID)
}

// AcquireOptions tunes AcquireForDispatch (acquire_for_dispatch()'s kwargs).
type AcquireOptions struct {
	RunnerOwnerID string
	LeaseUntil    string
	AttemptID     string // generated when empty
	IgnoreDueAt   bool
	Retry         bool
}

// AcquireForDispatch claims work for a runner, recording a fresh attempt and
// transitioning to "dispatching" (acquire_for_dispatch()). It is the retry path:
// with Retry=true it admits failed/unavailable/delivered work; otherwise
// queued/failed/unavailable. Returns (nil, nil) when the work is absent,
// cancel-requested, in a disallowed state, or not yet due.
func (s *Store) AcquireForDispatch(ctx context.Context, workID string, opt AcquireOptions) (*TrackedWork, error) {
	now := nowISO()
	attemptID := opt.AttemptID
	if attemptID == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		attemptID = "attempt-" + hex.EncodeToString(b[:])
	}

	existing, err := s.GetWork(ctx, workID)
	if err != nil {
		return nil, err
	}
	if existing == nil || existing.CancelRequested {
		return nil, nil
	}
	allowed := map[string]struct{}{"queued": {}, "failed": {}, "unavailable": {}}
	if opt.Retry {
		allowed = map[string]struct{}{"failed": {}, "unavailable": {}, "delivered": {}}
	}
	if _, ok := allowed[existing.State]; !ok {
		return nil, nil
	}
	if dueAt := requestDueAt(existing); dueAt != "" && !opt.IgnoreDueAt {
		due, perr := workParseISO(dueAt)
		if perr == nil && due.After(time.Now().UTC()) {
			return nil, nil
		}
	}

	provenance := runnerProvenance(existing)
	runner := provenance["runner"].(map[string]any)
	attempts := toAnySlice(runner["attempts"])
	attempts = append(attempts, map[string]any{
		"attempt_id":         attemptID,
		"status":             "dispatching",
		"phase":              "acquired",
		"started_at":         now,
		"completed_at":       nil,
		"runner_owner_id":    opt.RunnerOwnerID,
		"lease_until":        opt.LeaseUntil,
		"assigned_peer_id":   strOrNil(existing.AssignedPeerID),
		"assigned_peer_info": map[string]any{},
		"tmux":               map[string]any{},
		"correlation_id":     nil,
		"delivery_state":     nil,
		"error":              map[string]any{},
	})
	runner["attempt_count"] = len(attempts)
	runner["current_attempt_id"] = attemptID
	runner["runner_owner_id"] = opt.RunnerOwnerID
	runner["acquired_at"] = now
	runner["lease_until"] = opt.LeaseUntil
	runner["attempts"] = attempts
	provenance["runner"] = runner

	const q = `UPDATE tracked_work SET
		state = ?, state_reason = ?, phase = ?, assigned_peer_id = ?,
		correlation_id = ?, provenance_json = ?, error_json = ?,
		completed_at = ?, updated_at = ?
		WHERE work_id = ?`
	_, err = s.db.ExecContext(ctx, q,
		"dispatching", "dispatching", "acquired", strOrNil(existing.AssignedPeerID),
		strOrNil(existing.CorrelationID), dumpJSONObject(provenance), "{}",
		nil, nowISO(), workID,
	)
	if err != nil {
		return nil, fmt.Errorf("acquire work %s: %w", workID, err)
	}
	return s.GetWork(ctx, workID)
}

// --- runner-provenance helpers, matching work.py module functions. ---

// runnerProvenance returns a copy of provenance with a normalized runner block
// (_runner_provenance in work.py).
func runnerProvenance(w *TrackedWork) map[string]any {
	provenance := map[string]any{}
	for k, v := range w.Provenance {
		provenance[k] = v
	}
	runner := map[string]any{}
	if r, ok := provenance["runner"].(map[string]any); ok {
		for k, v := range r {
			runner[k] = v
		}
	}
	attempts := toAnySlice(runner["attempts"])
	if _, ok := runner["attempt_count"]; !ok {
		runner["attempt_count"] = len(attempts)
	}
	if _, ok := runner["attempts"]; !ok {
		runner["attempts"] = []any{}
	}
	provenance["runner"] = runner
	return provenance
}

// runnerCurrentAttempt returns the non-empty current_attempt_id, else ""
// (_runner_current_attempt in work.py).
func runnerCurrentAttempt(w *TrackedWork) string {
	runner, ok := w.Provenance["runner"].(map[string]any)
	if !ok {
		return ""
	}
	if cur, ok := runner["current_attempt_id"].(string); ok {
		return cur
	}
	return ""
}

// requestDueAt extracts request.execution.schedule.due_at, or "".
func requestDueAt(w *TrackedWork) string {
	exec, ok := w.Request["execution"].(map[string]any)
	if !ok {
		return ""
	}
	sched, ok := exec["schedule"].(map[string]any)
	if !ok {
		return ""
	}
	if due, ok := sched["due_at"].(string); ok {
		return due
	}
	return ""
}

// parseISO parses the ISO timestamps work.py writes (_parse_iso), normalizing to UTC.
func workParseISO(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unparseable ISO timestamp %q", value)
}

func toAnySlice(v any) []any {
	if a, ok := v.([]any); ok {
		return append([]any{}, a...)
	}
	return []any{}
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
