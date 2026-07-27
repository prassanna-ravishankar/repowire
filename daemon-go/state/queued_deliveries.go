package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// DeliveryKind is "ask" or "notify" — matches the Python Literal.
type DeliveryKind string

const (
	DeliveryAsk    DeliveryKind = "ask"
	DeliveryNotify DeliveryKind = "notify"
)

// QueuedDelivery mirrors the Python QueuedDelivery dataclass row-for-row. It is
// the durable, capped, delete-on-drain delivery queue entry for polling peers.
type QueuedDelivery struct {
	DeliveryID        string
	PeerID            string
	RepowireSessionID *string
	Kind              DeliveryKind
	FromPeerID        *string
	FromPeerName      string
	ToPeerName        string
	CorrelationID     *string
	Text              string
	// CreatedAt and ExpiresAt are stored verbatim as the ISO-8601 strings the
	// Python store writes; string comparison on them must remain lexicographic.
	CreatedAt   string
	ExpiresAt   string
	Attachments []map[string]any
	Metadata    map[string]any
}

func newDeliveryID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "qd-" + hex.EncodeToString(b[:]), nil
}

// EnqueueDelivery appends one delivery, evicting expired rows first and
// enforcing the per-peer cap afterwards. It returns nil (no error) when the
// store is disabled (cap or ttl <= 0), matching the Python early-return.
//
// ttlSeconds and maxPerPeer are passed per call (the Python store holds them as
// instance config); the daemon supplies its configured values.
func (s *Store) EnqueueDelivery(
	ctx context.Context,
	d QueuedDelivery,
	ttlSeconds float64,
	maxPerPeer int,
	now time.Time,
) (*QueuedDelivery, error) {
	if maxPerPeer <= 0 || ttlSeconds <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	id, err := newDeliveryID()
	if err != nil {
		return nil, fmt.Errorf("mint delivery id: %w", err)
	}

	out := d
	out.DeliveryID = id
	out.CreatedAt = formatISO(now)
	out.ExpiresAt = formatISO(now.Add(time.Duration(ttlSeconds * float64(time.Second))))
	if out.Attachments == nil {
		out.Attachments = []map[string]any{}
	}
	if out.Metadata == nil {
		out.Metadata = map[string]any{}
	}

	attachmentsJSON, err := json.Marshal(out.Attachments)
	if err != nil {
		return nil, fmt.Errorf("marshal attachments: %w", err)
	}
	metadataJSON, err := json.Marshal(out.Metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteExpiredTx(ctx, tx, out.CreatedAt); err != nil {
		return nil, err
	}

	const insert = `INSERT INTO queued_deliveries(
		delivery_id, peer_id, repowire_session_id, kind, from_peer_id,
		from_peer_name, to_peer_name, correlation_id, text,
		attachments_json, metadata_json, created_at, expires_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, insert,
		out.DeliveryID,
		out.PeerID,
		strOrNil(out.RepowireSessionID),
		string(out.Kind),
		strOrNil(out.FromPeerID),
		out.FromPeerName,
		out.ToPeerName,
		strOrNil(out.CorrelationID),
		out.Text,
		string(attachmentsJSON),
		string(metadataJSON),
		out.CreatedAt,
		out.ExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("insert queued delivery: %w", err)
	}

	if err := enforceCapTx(ctx, tx, out.PeerID, maxPerPeer); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit enqueue: %w", err)
	}
	return &out, nil
}

// DrainDeliveries returns and deletes unexpired deliveries for one peer, oldest
// first. This is the flush-on-connect query: peers drain their queue when they
// reconnect. Returns at most maxResults rows.
func (s *Store) DrainDeliveries(
	ctx context.Context,
	peerID string,
	maxResults int,
	now time.Time,
) ([]QueuedDelivery, error) {
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := formatISO(now.UTC())
	limit := maxResults
	if limit <= 0 {
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin drain tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteExpiredTx(ctx, tx, cutoff); err != nil {
		return nil, err
	}

	out, err := selectForPeerTx(ctx, tx, peerID, cutoff, limit)
	if err != nil {
		return nil, err
	}

	for _, d := range out {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM queued_deliveries WHERE delivery_id = ?", d.DeliveryID,
		); err != nil {
			return nil, fmt.Errorf("delete drained delivery %s: %w", d.DeliveryID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit drain: %w", err)
	}
	return out, nil
}

// ListDeliveries returns unexpired deliveries for one peer without deleting
// them (it still evicts expired rows as a side effect, matching Python).
func (s *Store) ListDeliveries(
	ctx context.Context,
	peerID string,
	maxResults int,
	now time.Time,
) ([]QueuedDelivery, error) {
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := formatISO(now.UTC())
	limit := maxResults
	if limit <= 0 {
		return nil, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin list tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteExpiredTx(ctx, tx, cutoff); err != nil {
		return nil, err
	}

	out, err := selectForPeerTx(ctx, tx, peerID, cutoff, limit)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list: %w", err)
	}
	return out, nil
}

// DeleteDelivery removes one queued delivery by id, returning whether a row was
// deleted (confirmed-handoff path).
func (s *Store) DeleteDelivery(ctx context.Context, deliveryID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM queued_deliveries WHERE delivery_id = ?", deliveryID,
	)
	if err != nil {
		return false, fmt.Errorf("delete delivery %s: %w", deliveryID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// CountDeliveries returns the raw row count for one peer (no expiry filter,
// matching the Python count_for_peer).
func (s *Store) CountDeliveries(ctx context.Context, peerID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM queued_deliveries WHERE peer_id = ?", peerID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count deliveries: %w", err)
	}
	return n, nil
}

func deleteExpiredTx(ctx context.Context, tx *sql.Tx, cutoffISO string) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM queued_deliveries WHERE expires_at <= ?", cutoffISO,
	); err != nil {
		return fmt.Errorf("delete expired deliveries: %w", err)
	}
	return nil
}

// enforceCapTx deletes the oldest rows beyond maxPerPeer for one peer. The
// Python keeps the newest `max` (ORDER BY created_at DESC ... OFFSET max).
func enforceCapTx(ctx context.Context, tx *sql.Tx, peerID string, maxPerPeer int) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT delivery_id FROM queued_deliveries
		WHERE peer_id = ?
		ORDER BY created_at DESC, delivery_id DESC
		LIMIT -1 OFFSET ?
	`, peerID, maxPerPeer)
	if err != nil {
		return fmt.Errorf("select over-cap deliveries: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan over-cap id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate over-cap ids: %w", err)
	}
	_ = rows.Close()

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM queued_deliveries WHERE delivery_id = ?", id,
		); err != nil {
			return fmt.Errorf("delete over-cap delivery %s: %w", id, err)
		}
	}
	return nil
}

