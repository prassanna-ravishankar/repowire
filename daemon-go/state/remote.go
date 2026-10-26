package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// Durable records for cross-machine asks and notifies
// (rfcs/cross-machine-circles.md). The target machine keeps remote_inbound
// and remote_fences; the source keeps remote_outbound. Every state change is
// a compare-and-set, and settled is terminal.

// Remote record states.
const (
	RemoteDispatching = "dispatching"
	RemoteOpen        = "open"
	RemoteDelivered   = "delivered"
	RemoteSettled     = "settled"
	RemoteSent        = "sent"
	RemoteUncertain   = "uncertain"
)

var (
	// ErrRemoteConflict is a known id reused with a different envelope.
	ErrRemoteConflict = errors.New("remote id reused with a different request")
	// ErrRemoteNotFound is an id this origin may not read.
	ErrRemoteNotFound = errors.New("remote request not found")
	// ErrRemoteExpired is an unknown id past the create horizon: its record
	// may have existed and been deleted, so nothing can be said about it.
	ErrRemoteExpired = errors.New("remote request expired")
)

// RemotePrincipal is a peer as its own machine vouches for it.
type RemotePrincipal struct {
	PeerID      string `json:"peer_id"`
	DisplayName string `json:"display_name"`
	Circle      string `json:"circle"`
}

// RemoteInbound is a request another machine sent here.
type RemoteInbound struct {
	ID          string
	Kind        string // ask | notify
	FromMachine string
	From        RemotePrincipal
	ToPeerID    string
	Text        string
	CreatedAt   int64
	State       string
	CloseReason string
	Reply       *string
	SettledAt   *int64
}

func (r RemoteInbound) sameEnvelope(o RemoteInbound) bool {
	return r.Kind == o.Kind && r.FromMachine == o.FromMachine && r.From == o.From && r.ToPeerID == o.ToPeerID && r.Text == o.Text
}

// StoreEpoch names this database. It is created once and never changes, so a
// replaced database is told apart from one that lost nothing.
func (s *Store) StoreEpoch(ctx context.Context) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO daemon_meta(key, value) VALUES ('store_epoch', ?)`, hex.EncodeToString(b[:])); err != nil {
		return "", err
	}
	var epoch string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM daemon_meta WHERE key = 'store_epoch'`).Scan(&epoch)
	return epoch, err
}

const inboundColumns = `id, kind, from_machine, from_peer_id, from_name, from_circle, to_peer_id, text, created_at, state, close_reason, reply, settled_at`

func scanInbound(row rowScanner) (RemoteInbound, error) {
	var r RemoteInbound
	var reply sql.NullString
	var settled sql.NullInt64
	err := row.Scan(&r.ID, &r.Kind, &r.FromMachine, &r.From.PeerID, &r.From.DisplayName, &r.From.Circle, &r.ToPeerID, &r.Text, &r.CreatedAt, &r.State, &r.CloseReason, &reply, &settled)
	if reply.Valid {
		r.Reply = &reply.String
	}
	if settled.Valid {
		r.SettledAt = &settled.Int64
	}
	return r, err
}

// fenceReceipt is what a fenced id reads as: settled, never delivered.
func fenceReceipt(id, fromMachine, fromPeer string, createdAt int64) RemoteInbound {
	return RemoteInbound{ID: id, FromMachine: fromMachine, From: RemotePrincipal{PeerID: fromPeer}, CreatedAt: createdAt, State: RemoteSettled, CloseReason: "not_delivered"}
}

