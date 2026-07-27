package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// DefaultCertificateTTL mirrors DEFAULT_CERTIFICATE_TTL_SECONDS in
// repowire/daemon/state/session_bindings.py (24h).
const DefaultCertificateTTL = 24 * time.Hour

// RuntimeIdentityCertificate is the daemon-minted proof that binds a live
// runtime to a peer identity. Fields map 1:1 to the
// runtime_identity_certificates table columns. Nullable text/int columns are
// represented as pointers; empty-but-NOT-NULL columns (project_path) are plain
// strings, matching the Python store which coerces None to "".
type RuntimeIdentityCertificate struct {
	Nonce            string
	PeerID           string
	DisplayName      string
	Backend          string
	ProjectPath      string
	RuntimeSessionID *string
	PaneID           *string
	AgentPID         *int
	ParentPID        *int
	IssuedAt         string
	ExpiresAt        string
	Metadata         map[string]any
}

// CertEnvelope is the caller-supplied claim validated against persisted state.
// It mirrors RuntimeIdentityCertificate.as_envelope() in the Python store: the
// fields a peer sends back to prove it is the same runtime.
type CertEnvelope struct {
	Nonce            string  `json:"nonce"`
	PeerID           string  `json:"peer_id"`
	DisplayName      string  `json:"display_name"`
	Backend          string  `json:"backend"`
	ProjectPath      string  `json:"project_path"`
	RuntimeSessionID *string `json:"runtime_session_id"`
	PaneID           *string `json:"pane_id"`
	AgentPID         *int    `json:"agent_pid"`
	ParentPID        *int    `json:"parent_pid"`
}