func selectForPeerTx(ctx context.Context, tx *sql.Tx, peerID, cutoffISO string, limit int) ([]QueuedDelivery, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT delivery_id, peer_id, repowire_session_id, kind, from_peer_id,
			from_peer_name, to_peer_name, correlation_id, text,
			attachments_json, metadata_json, created_at, expires_at
		FROM queued_deliveries
		WHERE peer_id = ? AND expires_at > ?
		ORDER BY created_at ASC, delivery_id ASC
		LIMIT ?
	`, peerID, cutoffISO, limit)
	if err != nil {
		return nil, fmt.Errorf("select deliveries for peer: %w", err)
	}
	defer rows.Close()

	var out []QueuedDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliveries: %w", err)
	}
	return out, nil
}

func scanDelivery(rows *sql.Rows) (QueuedDelivery, error) {
	var (
		d               QueuedDelivery
		sessionID       sql.NullString
		fromPeerID      sql.NullString
		correlationID   sql.NullString
		kind            string
		attachmentsJSON sql.NullString
		metadataJSON    sql.NullString
	)
	if err := rows.Scan(
		&d.DeliveryID,
		&d.PeerID,
		&sessionID,
		&kind,
		&fromPeerID,
		&d.FromPeerName,
		&d.ToPeerName,
		&correlationID,
		&d.Text,
		&attachmentsJSON,
		&metadataJSON,
		&d.CreatedAt,
		&d.ExpiresAt,
	); err != nil {
		return d, fmt.Errorf("scan delivery: %w", err)
	}
	d.Kind = DeliveryKind(kind)
	d.RepowireSessionID = nullStringPtr(sessionID)
	d.FromPeerID = nullStringPtr(fromPeerID)
	d.CorrelationID = nullStringPtr(correlationID)

	d.Attachments = []map[string]any{}
	if attachmentsJSON.Valid && attachmentsJSON.String != "" {
		var parsed []map[string]any
		if err := json.Unmarshal([]byte(attachmentsJSON.String), &parsed); err == nil && parsed != nil {
			d.Attachments = parsed
		}
	}
	d.Metadata = map[string]any{}
	if metadataJSON.Valid && metadataJSON.String != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(metadataJSON.String), &parsed); err == nil && parsed != nil {
			d.Metadata = parsed
		}
	}
	return d, nil
}
