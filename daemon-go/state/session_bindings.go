package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// BindingStatus is the lifecycle state of a session binding. Mirrors the Python
// Literal["active", "detached", "resumable", "archived", "lost", "superseded"].
type BindingStatus string

const (
	BindingActive     BindingStatus = "active"
	BindingDetached   BindingStatus = "detached"
	BindingResumable  BindingStatus = "resumable"
	BindingArchived   BindingStatus = "archived"
	BindingLost       BindingStatus = "lost"
	BindingSuperseded BindingStatus = "superseded"
)

// SessionBinding is the durable binding between a Repowire workstream and runtime
// session metadata. It persists only identifiers, locators, cursors, and small
// metadata envelopes; raw transcript bodies stay in backend-owned sources.
//
// created_at/last_seen_at are stored verbatim as the strings the Python daemon
// writes (datetime.isoformat()); we don't reparse/reformat them, matching the
// Python store which round-trips them as opaque strings.
type SessionBinding struct {
	RepowireSessionID     string
	PeerID                *string
	CurrentExecutorPeerID *string
	Backend               string
	ProjectPath           string
	RuntimeSessionID      *string
	RuntimeSourceURI      *string
	SourceCursor          map[string]any
	Provenance            map[string]any
	ResumeCapability      map[string]any
	Status                BindingStatus
	Metadata              map[string]any
	CreatedAt             string
	LastSeenAt            string
}

