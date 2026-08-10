// Package state implements peer.Store over Repowire's schema-v12 SQLite state.
// The Go daemon owns fresh bootstrap and migrations while retaining data
// compatibility with stores created by earlier Python releases.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// Compile-time assertion: Store satisfies the peer.Store contract.
var _ peer.Store = (*Store)(nil)

// SchemaVersion is the current user_version. Migrations advance older stores;
// newer stores fail loud rather than risking corruption.
const SchemaVersion = 12

// tsLayout is the exact format the Python daemon writes (strftime %Y-%m-%dT%H:%M:%fZ).
const tsLayout = "2006-01-02T15:04:05.000Z"

// isoLayout preserves the microsecond-precision datetime.isoformat shape used
// by the Python-created work, calendar, operation, binding, and queue rows.
const isoLayout = "2006-01-02T15:04:05.000000-07:00"

// tsLayouts are accepted on read; Python writes %f-millisecond Z, but be liberal.
var tsLayouts = []string{
	tsLayout,
	"2006-01-02T15:04:05Z",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000000Z",
}

// Store is the SQLite-backed peer.Store implementation.
type Store struct {
	db *sql.DB
}

// NewStore opens the daemon state db, creating and migrating it to
// schemaVersion if needed. It owns migrations now (the Python daemon no longer
// pre-migrates): a fresh path is bootstrapped; an existing DB at the current
// version is a no-op; a future/unknown version is refused rather than
// downgraded.
func NewStore(dbPath string) (*Store, error) {
	// Create the parent dir (0700) so a fresh ~/.repowire path just works, matching
	// database.py's mkdir+chmod. Skip when dbPath has no dir component (":memory:").
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create state dir: %w", err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)",
		dbPath,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// modernc.org/sqlite + WAL: a single writer connection is the simplest
	// correct model and avoids "database is locked" under concurrency.
	db.SetMaxOpenConns(1)

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read user_version: %w", err)
	}
	// A DB stamped newer than we understand is a downgrade hazard — refuse.
	if version > SchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("state db schema version %d is newer than supported %d", version, SchemaVersion)
	}
	// version 0 (fresh) or any older version: apply the idempotent bootstrap.
	if version != SchemaVersion {
		if err := migrate(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate state db: %w", err)
		}
	}
	store := &Store{db: db}
	if dbPath != ":memory:" {
		if err := store.importLegacy(context.Background(), filepath.Dir(dbPath)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("import legacy state: %w", err)
		}
	}
	return store, nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// parseTS tries each accepted layout, returning UTC.
func parseTS(raw string) (time.Time, error) {
	for _, layout := range tsLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", raw)
}

// formatTS renders the canonical Python strftime form in UTC.
func formatTS(t time.Time) string {
	return t.UTC().Format(tsLayout)
}

func formatISO(t time.Time) string { return t.UTC().Format(isoLayout) }

func nowISO() string { return formatISO(time.Now()) }

func newID(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
}

func marshalJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func decodeJSONObject(raw string) map[string]any {
	var out map[string]any
	if raw == "" || json.Unmarshal([]byte(raw), &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func nullStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullable[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

func strOrNil(value *string) any { return nullable(value) }

// LoadMappings hydrates every peer_session_mappings row.
func (s *Store) LoadMappings(ctx context.Context) ([]*proto.SessionMapping, error) {
	const q = `SELECT session_id, display_name, circle, backend, path, role, updated_at, description, model, agent_pid FROM peer_session_mappings`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("load mappings: %w", err)
	}
	defer rows.Close()

	var out []*proto.SessionMapping
	for rows.Next() {
		var (
			sessionID   string
			displayName string
			circle      string
			backend     string
			path        sql.NullString
			role        string
			updatedAt   sql.NullString
			description string
			model       sql.NullString
			agentPID    sql.NullInt64
		)
		if err := rows.Scan(&sessionID, &displayName, &circle, &backend, &path, &role, &updatedAt, &description, &model, &agentPID); err != nil {
			return nil, fmt.Errorf("scan mapping: %w", err)
		}
		m := &proto.SessionMapping{
			SessionID:   proto.PeerID(sessionID),
			DisplayName: proto.DisplayName(displayName),
			Circle:      circle,
			Backend:     proto.AgentType(backend),
			Role:        proto.PeerRole(role),
			Description: description,
		}
		if path.Valid {
			p := path.String
			m.Path = &p
		}
		if model.Valid {
			md := model.String
			m.Model = &md
		}
		if agentPID.Valid {
			pid := int(agentPID.Int64)
			m.AgentPID = &pid
		}
		if updatedAt.Valid && updatedAt.String != "" {
			t, err := parseTS(updatedAt.String)
			if err != nil {
				return nil, fmt.Errorf("mapping %s updated_at: %w", sessionID, err)
			}
			m.UpdatedAt = t
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mappings: %w", err)
	}
	return out, nil
}

// UpsertMapping persists one mapping row.
func (s *Store) UpsertMapping(ctx context.Context, m *proto.SessionMapping) error {
	const q = `INSERT OR REPLACE INTO peer_session_mappings
		(session_id, display_name, circle, backend, path, role, updated_at, description, model, agent_pid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var path any
	if m.Path != nil {
		path = *m.Path
	}
	var model any
	if m.Model != nil {
		model = *m.Model
	}
	var agentPID any
	if m.AgentPID != nil {
		agentPID = *m.AgentPID
	}
	updatedAt := m.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, q,
		string(m.SessionID),
		string(m.DisplayName),
		m.Circle,
		string(m.Backend),
		path,
		string(m.Role),
		formatTS(updatedAt),
		m.Description,
		model,
		agentPID,
	)
	if err != nil {
		return fmt.Errorf("upsert mapping %s: %w", m.SessionID, err)
	}
	return nil
}

// DeleteMapping removes a mapping by peer_id (session_id column).
func (s *Store) DeleteMapping(ctx context.Context, id proto.PeerID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM peer_session_mappings WHERE session_id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("delete mapping %s: %w", id, err)
	}
	return nil
}

// LoadRetired returns retired peer_ids whose retired_at >= cutoff.
func (s *Store) LoadRetired(ctx context.Context, cutoff time.Time) (map[proto.PeerID]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer_id, retired_at FROM retired_peers`)
	if err != nil {
		return nil, fmt.Errorf("load retired: %w", err)
	}
	defer rows.Close()

	out := make(map[proto.PeerID]time.Time)
	for rows.Next() {
		var (
			peerID    string
			retiredAt string
		)
		if err := rows.Scan(&peerID, &retiredAt); err != nil {
			return nil, fmt.Errorf("scan retired: %w", err)
		}
		t, err := parseTS(retiredAt)
		if err != nil {
			return nil, fmt.Errorf("retired %s retired_at: %w", peerID, err)
		}
		if t.Before(cutoff) {
			continue
		}
		out[proto.PeerID(peerID)] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retired: %w", err)
	}
	return out, nil
}

// Retire records a terminal peer_id.
func (s *Store) Retire(ctx context.Context, id proto.PeerID, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO retired_peers (peer_id, retired_at) VALUES (?, ?)`,
		string(id), formatTS(at),
	)
	if err != nil {
		return fmt.Errorf("retire %s: %w", id, err)
	}
	return nil
}

// Unretire clears a retirement.
func (s *Store) Unretire(ctx context.Context, id proto.PeerID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM retired_peers WHERE peer_id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("unretire %s: %w", id, err)
	}
	return nil
}

// AppendEvent writes one immutable journal row.
func (s *Store) AppendEvent(ctx context.Context, e peer.Event) error {
	eventID := e.EventID
	if eventID == "" {
		eventID = uuid.NewString()
	}

	payload := []byte("{}")
	if e.Payload != nil {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
		payload = b
	}

	ts := e.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	var peerID any
	if e.PeerID != "" {
		peerID = string(e.PeerID)
	}
	var peerName any
	if e.PeerName != "" {
		peerName = string(e.PeerName)
	}
	var sessionID any
	if e.SessionID != "" {
		sessionID = string(e.SessionID)
	}
	var turnID any
	if value, ok := e.Payload["turn_id"].(string); ok && value != "" {
		turnID = value
	}

	const q = `INSERT OR REPLACE INTO events
		(event_id, type, timestamp, peer_id, peer_name, session_id, turn_id, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		eventID,
		e.Type,
		formatTS(ts),
		peerID,
		peerName,
		sessionID,
		turnID,
		string(payload),
	)
	if err != nil {
		return fmt.Errorf("append event %s: %w", eventID, err)
	}
	return nil
}

// LoadRecentEvents returns the newest bounded event window in chronological
// order, reconstructing the canonical envelope around the stored payload.
func (s *Store) LoadRecentEvents(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, type, timestamp, peer_id,
		peer_name, session_id, turn_id, payload_json FROM events
		ORDER BY timestamp DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("load recent events: %w", err)
	}
	defer rows.Close()
	var reversed []map[string]any
	for rows.Next() {
		var id, typ, timestamp, payload string
		var peerID, peerName, sessionID, turnID sql.NullString
		if err := rows.Scan(&id, &typ, &timestamp, &peerID, &peerName, &sessionID, &turnID, &payload); err != nil {
			return nil, err
		}
		event := map[string]any{}
		_ = json.Unmarshal([]byte(payload), &event)
		event["id"], event["type"], event["timestamp"] = id, typ, timestamp
		if peerID.Valid {
			event["peer_id"] = peerID.String
		}
		if peerName.Valid {
			event["peer_name"] = peerName.String
		}
		if sessionID.Valid {
			event["session_id"] = sessionID.String
		}
		if turnID.Valid {
			event["turn_id"] = turnID.String
		}
		reversed = append(reversed, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed, nil
}
