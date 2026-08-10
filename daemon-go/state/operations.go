package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
)

// opJSONLoadsArray mirrors json_loads(raw, []) — returns a slice, [] on empty/non-array.
func opJSONLoadsArray(raw string) []map[string]any {
	if raw == "" {
		return []map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return []map[string]any{}
	}
	arr, ok := v.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		} else {
			out = append(out, map[string]any{})
		}
	}
	return out
}

// Operation mirrors operations.Operation. JSON columns are decoded into
// generic maps/slices since their shapes are caller-defined.
type Operation struct {
	OperationID string
	Kind        string
	State       string
	Target      map[string]any
	Strategy    *string
	Attempts    []map[string]any
	Result      map[string]any
	Error       map[string]any
	Provenance  map[string]any
	CreatedAt   string
	UpdatedAt   string
	CompletedAt *string
}

func scanOperation(row interface {
	Scan(dest ...any) error
}) (*Operation, error) {
	var (
		operationID  string
		kind         string
		state        string
		targetJSON   string
		strategy     sql.NullString
		attemptsJSON string
		resultJSON   string
		errorJSON    string
		provJSON     string
		createdAt    string
		updatedAt    string
		completedAt  sql.NullString
	)
	if err := row.Scan(
		&operationID, &kind, &state, &targetJSON, &strategy,
		&attemptsJSON, &resultJSON, &errorJSON, &provJSON,
		&createdAt, &updatedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	op := &Operation{
		OperationID: operationID,
		Kind:        kind,
		State:       state,
		Target:      decodeJSONObject(targetJSON),
		Attempts:    opJSONLoadsArray(attemptsJSON),
		Result:      decodeJSONObject(resultJSON),
		Error:       decodeJSONObject(errorJSON),
		Provenance:  decodeJSONObject(provJSON),
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}
	if strategy.Valid {
		s := strategy.String
		op.Strategy = &s
	}
	if completedAt.Valid {
		c := completedAt.String
		op.CompletedAt = &c
	}
	return op, nil
}

const operationColumns = `operation_id, kind, state, target_json, strategy,
	attempts_json, result_json, error_json, provenance_json,
	created_at, updated_at, completed_at`

// CreateOperation mirrors SQLiteOperationStore.create: inserts a queued operation
// with a fresh op id and now-stamped created_at/updated_at.
func (s *Store) CreateOperation(ctx context.Context, kind string, target, provenance map[string]any) (*Operation, error) {
	if target == nil {
		target = map[string]any{}
	}
	if provenance == nil {
		provenance = map[string]any{}
	}
	now := nowISO()
	op := &Operation{
		OperationID: newID("op-"),
		Kind:        kind,
		State:       "queued",
		Target:      target,
		Attempts:    []map[string]any{},
		Result:      map[string]any{},
		Error:       map[string]any{},
		Provenance:  provenance,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	targetJSON, err := marshalJSON(op.Target)
	if err != nil {
		return nil, fmt.Errorf("marshal target: %w", err)
	}
	attemptsJSON, err := marshalJSON(op.Attempts)
	if err != nil {
		return nil, fmt.Errorf("marshal attempts: %w", err)
	}
	resultJSON, err := marshalJSON(op.Result)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	errorJSON, err := marshalJSON(op.Error)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}
	provJSON, err := marshalJSON(op.Provenance)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}

	const q = `INSERT INTO operations(
		operation_id, kind, state, target_json, strategy,
		attempts_json, result_json, error_json, provenance_json,
		created_at, updated_at, completed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = s.db.ExecContext(ctx, q,
		op.OperationID, op.Kind, op.State, targetJSON, nil,
		attemptsJSON, resultJSON, errorJSON, provJSON,
		op.CreatedAt, op.UpdatedAt, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("insert operation %s: %w", op.OperationID, err)
	}
	return op, nil
}

// GetOperation mirrors SQLiteOperationStore.get: returns (nil, nil) when missing.
func (s *Store) GetOperation(ctx context.Context, operationID string) (*Operation, error) {
	const q = `SELECT ` + operationColumns + ` FROM operations WHERE operation_id = ?`
	op, err := scanOperation(s.db.QueryRowContext(ctx, q, operationID))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get operation %s: %w", operationID, err)
	}
	return op, nil
}

// ListOperations mirrors SQLiteOperationStore.list_all: optional kind/state filters
// (empty string means no filter), ordered by updated_at DESC.
func (s *Store) ListOperations(ctx context.Context, kind, state string) ([]*Operation, error) {
	q := `SELECT ` + operationColumns + ` FROM operations`
	var clauses []string
	var args []any
	if kind != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, kind)
	}
	if state != "" {
		clauses = append(clauses, "state = ?")
		args = append(args, state)
	}
	if len(clauses) > 0 {
		q += " WHERE " + clauses[0]
		for _, c := range clauses[1:] {
			q += " AND " + c
		}
	}
	q += " ORDER BY updated_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	defer rows.Close()

	var out []*Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan operation: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operations: %w", err)
	}
	return out, nil
}

// StartAttempt mirrors SQLiteOperationStore.start_attempt: appends a running
// attempt and transitions the operation to running. Returns (nil, nil) when missing.
func (s *Store) StartAttempt(ctx context.Context, operationID string, strategy *string, detail map[string]any) (*Operation, error) {
	op, err := s.GetOperation(ctx, operationID)
	if err != nil || op == nil {
		return op, err
	}
	if detail == nil {
		detail = map[string]any{}
	}
	attempts := append([]map[string]any{}, op.Attempts...)
	attempts = append(attempts, map[string]any{
		"attempt_id":   newID("op-attempt-"),
		"state":        "running",
		"strategy":     opStrategyValue(strategy),
		"detail":       detail,
		"started_at":   nowISO(),
		"completed_at": nil,
		"error":        map[string]any{},
	})
	nextStrategy := op.Strategy
	if strategy != nil {
		nextStrategy = strategy
	}
	return s.updateOperation(ctx, op, "running", nextStrategy, attempts, nil, nil, nil)
}

// CompleteOperation mirrors SQLiteOperationStore.complete: marks the last attempt
// and the operation completed. Returns (nil, nil) when missing.
func (s *Store) CompleteOperation(ctx context.Context, operationID string, strategy *string, result map[string]any) (*Operation, error) {
	op, err := s.GetOperation(ctx, operationID)
	if err != nil || op == nil {
		return op, err
	}
	if result == nil {
		result = map[string]any{}
	}
	now := nowISO()
	attempts := append([]map[string]any{}, op.Attempts...)
	if n := len(attempts); n > 0 {
		last := opCloneMap(attempts[n-1])
		last["state"] = "completed"
		last["completed_at"] = now
		last["result"] = result
		attempts[n-1] = last
	}
	nextStrategy := op.Strategy
	if strategy != nil {
		nextStrategy = strategy
	}
	return s.updateOperation(ctx, op, "completed", nextStrategy, attempts, result, nil, &now)
}

// FailOperation mirrors SQLiteOperationStore.fail: marks the last attempt and the
// operation with the given state (empty defaults to "failed"). Returns (nil, nil) when missing.
func (s *Store) FailOperation(ctx context.Context, operationID, state string, strategy *string, opErr map[string]any) (*Operation, error) {
	op, err := s.GetOperation(ctx, operationID)
	if err != nil || op == nil {
		return op, err
	}
	if state == "" {
		state = "failed"
	}
	if opErr == nil {
		opErr = map[string]any{}
	}
	now := nowISO()
	attempts := append([]map[string]any{}, op.Attempts...)
	if n := len(attempts); n > 0 {
		last := opCloneMap(attempts[n-1])
		last["state"] = state
		last["completed_at"] = now
		last["error"] = opErr
		attempts[n-1] = last
	}
	nextStrategy := op.Strategy
	if strategy != nil {
		nextStrategy = strategy
	}
	return s.updateOperation(ctx, op, state, nextStrategy, attempts, nil, opErr, &now)
}

// updateOperation mirrors SQLiteOperationStore._update: persists the mutated
// columns with a fresh updated_at and re-reads the row.
func (s *Store) updateOperation(
	ctx context.Context,
	op *Operation,
	state string,
	strategy *string,
	attempts []map[string]any,
	result map[string]any,
	opErr map[string]any,
	completedAt *string,
) (*Operation, error) {
	now := nowISO()
	if attempts == nil {
		attempts = op.Attempts
	}
	if result == nil {
		result = op.Result
	}
	if opErr == nil {
		opErr = op.Error
	}

	attemptsJSON, err := marshalJSON(attempts)
	if err != nil {
		return nil, fmt.Errorf("marshal attempts: %w", err)
	}
	resultJSON, err := marshalJSON(result)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	errorJSON, err := marshalJSON(opErr)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	const q = `UPDATE operations
		SET state = ?, strategy = ?, attempts_json = ?, result_json = ?,
		    error_json = ?, updated_at = ?, completed_at = ?
		WHERE operation_id = ?`
	_, err = s.db.ExecContext(ctx, q,
		state, strOrNil(strategy), attemptsJSON, resultJSON,
		errorJSON, now, strOrNil(completedAt),
		op.OperationID,
	)
	if err != nil {
		return nil, fmt.Errorf("update operation %s: %w", op.OperationID, err)
	}
	return s.GetOperation(ctx, op.OperationID)
}

// opStrategyValue renders an optional strategy as a JSON-friendly value (nil → null),
// matching Python's None passthrough into attempt records.
func opStrategyValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// opCloneMap shallow-copies a map so attempt mutation doesn't alias the decoded slice.
func opCloneMap(m map[string]any) map[string]any {
	return maps.Clone(m)
}
