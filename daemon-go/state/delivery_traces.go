package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TraceRow is one recorded delivery stage. Mirrors delivery_trace.TraceRow in
// the Python daemon: rows for one message share a trace_id and carry a
// monotonic seq so stages render in true order even when timestamps tie.
type TraceRow struct {
	TraceID    string
	DeliveryID string
	Seq        int
	Kind       string
	Stage      string
	Status     string
	PeerID     string
	FromPeerID string
	TS         string
	Detail     map[string]any
}

// RecordTrace appends one delivery-trace stage row. The per-trace seq is
// computed inside the INSERT as a scalar subquery so read+write are one atomic
// statement (no select-then-insert race for the next ordinal), exactly as the
// Python DeliveryTraceStore.record does.
//
// status defaults to "ok" when empty; deliveryID defaults to trace_id when
// empty; detail defaults to {}. peerID/fromPeerID are stored NULL when empty.
func (s *Store) RecordTrace(
	ctx context.Context,
	traceID, kind, stage, status, deliveryID, peerID, fromPeerID string,
	detail map[string]any,
) error {
	if status == "" {
		status = "ok"
	}
	if deliveryID == "" {
		deliveryID = traceID
	}
	detailJSON := []byte("{}")
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("marshal trace detail: %w", err)
		}
		detailJSON = b
	}
	ts := time.Now().UTC().Format(tsLayout)

	const q = `INSERT INTO delivery_traces(
		id, trace_id, delivery_id, seq, kind, stage, status,
		peer_id, from_peer_id, ts, detail_json
	) VALUES (
		?, ?, ?,
		(SELECT COALESCE(MAX(seq), -1) + 1 FROM delivery_traces WHERE trace_id = ?),
		?, ?, ?, ?, ?, ?, ?
	)`
	_, err := s.db.ExecContext(ctx, q,
		uuid.NewString(),
		traceID,
		deliveryID,
		traceID,
		kind,
		stage,
		status,
		nullString(peerID),
		nullString(fromPeerID),
		ts,
		string(detailJSON),
	)
	if err != nil {
		return fmt.Errorf("record trace stage %s: %w", stage, err)
	}
	return nil
}

// StagesFor returns all recorded stages for a trace, in seq order.
func (s *Store) StagesFor(ctx context.Context, traceID string) ([]TraceRow, error) {
	const q = `SELECT trace_id, delivery_id, seq, kind, stage, status,
		       peer_id, from_peer_id, ts, detail_json
		FROM delivery_traces
		WHERE trace_id = ?
		ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q, traceID)
	if err != nil {
		return nil, fmt.Errorf("load trace stages %s: %w", traceID, err)
	}
	defer rows.Close()

	var out []TraceRow
	for rows.Next() {
		tr, err := scanTraceRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trace stages: %w", err)
	}
	return out, nil
}

// defaultInboundStages are the terminal hook-receipt stages list_peers uses to
// derive per-peer inbound health (last_success_at / last_failure_at).
var defaultInboundStages = []string{"pane_injected", "thread_input_accepted", "injection_failed"}

// LatestStagesForPeers returns the newest ts per (peer_id, stage) for a set of
// peers in one query, keyed by [2]string{peer_id, stage}. Only present keys are
// included. When stages is empty it falls back to pane_injected/injection_failed.
// Mirrors Python latest_stages_for_peers, used to compute inbound health
// without 2N per-peer round trips.
func (s *Store) LatestStagesForPeers(
	ctx context.Context,
	peerIDs []string,
	stages ...string,
) (map[[2]string]string, error) {
	if len(stages) == 0 {
		stages = defaultInboundStages
	}
	if len(peerIDs) == 0 {
		return map[[2]string]string{}, nil
	}

	args := make([]any, 0, len(peerIDs)+len(stages))
	for _, p := range peerIDs {
		args = append(args, p)
	}
	for _, st := range stages {
		args = append(args, st)
	}
	q := fmt.Sprintf(`SELECT peer_id, stage, MAX(ts) AS ts
		FROM delivery_traces
		WHERE peer_id IN (%s) AND stage IN (%s)
		GROUP BY peer_id, stage`,
		placeholders(len(peerIDs)),
		placeholders(len(stages)),
	)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("latest stages for peers: %w", err)
	}
	defer rows.Close()

	out := make(map[[2]string]string)
	for rows.Next() {
		var (
			peerID string
			stage  string
			ts     sql.NullString
		)
		if err := rows.Scan(&peerID, &stage, &ts); err != nil {
			return nil, fmt.Errorf("scan latest stage: %w", err)
		}
		if ts.Valid {
			out[[2]string{peerID, stage}] = ts.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest stages: %w", err)
	}
	return out, nil
}

// scanTraceRow reads one delivery_traces row into a TraceRow, tolerating NULL
// peer/from_peer and corrupt detail_json (defaults to {}, matching Python).
func scanTraceRow(rows *sql.Rows) (TraceRow, error) {
	var (
		traceID    string
		deliveryID sql.NullString
		seq        int
		kind       string
		stage      string
		status     string
		peerID     sql.NullString
		fromPeerID sql.NullString
		ts         string
		detailJSON string
	)
	if err := rows.Scan(&traceID, &deliveryID, &seq, &kind, &stage, &status,
		&peerID, &fromPeerID, &ts, &detailJSON); err != nil {
		return TraceRow{}, fmt.Errorf("scan trace row: %w", err)
	}
	detail := map[string]any{}
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		detail = map[string]any{}
	}
	return TraceRow{
		TraceID:    traceID,
		DeliveryID: deliveryID.String,
		Seq:        seq,
		Kind:       kind,
		Stage:      stage,
		Status:     status,
		PeerID:     peerID.String,
		FromPeerID: fromPeerID.String,
		TS:         ts,
		Detail:     detail,
	}, nil
}

// placeholders returns "?,?,..." with n marks.
func placeholders(n int) string {
	return strings.Repeat(",?", n)[1:]
}

// nullString stores empty strings as SQL NULL.
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
