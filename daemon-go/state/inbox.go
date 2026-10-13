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

// Inbox directions: "in" items were addressed to the human; "out" items record
// what the human sent from a surface, so replies can thread against them.
const (
	InboxIn  = "in"
	InboxOut = "out"
)

// Inbox push statuses, one per (item, surface).
const (
	PushPushed = "pushed"
	PushFailed = "failed"
)

// InboxItem is one message in the human inbox. Text lives here and only here:
// trace rows that mention the item carry its id, never its body.
type InboxItem struct {
	ID            string
	Direction     string
	Kind          string // notify | ask | answer | broadcast
	TraceID       string // delivery_traces trace this item's stages join
	TraceKind     string // delivery_traces kind for those stages
	FromPeerID    string
	FromName      string
	ToPeerID      string
	ToName        string
	CorrelationID string
	Priority      int
	Text          string
	Attachments   []map[string]any
	Question      map[string]any
	CreatedAt     string
	ReadAt        string
	ReadSurface   string
	RepliedAt     string
	ReplySurface  string
	Pushes        []InboxPush
}

// InboxPush is the outcome of pushing one item to one surface. SurfaceRef is
// the first message the surface rendered (the thread anchor); Refs is every
// message it rendered, so a reply to any of them finds the item.
type InboxPush struct {
	Surface    string
	Status     string
	ErrorClass string
	SurfaceRef string
	Refs       []string
	TS         string
}

// InboxFilter narrows ListInbox. Limit <= 0 means 50.
type InboxFilter struct {
	UnreadOnly bool
	Limit      int
}

const inboxColumns = `id, direction, kind, trace_id, trace_kind, from_peer_id, from_name,
	to_peer_id, to_name, correlation_id, priority, text, attachments_json, question_json,
	created_at, read_at, read_surface, replied_at, reply_surface`

// NewInboxItemID mints an inbox item id ("inb-" + 8 hex).
func NewInboxItemID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "inb-" + hex.EncodeToString(b[:])
}

// InsertInboxItem stores item, minting its id and created_at when empty.
func (s *Store) InsertInboxItem(ctx context.Context, item *InboxItem) error {
	if item.ID == "" {
		item.ID = NewInboxItemID()
	}
	if item.CreatedAt == "" {
		item.CreatedAt = time.Now().UTC().Format(tsLayout)
	}
	if item.Direction == "" {
		item.Direction = InboxIn
	}
	attachments := item.Attachments
	if attachments == nil {
		attachments = []map[string]any{}
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return fmt.Errorf("marshal inbox attachments: %w", err)
	}
	var questionJSON any
	if item.Question != nil {
		b, err := json.Marshal(item.Question)
		if err != nil {
			return fmt.Errorf("marshal inbox question: %w", err)
		}
		questionJSON = string(b)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO inbox_items(`+inboxColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL)`,
		item.ID, item.Direction, item.Kind, item.TraceID, item.TraceKind,
		nullString(item.FromPeerID), item.FromName, nullString(item.ToPeerID), item.ToName,
		nullString(item.CorrelationID), item.Priority, item.Text, string(attachmentsJSON), questionJSON,
		item.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert inbox item: %w", err)
	}
	return nil
}

// InboxItem loads one item with its push records; (nil, nil) when unknown.
func (s *Store) InboxItem(ctx context.Context, id string) (*InboxItem, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM inbox_items WHERE id = ?`, id)
	item, err := scanInboxItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pushes, err := s.inboxPushes(ctx, []string{item.ID})
	if err != nil {
		return nil, err
	}
	item.Pushes = pushes[item.ID]
	return item, nil
}

// ListInbox returns inbound items, newest first, with their push records.
func (s *Store) ListInbox(ctx context.Context, filter InboxFilter) ([]InboxItem, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	where := `direction = 'in'`
	if filter.UnreadOnly {
		where += ` AND read_at IS NULL`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+inboxColumns+` FROM inbox_items WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}
	var items []InboxItem
	var ids []string
	for rows.Next() {
		item, err := scanInboxItem(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, *item)
		ids = append(ids, item.ID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	pushes, err := s.inboxPushes(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Pushes = pushes[items[i].ID]
	}
	return items, nil
}

// RecordInboxPush upserts the outcome of pushing item to surface and maps
// each rendered ref back to the item.
func (s *Store) RecordInboxPush(ctx context.Context, itemID string, push InboxPush) error {
	if push.TS == "" {
		push.TS = time.Now().UTC().Format(tsLayout)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inbox push tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_pushes(item_id, surface, status, error_class, surface_ref, ts)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_id, surface) DO UPDATE SET
			status = excluded.status, error_class = excluded.error_class,
			surface_ref = excluded.surface_ref, ts = excluded.ts`,
		itemID, push.Surface, push.Status, nullString(push.ErrorClass), nullString(push.SurfaceRef), push.TS); err != nil {
		return fmt.Errorf("record inbox push: %w", err)
	}
	for _, ref := range append([]string{push.SurfaceRef}, push.Refs...) {
		if ref == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO inbox_refs(surface, ref, item_id) VALUES (?, ?, ?)`,
			push.Surface, ref, itemID); err != nil {
			return fmt.Errorf("record inbox ref: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit inbox push: %w", err)
	}
	return nil
}

// InboxItemBySurfaceRef finds the item a surface rendered as ref (for a
// Telegram reply-to, the message_id of any chunk). (nil, nil) when unknown.
func (s *Store) InboxItemBySurfaceRef(ctx context.Context, surface, ref string) (*InboxItem, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT item_id FROM inbox_refs WHERE surface = ? AND ref = ?`,
		surface, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inbox item by surface ref: %w", err)
	}
	return s.InboxItem(ctx, id)
}

// InboxThreadRef returns the surface_ref of the earliest item on correlation
// id cid that surface rendered, so a later item on the same thread can reply
// to it. "" when the thread has not been rendered there.
func (s *Store) InboxThreadRef(ctx context.Context, cid, surface string) (string, error) {
	if cid == "" {
		return "", nil
	}
	var ref sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT p.surface_ref FROM inbox_items i
		JOIN inbox_pushes p ON p.item_id = i.id
		WHERE i.correlation_id = ? AND p.surface = ? AND p.status = 'pushed' AND p.surface_ref IS NOT NULL
		ORDER BY i.created_at ASC LIMIT 1`, cid, surface).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inbox thread ref: %w", err)
	}
	return ref.String, nil
}

