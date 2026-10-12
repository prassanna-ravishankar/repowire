package state

import (
	"context"
	"fmt"
)

// AskRef is one ask as the durable stores record it: who sent it to whom.
type AskRef struct {
	CorrelationID string
	From, To      string // peer ids
}

// AskRefs returns every ask the stores remember, once per correlation id.
// Asks before 2026-08-10 exist only as events-journal rows; later ones only as
// the delivery-trace ledger's "created" stage, so both are read and merged.
// (ask_many children write no "created" stage and are not included.) Both
// reads use an index (type+timestamp, stage+ts); the result is meant to be
// cached by the caller, not queried per decision.
func (s *Store) AskRefs(ctx context.Context) ([]AskRef, error) {
	seen := map[string]struct{}{}
	var out []AskRef
	collect := func(query string) error {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cid, from, to string
			if err := rows.Scan(&cid, &from, &to); err != nil {
				return err
			}
			if cid == "" {
				continue
			}
			if _, dup := seen[cid]; dup {
				continue
			}
			seen[cid] = struct{}{}
			out = append(out, AskRef{CorrelationID: cid, From: from, To: to})
		}
		return rows.Err()
	}
	if err := collect(`SELECT COALESCE(json_extract(payload_json, '$.correlation_id'), ''),
			COALESCE(json_extract(payload_json, '$.from_peer_id'), ''),
			COALESCE(json_extract(payload_json, '$.to_peer_id'), '')
		FROM events WHERE type = 'ask'`); err != nil {
		return nil, fmt.Errorf("load ask events: %w", err)
	}
	if err := collect(`SELECT trace_id, COALESCE(from_peer_id, ''), COALESCE(peer_id, '')
		FROM delivery_traces WHERE kind = 'ask' AND stage = 'created'`); err != nil {
		return nil, fmt.Errorf("load ask traces: %w", err)
	}
	return out, nil
}
