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

	// A caller that already traces the delivery keeps its id, so the queue
	// and the trace ledger name the same delivery.
	id := d.DeliveryID
	if id == "" {
		var err error
		if id, err = newDeliveryID(); err != nil {
			return nil, fmt.Errorf("mint delivery id: %w", err)
		}
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
		if _, err := removeQueuedTx(ctx, tx, QueueDequeued, "", "delivery_id = ?", d.DeliveryID); err != nil {
			return nil, fmt.Errorf("drain delivery %s: %w", d.DeliveryID, err)
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

// DeleteDelivery removes one queued delivery by id once it has been handed to
// the peer (the replayed frame is on the wire), returning whether a row was
// deleted. A traced notify records it as dequeued.
func (s *Store) DeleteDelivery(ctx context.Context, deliveryID string) (bool, error) {
	n, err := s.removeQueued(ctx, QueueDequeued, "", "delivery_id = ?", deliveryID)
	if err != nil {
		return false, fmt.Errorf("delete delivery %s: %w", deliveryID, err)
	}
	return n > 0, nil
}

func deleteExpiredTx(ctx context.Context, tx *sql.Tx, cutoffISO string) error {
	if _, err := removeQueuedTx(ctx, tx, QueueExpired, "", "expires_at <= ?", cutoffISO); err != nil {
		return fmt.Errorf("delete expired deliveries: %w", err)
	}
	return nil
}

// How a queued delivery left the queue. For a traced notify the queue
// records it as a stage under the notify's delivery id, so its trace says
// what happened (the wire view reads it).
const (
	QueueDequeued = "dequeued" // handed to the reconnected peer (replayed or drained); not a receipt
	QueueExpired  = "expired"  // its TTL passed first
	QueueDropped  = "dropped"  // removed undelivered: the per-peer cap, or a peer that cannot take input
)

// removeQueued is removeQueuedTx in its own transaction.
func (s *Store) removeQueued(ctx context.Context, outcome, reason, where string, args ...any) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin queue removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := removeQueuedTx(ctx, tx, outcome, reason, where, args...)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// removeQueuedTx deletes the queue rows where selects and, for each traced
// notify among them, records outcome as a stage of its trace.
func removeQueuedTx(ctx context.Context, tx *sql.Tx, outcome, reason, where string, args ...any) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT delivery_id, peer_id, COALESCE(from_peer_id, '') FROM queued_deliveries
		WHERE kind = 'notify' AND `+where+`
		  AND EXISTS (SELECT 1 FROM delivery_traces t WHERE t.trace_id = queued_deliveries.delivery_id)`, args...)
	if err != nil {
		return 0, fmt.Errorf("select %s deliveries: %w", outcome, err)
	}
	type traced struct{ id, peer, from string }
	var notifies []traced
	for rows.Next() {
		var t traced
		if err := rows.Scan(&t.id, &t.peer, &t.from); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan %s delivery: %w", outcome, err)
		}
		notifies = append(notifies, t)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	status := "fail"
	if outcome == QueueDequeued {
		status = "ok"
	}
	var detail map[string]any
	if reason != "" {
		detail = map[string]any{"reason": reason}
	}
	for _, t := range notifies {
		if err := recordTrace(ctx, tx, t.id, "notify", outcome, status, t.id, t.peer, t.from, detail); err != nil {
			return 0, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM queued_deliveries WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("delete %s deliveries: %w", outcome, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
		if _, err := removeQueuedTx(ctx, tx, QueueDropped, "queue_cap", "delivery_id = ?", id); err != nil {
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

// DeleteDeliveriesForPeer drops every queued delivery addressed to peerID
// (a peer that can no longer take input). Returns the number removed. Each
// traced notify records it as dropped (peer_not_addressable).
func (s *Store) DeleteDeliveriesForPeer(ctx context.Context, peerID string) (int, error) {
	n, err := s.removeQueued(ctx, QueueDropped, "peer_not_addressable", "peer_id = ?", peerID)
	if err != nil {
		return 0, fmt.Errorf("delete deliveries for %s: %w", peerID, err)
	}
	return n, nil
}

var queuedDeliveryRetention = tableRetention{tables: []string{"queued_deliveries"}, exempt: "live queue with its own TTL (daemon.delivery_queue_ttl_seconds)"}

// DeliveryQueued reports whether delivery id is still waiting in the queue
// (present and not expired).
func (s *Store) DeliveryQueued(ctx context.Context, id string, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queued_deliveries WHERE delivery_id = ? AND expires_at > ?`,
		id, formatISO(now.UTC())).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("queued delivery %s: %w", id, err)
	}
	return n > 0, nil
}