// newNonce returns 32 random bytes URL-safe base64-encoded without padding,
// matching Python's secrets.token_urlsafe(32).
func newNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// optStr compares an optional string column against an optional envelope value
// for the SQL-NULL-aware equality the Python store relies on (None == None).
func optStrEqual(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func optIntEqual(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// MintBirthCertificate persists and returns an unguessable runtime identity
// certificate. issuedAt, when non-zero, pins the issue time (the rest derive
// from it); otherwise time.Now() is used. project_path is coerced to "" when
// nil, matching the Python NOT NULL column semantics.
func (s *Store) MintBirthCertificate(
	ctx context.Context,
	peerID string,
	displayName string,
	backend string,
	projectPath *string,
	runtimeSessionID *string,
	paneID *string,
	agentPID *int,
	parentPID *int,
	metadata map[string]any,
	ttl time.Duration,
	issuedAt time.Time,
) (*RuntimeIdentityCertificate, error) {
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultCertificateTTL
	}
	issued := issuedAt
	if issued.IsZero() {
		issued = time.Now()
	}
	issued = issued.UTC()
	expires := issued.Add(ttl)
	if metadata == nil {
		metadata = map[string]any{}
	}
	path := ""
	if projectPath != nil {
		path = *projectPath
	}

	cert := &RuntimeIdentityCertificate{
		Nonce:            nonce,
		PeerID:           peerID,
		DisplayName:      displayName,
		Backend:          backend,
		ProjectPath:      path,
		RuntimeSessionID: runtimeSessionID,
		PaneID:           paneID,
		AgentPID:         agentPID,
		ParentPID:        parentPID,
		IssuedAt:         formatTS(issued),
		ExpiresAt:        formatTS(expires),
		Metadata:         metadata,
	}

	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal cert metadata: %w", err)
	}

	const q = `INSERT INTO runtime_identity_certificates(
		nonce, peer_id, display_name, backend, project_path,
		runtime_session_id, pane_id, agent_pid, parent_pid,
		issued_at, expires_at, metadata
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q,
		cert.Nonce,
		cert.PeerID,
		cert.DisplayName,
		cert.Backend,
		cert.ProjectPath,
		strOrNil(cert.RuntimeSessionID),
		strOrNil(cert.PaneID),
		nullable(cert.AgentPID),
		nullable(cert.ParentPID),
		cert.IssuedAt,
		cert.ExpiresAt,
		string(metaJSON),
	); err != nil {
		return nil, fmt.Errorf("insert birth certificate: %w", err)
	}
	return cert, nil
}

// ValidateBirthCertificate returns the persisted certificate when the caller
// proves the same runtime. It is deliberately stricter than path lookup: the
// nonce must match daemon state, backend/path must match the current process,
// optional pane evidence must not contradict, pid evidence must match when both
// sides have it, and expired envelopes fail closed. Returns (nil, nil) on any
// mismatch — a not-found is not an error, exactly like the Python None return.
func (s *Store) ValidateBirthCertificate(
	ctx context.Context,
	envelope CertEnvelope,
	backend string,
	projectPath *string,
	paneID *string,
	agentPID *int,
) (*RuntimeIdentityCertificate, error) {
	if envelope.Nonce == "" {
		return nil, nil
	}
	cert, err := s.getCertByNonce(ctx, envelope.Nonce)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, nil
	}

	expires, err := parseTS(cert.ExpiresAt)
	if err != nil {
		return nil, nil
	}
	if !expires.After(time.Now().UTC()) {
		return nil, nil
	}

	if cert.Backend != backend {
		return nil, nil
	}
	wantPath := ""
	if projectPath != nil {
		wantPath = *projectPath
	}
	if cert.ProjectPath != wantPath {
		return nil, nil
	}

	if envelope.PeerID != cert.PeerID {
		return nil, nil
	}
	if envelope.DisplayName != cert.DisplayName {
		return nil, nil
	}
	if envelope.Backend != cert.Backend {
		return nil, nil
	}
	if envelope.ProjectPath != cert.ProjectPath {
		return nil, nil
	}
	if !optStrEqual(envelope.RuntimeSessionID, cert.RuntimeSessionID) {
		return nil, nil
	}
	// Pane evidence must not contradict when both the cert and the live process
	// carry it; then the envelope's pane claim must match the cert exactly.
	if cert.PaneID != nil && paneID != nil && *cert.PaneID != *paneID {
		return nil, nil
	}
	if !optStrEqual(envelope.PaneID, cert.PaneID) {
		return nil, nil
	}
	// Pid evidence must match when both sides have it; then the envelope's pid
	// claim must match the cert exactly.
	if cert.AgentPID != nil && agentPID != nil && *cert.AgentPID != *agentPID {
		return nil, nil
	}
	if !optIntEqual(envelope.AgentPID, cert.AgentPID) {
		return nil, nil
	}
	if !optIntEqual(envelope.ParentPID, cert.ParentPID) {
		return nil, nil
	}
	return cert, nil
}

// ValidateMCPIdentityProof checks the unguessable nonce carried by the local
// stdio MCP shim. Full runtime evidence was validated when the shim resolved
// the certificate; the HTTP hop only needs to bind that current certificate to
// the claimed peer and reject expired or invented claims.
func (s *Store) ValidateMCPIdentityProof(ctx context.Context, nonce, peerID string) bool {
	if nonce == "" || peerID == "" {
		return false
	}
	cert, err := s.getCertByNonce(ctx, nonce)
	if err != nil || cert == nil || cert.PeerID != peerID {
		return false
	}
	expires, err := parseTS(cert.ExpiresAt)
	return err == nil && expires.After(time.Now().UTC())
}

func (s *Store) getCertByNonce(ctx context.Context, nonce string) (*RuntimeIdentityCertificate, error) {
	const q = `SELECT nonce, peer_id, display_name, backend, project_path,
		runtime_session_id, pane_id, agent_pid, parent_pid,
		issued_at, expires_at, metadata
		FROM runtime_identity_certificates WHERE nonce = ?`
	row := s.db.QueryRowContext(ctx, q, nonce)

	var (
		runtimeSessionID sql.NullString
		paneID           sql.NullString
		agentPID         sql.NullInt64
		parentPID        sql.NullInt64
		metaRaw          string
	)
	cert := &RuntimeIdentityCertificate{}
	err := row.Scan(
		&cert.Nonce,
		&cert.PeerID,
		&cert.DisplayName,
		&cert.Backend,
		&cert.ProjectPath,
		&runtimeSessionID,
		&paneID,
		&agentPID,
		&parentPID,
		&cert.IssuedAt,
		&cert.ExpiresAt,
		&metaRaw,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan certificate: %w", err)
	}

	cert.RuntimeSessionID = nullStringPtr(runtimeSessionID)
	cert.PaneID = nullStringPtr(paneID)
	if agentPID.Valid {
		v := int(agentPID.Int64)
		cert.AgentPID = &v
	}
	if parentPID.Valid {
		v := int(parentPID.Int64)
		cert.ParentPID = &v
	}
	cert.Metadata = decodeJSONObject(metaRaw)
	return cert, nil
}
