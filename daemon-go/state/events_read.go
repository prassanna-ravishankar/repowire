package state

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EventRow is one journal row as offline analysis reads it.
type EventRow struct {
	Type     string
	At       time.Time
	PeerID   string
	PeerName string
	Payload  map[string]any
}

// EventsOfTypes returns the journal rows of the given types, oldest first.
// Bulky text is stripped from every type except ask, which keeps its body:
// analysis that rebuilds state from the journal never needs a chat turn's
// words. Rows with an unparseable timestamp are an error, not skipped.
func (s *Store) EventsOfTypes(ctx context.Context, types ...string) ([]EventRow, error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(types)), ",")
	q := `SELECT type, timestamp, COALESCE(peer_id, ''), COALESCE(peer_name, ''),
		CASE WHEN type = 'ask' THEN payload_json
		     ELSE json_remove(payload_json, '$.text', '$.tool_calls') END
		FROM events WHERE type IN (` + marks + `)`
	args := make([]any, len(types))
	for i, t := range types {
		args[i] = t
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var raw, payload string
		if err := rows.Scan(&e.Type, &raw, &e.PeerID, &e.PeerName, &payload); err != nil {
			return nil, err
		}
		if e.At, err = parseTS(raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, fmt.Errorf("event payload (%s at %s): %w", e.Type, raw, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}
