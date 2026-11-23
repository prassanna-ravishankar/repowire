package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Peer message kinds (rfcs/message-ids-pull-delegate.md).
const (
	MessageNotify = "notify"
	MessageAsk    = "ask"
	MessageReply  = "reply"
)

// PeerMessage is one message delivered to one agent peer: the durable record
// behind the message_id its frame carries. The human's messages live in
// inbox_items instead.
type PeerMessage struct {
	ID            string
	Kind          string // notify | ask | reply
	PeerID        string // recipient
	PeerName      string
	FromPeerID    string
	FromName      string
	CorrelationID string
	TraceID       string // the notify's delivery_id, or the ask's correlation_id
	Text          string
	Attachments   []map[string]any
	Question      map[string]any
	CreatedAt     string
	ReadAt        string
}

// PeerMessageFilter narrows ListPeerMessages to one recipient. Limit <= 0
// means 50.
type PeerMessageFilter struct {
	PeerID     string
	UnreadOnly bool
	Limit      int
}

const peerMessageColumns = `id, kind, peer_id, peer_name, from_peer_id, from_name, correlation_id,
	trace_id, text, attachments_json, question_json, created_at, read_at`

// NewPeerMessageID mints a message id ("msg-" + 8 hex).
func NewPeerMessageID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "msg-" + hex.EncodeToString(b[:])
}

// InsertPeerMessage stores msg, minting its id and created_at when empty.
func (s *Store) InsertPeerMessage(ctx context.Context, msg *PeerMessage) error {
	if msg.ID == "" {
		msg.ID = NewPeerMessageID()
	}
	if msg.CreatedAt == "" {
		msg.CreatedAt = time.Now().UTC().Format(tsLayout)
	}
	attachments := msg.Attachments
	if attachments == nil {
		attachments = []map[string]any{}
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return fmt.Errorf("marshal message attachments: %w", err)
	}
	var questionJSON any
	if msg.Question != nil {
		b, err := json.Marshal(msg.Question)
		if err != nil {
			return fmt.Errorf("marshal message question: %w", err)
		}
		questionJSON = string(b)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO peer_messages(`+peerMessageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		msg.ID, msg.Kind, msg.PeerID, msg.PeerName, nullString(msg.FromPeerID), msg.FromName,
		nullString(msg.CorrelationID), msg.TraceID, msg.Text, string(attachmentsJSON), questionJSON, msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert peer message: %w", err)
	}
	return nil
}

// PeerMessage loads one message; (nil, nil) when unknown.
func (s *Store) PeerMessage(ctx context.Context, id string) (*PeerMessage, error) {
	msg, err := scanPeerMessage(s.db.QueryRowContext(ctx, `SELECT `+peerMessageColumns+` FROM peer_messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return msg, err
}

// ListPeerMessages returns one recipient's messages, newest first.
func (s *Store) ListPeerMessages(ctx context.Context, filter PeerMessageFilter) ([]PeerMessage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	where := `peer_id = ?`
	if filter.UnreadOnly {
		where += ` AND read_at IS NULL`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+peerMessageColumns+` FROM peer_messages WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, filter.PeerID, limit)
	if err != nil {
		return nil, fmt.Errorf("list peer messages: %w", err)
	}
	defer rows.Close()
	var out []PeerMessage
	for rows.Next() {
		msg, err := scanPeerMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *msg)
	}
	return out, rows.Err()
}

// MarkPeerMessageRead sets read_at once; changed is false when it was already
// read or the message is unknown.
func (s *Store) MarkPeerMessageRead(ctx context.Context, id string) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE peer_messages SET read_at = ? WHERE id = ? AND read_at IS NULL`,
		time.Now().UTC().Format(tsLayout), id)
	if err != nil {
		return false, fmt.Errorf("mark peer message read: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeletePeerMessage drops one message (a send that never left).
func (s *Store) DeletePeerMessage(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM peer_messages WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete peer message: %w", err)
	}
	return nil
}

func scanPeerMessage(row rowScanner) (*PeerMessage, error) {
	var msg PeerMessage
	var fromID, cid, question, readAt sql.NullString
	var attachments string
	if err := row.Scan(&msg.ID, &msg.Kind, &msg.PeerID, &msg.PeerName, &fromID, &msg.FromName, &cid,
		&msg.TraceID, &msg.Text, &attachments, &question, &msg.CreatedAt, &readAt); err != nil {
		return nil, err
	}
	msg.FromPeerID, msg.CorrelationID, msg.ReadAt = fromID.String, cid.String, readAt.String
	if err := json.Unmarshal([]byte(attachments), &msg.Attachments); err != nil {
		return nil, fmt.Errorf("decode message attachments: %w", err)
	}
	if question.Valid && question.String != "" {
		if err := json.Unmarshal([]byte(question.String), &msg.Question); err != nil {
			return nil, fmt.Errorf("decode message question: %w", err)
		}
	}
	return &msg, nil
}

// SetPeerMessageDelivery records peerID's delivery mode.
func (s *Store) SetPeerMessageDelivery(ctx context.Context, peerID, mode string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO peer_message_delivery(peer_id, mode, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
		peerID, mode, time.Now().UTC().Format(tsLayout)); err != nil {
		return fmt.Errorf("set peer message delivery: %w", err)
	}
	return nil
}

// PeerMessageDeliveries returns every recorded delivery mode by peer id.
func (s *Store) PeerMessageDeliveries(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer_id, mode FROM peer_message_delivery`)
	if err != nil {
		return nil, fmt.Errorf("list peer message delivery: %w", err)
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