// CreateInbound records a request before it is dispatched. One transaction
// reads the fence and the record and inserts at most one row, so a create and
// a fencing read of the same id can never both win. created is false when the
// id was already known (the stored receipt is returned and nothing may be
// dispatched); a different envelope under a known id is ErrRemoteConflict.
func (s *Store) CreateInbound(ctx context.Context, r RemoteInbound) (RemoteInbound, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RemoteInbound{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var fencedBy string
	var fencedAt int64
	switch err := tx.QueryRowContext(ctx, `SELECT from_peer_id, created_at FROM remote_fences WHERE id = ? AND from_machine = ?`, r.ID, r.FromMachine).Scan(&fencedBy, &fencedAt); {
	case err == nil:
		return fenceReceipt(r.ID, r.FromMachine, fencedBy, fencedAt), false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return RemoteInbound{}, false, err
	}
	existing, err := scanInbound(tx.QueryRowContext(ctx, `SELECT `+inboundColumns+` FROM remote_inbound WHERE id = ?`, r.ID))
	switch {
	case err == nil:
		if !existing.sameEnvelope(r) {
			return RemoteInbound{}, false, ErrRemoteConflict
		}
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return RemoteInbound{}, false, err
	}
	r.State = RemoteDispatching
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_inbound(`+inboundColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', NULL, NULL)`,
		r.ID, r.Kind, r.FromMachine, r.From.PeerID, r.From.DisplayName, r.From.Circle, r.ToPeerID, r.Text, r.CreatedAt, r.State); err != nil {
		return RemoteInbound{}, false, err
	}
	return r, true, tx.Commit()
}

// ReadInbound is the recorded origin reading its request. A row belonging to
// another origin is ErrRemoteNotFound. An unknown id created after horizon is
// fenced in the same transaction as the read, so a create arriving later
// finds the fence and dispatches nothing; one created before horizon is
// ErrRemoteExpired, since its record may simply have been pruned.
func (s *Store) ReadInbound(ctx context.Context, id, fromMachine, fromPeer string, createdAt, horizon int64) (RemoteInbound, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RemoteInbound{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := scanInbound(tx.QueryRowContext(ctx, `SELECT `+inboundColumns+` FROM remote_inbound WHERE id = ?`, id))
	if err == nil {
		if r.FromMachine != fromMachine || r.From.PeerID != fromPeer {
			return RemoteInbound{}, ErrRemoteNotFound
		}
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return RemoteInbound{}, err
	}
	var fencedBy string
	var fencedAt int64
	switch err := tx.QueryRowContext(ctx, `SELECT from_peer_id, created_at FROM remote_fences WHERE id = ? AND from_machine = ?`, id, fromMachine).Scan(&fencedBy, &fencedAt); {
	case err == nil:
		if fencedBy != fromPeer {
			return RemoteInbound{}, ErrRemoteNotFound
		}
		return fenceReceipt(id, fromMachine, fencedBy, fencedAt), nil
	case !errors.Is(err, sql.ErrNoRows):
		return RemoteInbound{}, err
	}
	if createdAt < horizon {
		return RemoteInbound{}, ErrRemoteExpired
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_fences(id, from_machine, from_peer_id, created_at) VALUES (?, ?, ?, ?)`, id, fromMachine, fromPeer, createdAt); err != nil {
		return RemoteInbound{}, err
	}
	return fenceReceipt(id, fromMachine, fromPeer, createdAt), tx.Commit()
}

// GetInbound returns one record by id.
func (s *Store) GetInbound(ctx context.Context, id string) (RemoteInbound, error) {
	r, err := scanInbound(s.db.QueryRowContext(ctx, `SELECT `+inboundColumns+` FROM remote_inbound WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteInbound{}, ErrRemoteNotFound
	}
	return r, err
}

// AdvanceInbound moves a record from one nonterminal state to another.
func (s *Store) AdvanceInbound(ctx context.Context, id, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE remote_inbound SET state = ? WHERE id = ? AND state = ?`, to, id, from)
	return changed(res, err)
}

// SettleInbound writes a record's result. The first settlement wins; a later
// one reports false and changes nothing.
func (s *Store) SettleInbound(ctx context.Context, id, closeReason string, reply *string, now int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE remote_inbound SET state = 'settled', close_reason = ?, reply = ?, settled_at = ? WHERE id = ? AND state != 'settled'`, closeReason, reply, now, id)
	return changed(res, err)
}

// InboundInStates lists records in any of the given states, oldest first.
func (s *Store) InboundInStates(ctx context.Context, states ...string) ([]RemoteInbound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+inboundColumns+` FROM remote_inbound WHERE state IN (`+placeholders(len(states))+`) ORDER BY created_at`, anySlice(states)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteInbound
	for rows.Next() {
		r, err := scanInbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemoteOutbound is a request this machine sent to another.
type RemoteOutbound struct {
	ID          string
	Machine     string
	StoreEpoch  string
	ToPeerID    string
	ToName      string
	From        RemotePrincipal
	Text        string
	CreatedAt   int64
	State       string
	CloseReason string
	Reply       *string
	Delivered   bool
	CheckedAt   int64
}

const outboundColumns = `id, machine, store_epoch, to_peer_id, to_name, from_peer_id, from_name, from_circle, text, created_at, state, close_reason, reply, delivered, checked_at`

func scanOutbound(row rowScanner) (RemoteOutbound, error) {
	var r RemoteOutbound
	var reply sql.NullString
	err := row.Scan(&r.ID, &r.Machine, &r.StoreEpoch, &r.ToPeerID, &r.ToName, &r.From.PeerID, &r.From.DisplayName, &r.From.Circle, &r.Text, &r.CreatedAt, &r.State, &r.CloseReason, &reply, &r.Delivered, &r.CheckedAt)
	if reply.Valid {
		r.Reply = &reply.String
	}
	return r, err
}

// InsertOutbound commits a request's immutable envelope before it is sent.
func (s *Store) InsertOutbound(ctx context.Context, r RemoteOutbound) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO remote_outbound(`+outboundColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', NULL, 0, 0)`,
		r.ID, r.Machine, r.StoreEpoch, r.ToPeerID, r.ToName, r.From.PeerID, r.From.DisplayName, r.From.Circle, r.Text, r.CreatedAt, RemoteSent)
	return err
}

// GetOutbound returns one record by id.
func (s *Store) GetOutbound(ctx context.Context, id string) (RemoteOutbound, error) {
	r, err := scanOutbound(s.db.QueryRowContext(ctx, `SELECT `+outboundColumns+` FROM remote_outbound WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteOutbound{}, ErrRemoteNotFound
	}
	return r, err
}

// MarkOutboundUncertain records that a send's outcome is unknown. Only a
// still-sent record changes: a result that already arrived is kept.
func (s *Store) MarkOutboundUncertain(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE remote_outbound SET state = 'uncertain' WHERE id = ? AND state = 'sent'`, id)
	return changed(res, err)
}

// SettleOutbound writes a request's result. Push, read and create response
// all come through here; the first wins.
func (s *Store) SettleOutbound(ctx context.Context, id, closeReason string, reply *string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE remote_outbound SET state = 'settled', close_reason = ?, reply = ? WHERE id = ? AND state IN ('sent', 'uncertain')`, closeReason, reply, id)
	return changed(res, err)
}

// MarkOutboundDelivered records that the reply reached the asker.
func (s *Store) MarkOutboundDelivered(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE remote_outbound SET delivered = 1 WHERE id = ?`, id)
	return err
}

// ClaimOutboundChecks picks up to limit of an asker's unsettled records not
// checked since checkedBefore, oldest check first, and stamps them now.
func (s *Store) ClaimOutboundChecks(ctx context.Context, asker string, checkedBefore, now int64, limit int) ([]RemoteOutbound, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+outboundColumns+` FROM remote_outbound WHERE from_peer_id = ? AND state IN ('sent', 'uncertain') AND checked_at < ? ORDER BY checked_at, created_at LIMIT ?`, asker, checkedBefore, limit)
	if err != nil {
		return nil, err
	}
	out, err := collectOutbound(rows)
	if err != nil {
		return nil, err
	}
	for _, r := range out {
		if _, err := tx.ExecContext(ctx, `UPDATE remote_outbound SET checked_at = ? WHERE id = ?`, now, r.ID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// UndeliveredOutbound lists settled records whose reply has not reached the
// asker; asker "" means every asker.
func (s *Store) UndeliveredOutbound(ctx context.Context, asker string, limit int) ([]RemoteOutbound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboundColumns+` FROM remote_outbound WHERE state = 'settled' AND delivered = 0 AND (? = '' OR from_peer_id = ?) ORDER BY created_at LIMIT ?`, asker, asker, limit)
	if err != nil {
		return nil, err
	}
	return collectOutbound(rows)
}

// UnsettledOutbound lists every unsettled record, for startup recovery.
func (s *Store) UnsettledOutbound(ctx context.Context) ([]RemoteOutbound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboundColumns+` FROM remote_outbound WHERE state IN ('sent', 'uncertain') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return collectOutbound(rows)
}

// PruneRemote deletes every cross-machine record created before cutoff.
func (s *Store) PruneRemote(ctx context.Context, cutoff int64) error {
	for _, table := range []string{"remote_inbound", "remote_fences", "remote_outbound"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE created_at < ?`, cutoff); err != nil {
			return fmt.Errorf("prune %s: %w", table, err)
		}
	}
	return nil
}

func collectOutbound(rows *sql.Rows) ([]RemoteOutbound, error) {
	defer rows.Close()
	var out []RemoteOutbound
	for rows.Next() {
		r, err := scanOutbound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func changed(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func anySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
