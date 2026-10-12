package state

import (
	"context"
	"database/sql"
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
// (ask_many children and /query asks write no "created" stage and are not
// included.) Both reads use an index (type+timestamp, stage+ts).
//
// The store's own connection is the daemon's only one, so a scan on it stalls
// every other store call for its whole duration. AskRefs therefore reads on a
// separate read-only connection to the same file (WAL lets readers run beside
// the writer) and closes it before returning.
func (s *Store) AskRefs(ctx context.Context) ([]AskRef, error) {
	var out []AskRef
	err := s.askRefs(ctx, func(r AskRef) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

// askRefs streams the deduplicated asks to each.
func (s *Store) askRefs(ctx context.Context, each func(AskRef) error) error {
	db := s.db
	if s.path != "" && s.path != ":memory:" {
		ro, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", s.path))
		if err != nil {
			return fmt.Errorf("open read-only state db: %w", err)
		}
		ro.SetMaxOpenConns(1)
		defer ro.Close()
		db = ro
	}
	seen := map[string]struct{}{}
	collect := func(query string) error {
		rows, err := db.QueryContext(ctx, query)
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
			if err := each(AskRef{CorrelationID: cid, From: from, To: to}); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := collect(`SELECT COALESCE(json_extract(payload_json, '$.correlation_id'), ''),
			COALESCE(json_extract(payload_json, '$.from_peer_id'), ''),
			COALESCE(json_extract(payload_json, '$.to_peer_id'), '')
		FROM events WHERE type = 'ask'`); err != nil {
		return fmt.Errorf("load ask events: %w", err)
	}
	if err := collect(`SELECT trace_id, COALESCE(from_peer_id, ''), COALESCE(peer_id, '')
		FROM delivery_traces WHERE kind = 'ask' AND stage = 'created'`); err != nil {
		return fmt.Errorf("load ask traces: %w", err)
	}
	return nil
}