// sbMergeDicts mirrors Python _merge_dicts: copy existing, overlay non-nil updates.
func sbMergeDicts(existing, updates map[string]any) map[string]any {
	out := make(map[string]any, len(existing))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range updates {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// sessionBindingColumns is the column order for SELECT * round-trips.
const sessionBindingColumns = `repowire_session_id, peer_id, current_executor_peer_id, backend, project_path, runtime_session_id, runtime_source_uri, source_cursor, provenance, resume_capability, status, metadata, created_at, last_seen_at`

func sbScan(row interface{ Scan(...any) error }) (*SessionBinding, error) {
	var (
		repowireSessionID string
		peerID            sql.NullString
		execPeerID        sql.NullString
		backend           string
		projectPath       string
		runtimeSessionID  sql.NullString
		runtimeSourceURI  sql.NullString
		sourceCursor      sql.NullString
		provenance        sql.NullString
		resumeCapability  sql.NullString
		status            string
		metadata          sql.NullString
		createdAt         string
		lastSeenAt        string
	)
	if err := row.Scan(
		&repowireSessionID, &peerID, &execPeerID, &backend, &projectPath,
		&runtimeSessionID, &runtimeSourceURI, &sourceCursor, &provenance,
		&resumeCapability, &status, &metadata, &createdAt, &lastSeenAt,
	); err != nil {
		return nil, err
	}
	b := &SessionBinding{
		RepowireSessionID:     repowireSessionID,
		Backend:               backend,
		ProjectPath:           projectPath,
		PeerID:                nullStringPtr(peerID),
		CurrentExecutorPeerID: nullStringPtr(execPeerID),
		RuntimeSessionID:      nullStringPtr(runtimeSessionID),
		RuntimeSourceURI:      nullStringPtr(runtimeSourceURI),
		SourceCursor:          loadJSONObject(sourceCursor),
		Provenance:            loadJSONObject(provenance),
		ResumeCapability:      loadJSONObject(resumeCapability),
		Status:                BindingStatus(status),
		Metadata:              loadJSONObject(metadata),
		CreatedAt:             createdAt,
		LastSeenAt:            lastSeenAt,
	}
	return b, nil
}

// Observation captures one observed runtime edge for UpsertObservation. Nil-able
// fields use pointers/maps to distinguish "unset" (don't touch) from empty.
type Observation struct {
	PeerID           *string
	Backend          string
	ProjectPath      *string
	RuntimeSessionID *string
	RuntimeSourceURI *string
	SourceCursor     map[string]any
	Provenance       map[string]any
	ResumeCapability map[string]any
	Status           BindingStatus
	Metadata         map[string]any
	ObservedAt       string // optional; defaults to now
}

// UpsertObservation creates or updates the binding matching an observed runtime
// edge. Matching precedence: source_uri, then runtime_session_id (scoped by
// backend+project), then peer_id with NULL runtime markers. Mirrors the Python
// SQLiteSessionBindingStore.upsert_observation.
func (s *Store) UpsertObservation(ctx context.Context, obs Observation) (*SessionBinding, error) {
	normalizedPath := ""
	if obs.ProjectPath != nil {
		normalizedPath = *obs.ProjectPath
	}
	now := obs.ObservedAt
	if now == "" {
		now = nowISO()
	}
	status := obs.Status
	if status == "" {
		status = BindingActive
	}

	existing, err := s.findExistingBinding(ctx, obs.PeerID, obs.Backend, normalizedPath, obs.RuntimeSessionID, obs.RuntimeSourceURI)
	if err != nil {
		return nil, err
	}

	var binding *SessionBinding
	if existing == nil {
		binding = &SessionBinding{
			RepowireSessionID:     newID("rw-session-"),
			PeerID:                obs.PeerID,
			CurrentExecutorPeerID: obs.PeerID,
			Backend:               obs.Backend,
			ProjectPath:           normalizedPath,
			RuntimeSessionID:      obs.RuntimeSessionID,
			RuntimeSourceURI:      obs.RuntimeSourceURI,
			SourceCursor:          sbOrEmpty(obs.SourceCursor),
			Provenance:            sbOrEmpty(obs.Provenance),
			ResumeCapability:      sbOrEmpty(obs.ResumeCapability),
			Status:                status,
			Metadata:              sbOrEmpty(obs.Metadata),
			CreatedAt:             now,
			LastSeenAt:            now,
		}
	} else {
		binding = &SessionBinding{
			RepowireSessionID:     existing.RepowireSessionID,
			PeerID:                sbCoalesce(obs.PeerID, existing.PeerID),
			CurrentExecutorPeerID: sbCoalesce(obs.PeerID, existing.CurrentExecutorPeerID),
			Backend:               obs.Backend,
			ProjectPath:           sbCoalesceStr(normalizedPath, existing.ProjectPath),
			RuntimeSessionID:      sbCoalesce(obs.RuntimeSessionID, existing.RuntimeSessionID),
			RuntimeSourceURI:      sbCoalesce(obs.RuntimeSourceURI, existing.RuntimeSourceURI),
			SourceCursor:          sbMergeDicts(existing.SourceCursor, obs.SourceCursor),
			Provenance:            sbMergeDicts(existing.Provenance, obs.Provenance),
			ResumeCapability:      sbMergeDicts(existing.ResumeCapability, obs.ResumeCapability),
			Status:                status,
			Metadata:              sbMergeDicts(existing.Metadata, obs.Metadata),
			CreatedAt:             existing.CreatedAt,
			LastSeenAt:            now,
		}
	}

	const q = `INSERT OR REPLACE INTO session_bindings(
		repowire_session_id, peer_id, current_executor_peer_id, backend,
		project_path, runtime_session_id, runtime_source_uri, source_cursor,
		provenance, resume_capability, status, metadata, created_at, last_seen_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q,
		binding.RepowireSessionID,
		strOrNil(binding.PeerID),
		strOrNil(binding.CurrentExecutorPeerID),
		binding.Backend,
		binding.ProjectPath,
		strOrNil(binding.RuntimeSessionID),
		strOrNil(binding.RuntimeSourceURI),
		dumpJSONObject(binding.SourceCursor),
		dumpJSONObject(binding.Provenance),
		dumpJSONObject(binding.ResumeCapability),
		string(binding.Status),
		dumpJSONObject(binding.Metadata),
		binding.CreatedAt,
		binding.LastSeenAt,
	); err != nil {
		return nil, fmt.Errorf("upsert session binding %s: %w", binding.RepowireSessionID, err)
	}
	return binding, nil
}

// findExistingBinding mirrors the Python _find_existing precedence.
func (s *Store) findExistingBinding(ctx context.Context, peerID *string, backend, projectPath string, runtimeSessionID, runtimeSourceURI *string) (*SessionBinding, error) {
	if runtimeSourceURI != nil && *runtimeSourceURI != "" {
		found, err := s.GetBySourceURI(ctx, *runtimeSourceURI)
		if err != nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	if runtimeSessionID != nil && *runtimeSessionID != "" {
		var pp *string
		if projectPath != "" {
			pp = &projectPath
		}
		found, err := s.GetByRuntimeSession(ctx, *runtimeSessionID, &backend, pp)
		if err != nil {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	if peerID != nil && *peerID != "" {
		q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings
			WHERE peer_id = ? AND backend = ? AND project_path = ?
			  AND runtime_session_id IS NULL AND runtime_source_uri IS NULL
			ORDER BY last_seen_at DESC LIMIT 1`
		row := s.db.QueryRowContext(ctx, q, *peerID, backend, projectPath)
		b, err := sbScan(row)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("find existing binding by peer: %w", err)
		}
		return b, nil
	}
	return nil, nil
}