// MarkInboxRead sets read_at once; changed is false when it was already read.
func (s *Store) MarkInboxRead(ctx context.Context, id, surface string) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_items SET read_at = ?, read_surface = ?
		WHERE id = ? AND read_at IS NULL`, time.Now().UTC().Format(tsLayout), surface, id)
	if err != nil {
		return false, fmt.Errorf("mark inbox read: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkInboxReplied sets replied_at once (and read_at if still unread);
// changed is false when it was already replied.
func (s *Store) MarkInboxReplied(ctx context.Context, id, surface string) (changed bool, err error) {
	now := time.Now().UTC().Format(tsLayout)
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_items SET replied_at = ?, reply_surface = ?,
		read_at = COALESCE(read_at, ?), read_surface = COALESCE(read_surface, ?)
		WHERE id = ? AND replied_at IS NULL`, now, surface, now, surface, id)
	if err != nil {
		return false, fmt.Errorf("mark inbox replied: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) inboxPushes(ctx context.Context, ids []string) (map[string][]InboxPush, error) {
	out := map[string][]InboxPush{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, surface, status, error_class, surface_ref, ts
		FROM inbox_pushes WHERE item_id IN (`+placeholders(len(ids))+`) ORDER BY ts`, args...)
	if err != nil {
		return nil, fmt.Errorf("inbox pushes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var itemID string
		var p InboxPush
		var errorClass, ref sql.NullString
		if err := rows.Scan(&itemID, &p.Surface, &p.Status, &errorClass, &ref, &p.TS); err != nil {
			return nil, err
		}
		p.ErrorClass, p.SurfaceRef = errorClass.String, ref.String
		out[itemID] = append(out[itemID], p)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInboxItem(row rowScanner) (*InboxItem, error) {
	var item InboxItem
	var fromID, toID, cid, question, readAt, readSurface, repliedAt, replySurface sql.NullString
	var attachments string
	if err := row.Scan(&item.ID, &item.Direction, &item.Kind, &item.TraceID, &item.TraceKind,
		&fromID, &item.FromName, &toID, &item.ToName, &cid, &item.Priority, &item.Text,
		&attachments, &question, &item.CreatedAt, &readAt, &readSurface, &repliedAt, &replySurface); err != nil {
		return nil, err
	}
	item.FromPeerID, item.ToPeerID, item.CorrelationID = fromID.String, toID.String, cid.String
	item.ReadAt, item.ReadSurface = readAt.String, readSurface.String
	item.RepliedAt, item.ReplySurface = repliedAt.String, replySurface.String
	if err := json.Unmarshal([]byte(attachments), &item.Attachments); err != nil {
		return nil, fmt.Errorf("decode inbox attachments: %w", err)
	}
	if question.Valid && question.String != "" {
		if err := json.Unmarshal([]byte(question.String), &item.Question); err != nil {
			return nil, fmt.Errorf("decode inbox question: %w", err)
		}
	}
	return &item, nil
}
