package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// importLegacy imports the three pre-SQLite state files once and audits each
// source in legacy_imports. The JSON files remain untouched for downgrade and
// export compatibility.
func (s *Store) importLegacy(ctx context.Context, dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if err := s.importLegacySessions(ctx, filepath.Join(dir, "sessions.json")); err != nil {
		return err
	}
	if err := s.importLegacyEvents(ctx, filepath.Join(dir, "events.json")); err != nil {
		return err
	}
	return s.importLegacySchedules(ctx, filepath.Join(dir, "schedules.json"))
}

func (s *Store) importLegacySessions(ctx context.Context, path string) error {
	if s.legacyDone(ctx, path) || !regularFile(path) {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return s.recordLegacy(ctx, path, 0, "error", err.Error())
	}
	mappings := make([]proto.SessionMapping, 0, len(values))
	for id, value := range values {
		var mapping proto.SessionMapping
		if json.Unmarshal(value, &mapping) != nil {
			continue
		}
		if mapping.SessionID == "" {
			mapping.SessionID = proto.PeerID(id)
		}
		mappings = append(mappings, mapping)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, mapping := range mappings {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO peer_session_mappings(
			session_id, display_name, circle, backend, path, role, updated_at,
			description, model, agent_pid
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(mapping.SessionID), string(mapping.DisplayName), mapping.Circle, string(mapping.Backend),
			strOrNil(mapping.Path), string(mapping.Role), legacyTime(mapping.UpdatedAt),
			mapping.Description, strOrNil(mapping.Model), nullable(mapping.AgentPID)); err != nil {
			return err
		}
	}
	if err := recordLegacy(ctx, tx, path, len(mappings), "ok", nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) importLegacyEvents(ctx context.Context, path string) error {
	if s.legacyDone(ctx, path) || !regularFile(path) {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var events []map[string]any
	if err := json.Unmarshal(raw, &events); err != nil {
		return s.recordLegacy(ctx, path, 0, "error", err.Error())
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	imported := 0
	for _, event := range events {
		id, idOK := event["id"].(string)
		typ, typeOK := event["type"].(string)
		timestamp, timeOK := event["timestamp"].(string)
		if !idOK || !typeOK || !timeOK || id == "" || typ == "" || timestamp == "" {
			continue
		}
		payload, _ := json.Marshal(event)
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO events(
			event_id, type, timestamp, peer_id, peer_name, session_id, turn_id, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, typ, timestamp,
			nullString(firstLegacyString(event, "peer_id")),
			nullString(firstLegacyString(event, "peer", "from_peer", "to_peer")),
			nullString(firstLegacyString(event, "session_id", "repowire_session_id")),
			nullString(firstLegacyString(event, "turn_id")), string(payload)); err != nil {
			return err
		}
		imported++
	}
	if err := recordLegacy(ctx, tx, path, imported, "ok", nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) importLegacySchedules(ctx context.Context, path string) error {
	if !regularFile(path) {
		return nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedules`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return s.recordLegacy(ctx, path, 0, "error", err.Error())
	}
	schedules := make([]Schedule, 0, len(values))
	for id, value := range values {
		var wire struct {
			ScheduleID string  `json:"schedule_id"`
			FromPeer   string  `json:"from_peer"`
			ToPeer     string  `json:"to_peer"`
			Text       string  `json:"text"`
			FireAt     string  `json:"fire_at"`
			Kind       string  `json:"kind"`
			Circle     *string `json:"circle"`
			Cron       *string `json:"cron"`
			CreatedAt  string  `json:"created_at"`
		}
		if json.Unmarshal(value, &wire) != nil {
			continue
		}
		if wire.ScheduleID == "" {
			wire.ScheduleID = id
		}
		if wire.Kind == "" {
			wire.Kind = "notify"
		}
		schedules = append(schedules, Schedule{
			ScheduleID: wire.ScheduleID, FromPeer: wire.FromPeer, ToPeer: wire.ToPeer,
			Text: wire.Text, FireAt: wire.FireAt, Kind: wire.Kind, Circle: wire.Circle,
			Cron: wire.Cron, CreatedAt: wire.CreatedAt,
		})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, schedule := range schedules {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO schedules(
			schedule_id, from_peer, to_peer, text, kind, circle, fire_at, cron, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, schedule.ScheduleID, schedule.FromPeer,
			schedule.ToPeer, schedule.Text, schedule.Kind, strOrNil(schedule.Circle),
			schedule.FireAt, strOrNil(schedule.Cron), schedule.CreatedAt, formatTS(time.Now())); err != nil {
			return err
		}
	}
	if err := recordLegacy(ctx, tx, path, len(schedules), "ok", nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) legacyDone(ctx context.Context, path string) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM legacy_imports WHERE source_path = ?`, path).Scan(&one) == nil
}

func (s *Store) recordLegacy(ctx context.Context, path string, count int, status, detail string) error {
	return recordLegacy(ctx, s.db, path, count, status, nullString(detail))
}

type legacyExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordLegacy(ctx context.Context, db legacyExecer, path string, count int, status string, detail any) error {
	info, _ := os.Stat(path)
	var mtime, size any
	if info != nil {
		mtime, size = float64(info.ModTime().UnixNano())/1e9, info.Size()
	}
	_, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO legacy_imports(
		source_path, source_mtime, source_size, row_count, status, error
	) VALUES (?, ?, ?, ?, ?, ?)`, path, mtime, size, count, status, detail)
	return err
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func legacyTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func firstLegacyString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