// GetSessionBinding fetches a binding by its repowire_session_id, or nil if absent.
func (s *Store) GetSessionBinding(ctx context.Context, repowireSessionID string) (*SessionBinding, error) {
	q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings WHERE repowire_session_id = ?`
	b, err := sbScan(s.db.QueryRowContext(ctx, q, repowireSessionID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session binding %s: %w", repowireSessionID, err)
	}
	return b, nil
}

// ListBindingsByPeer returns every binding for a peer, newest last_seen_at first.
func (s *Store) ListBindingsByPeer(ctx context.Context, peerID string) ([]*SessionBinding, error) {
	q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings WHERE peer_id = ? ORDER BY last_seen_at DESC`
	return s.querySessionBindings(ctx, q, peerID)
}

// GetByRuntimeSession returns the newest binding matching a runtime session id,
// optionally scoped by backend and/or project_path. nil backend/projectPath =
// not constrained, matching the Python keyword defaults.
func (s *Store) GetByRuntimeSession(ctx context.Context, runtimeSessionID string, backend, projectPath *string) (*SessionBinding, error) {
	clauses := []string{"runtime_session_id = ?"}
	args := []any{runtimeSessionID}
	if backend != nil {
		clauses = append(clauses, "backend = ?")
		args = append(args, *backend)
	}
	if projectPath != nil {
		clauses = append(clauses, "project_path = ?")
		args = append(args, *projectPath)
	}
	q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings WHERE ` +
		strings.Join(clauses, " AND ") + ` ORDER BY last_seen_at DESC LIMIT 1`
	b, err := sbScan(s.db.QueryRowContext(ctx, q, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get binding by runtime session: %w", err)
	}
	return b, nil
}

// ListBindingsByBackendProject returns bindings for a backend+project, newest first.
func (s *Store) ListBindingsByBackendProject(ctx context.Context, backend, projectPath string) ([]*SessionBinding, error) {
	q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings WHERE backend = ? AND project_path = ? ORDER BY last_seen_at DESC`
	return s.querySessionBindings(ctx, q, backend, projectPath)
}

// GetBySourceURI returns the newest binding for a runtime source URI, or nil.
func (s *Store) GetBySourceURI(ctx context.Context, runtimeSourceURI string) (*SessionBinding, error) {
	q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings WHERE runtime_source_uri = ? ORDER BY last_seen_at DESC LIMIT 1`
	b, err := sbScan(s.db.QueryRowContext(ctx, q, runtimeSourceURI))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get binding by source uri: %w", err)
	}
	return b, nil
}

// ListAllBindings returns every binding, newest last_seen_at first.
func (s *Store) ListAllBindings(ctx context.Context) ([]*SessionBinding, error) {
	q := `SELECT ` + sessionBindingColumns + ` FROM session_bindings ORDER BY last_seen_at DESC`
	return s.querySessionBindings(ctx, q)
}

func (s *Store) querySessionBindings(ctx context.Context, query string, args ...any) ([]*SessionBinding, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query session bindings: %w", err)
	}
	defer rows.Close()
	var out []*SessionBinding
	for rows.Next() {
		b, err := sbScan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session binding: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session bindings: %w", err)
	}
	return out, nil
}

func sbOrEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// sbCoalesce returns next if it is a non-empty string pointer, else fallback.
// Mirrors Python `a or b` truthiness for optional strings.
func sbCoalesce(next, fallback *string) *string {
	if next != nil && *next != "" {
		return next
	}
	return fallback
}

func sbCoalesceStr(next, fallback string) string {
	if next != "" {
		return next
	}
	return fallback
}
