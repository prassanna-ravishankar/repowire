package state

import (
	"context"
	"fmt"
	"time"
)

// SetPeerWireVisibility records peerID's wire visibility
// (rfcs/wire-observability.md section 2).
func (s *Store) SetPeerWireVisibility(ctx context.Context, peerID, mode string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO peer_wire_visibility(peer_id, mode, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
		peerID, mode, time.Now().UTC().Format(tsLayout)); err != nil {
		return fmt.Errorf("set peer wire visibility: %w", err)
	}
	return nil
}

// PeerWireVisibilities returns every recorded wire visibility by peer id.
func (s *Store) PeerWireVisibilities(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer_id, mode FROM peer_wire_visibility`)
	if err != nil {
		return nil, fmt.Errorf("list peer wire visibility: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, mode string
		if err := rows.Scan(&id, &mode); err != nil {
			return nil, err
		}
		out[id] = mode
	}
	return out, rows.Err()
}

// peerWireVisibilityRetention: a peer's visibility is its live choice, not
// history, and goes when the peer is closed for good.
var peerWireVisibilityRetention = tableRetention{tables: []string{"peer_wire_visibility"}, exempt: "a peer's wire visibility is live configuration", closedWithPeer: true}
