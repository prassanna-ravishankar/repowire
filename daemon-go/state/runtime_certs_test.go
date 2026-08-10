package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// certDDL is the runtime_identity_certificates table copied verbatim from
// repowire/daemon/state/database.py, plus user_version=12 so NewStore opens.
const certDDL = `
CREATE TABLE IF NOT EXISTS runtime_identity_certificates (
    nonce TEXT PRIMARY KEY,
    peer_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    backend TEXT NOT NULL,
    project_path TEXT NOT NULL,
    runtime_session_id TEXT,
    pane_id TEXT,
    agent_pid INTEGER,
    parent_pid INTEGER,
    issued_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    metadata TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_runtime_identity_certificates_peer
    ON runtime_identity_certificates(peer_id);
CREATE INDEX IF NOT EXISTS idx_runtime_identity_certificates_runtime
    ON runtime_identity_certificates(backend, runtime_session_id);
PRAGMA user_version=12;
`

func newCertStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "certs.db")
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	if _, err := seed.Exec(certDDL); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// envelopeFrom builds the full claim a peer would send back from its cert.
func envelopeFrom(c *RuntimeIdentityCertificate) CertEnvelope {
	return CertEnvelope{
		Nonce:            c.Nonce,
		PeerID:           c.PeerID,
		DisplayName:      c.DisplayName,
		Backend:          c.Backend,
		ProjectPath:      c.ProjectPath,
		RuntimeSessionID: c.RuntimeSessionID,
		PaneID:           c.PaneID,
		AgentPID:         c.AgentPID,
		ParentPID:        c.ParentPID,
	}
}

func TestMintAndValidateBirthCertificate(t *testing.T) {
	s := newCertStore(t)
	ctx := context.Background()

	cert, err := s.MintBirthCertificate(ctx,
		"peer-1", "alice", "claude-code",
		strp("/work/repo"), strp("rt-sess-9"), strp("%5"),
		intp(4242), intp(99),
		map[string]any{"k": "v"}, DefaultCertificateTTL, time.Time{},
	)
	if err != nil {
		t.Fatalf("MintBirthCertificate: %v", err)
	}
	if cert.Nonce == "" {
		t.Fatal("nonce should be generated")
	}
	if cert.ProjectPath != "/work/repo" {
		t.Errorf("project_path = %q", cert.ProjectPath)
	}

	got, err := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"claude-code", strp("/work/repo"), strp("%5"), intp(4242))
	if err != nil {
		t.Fatalf("ValidateBirthCertificate: %v", err)
	}
	if got == nil {
		t.Fatal("matching envelope should validate, got nil")
	}
	if got.Nonce != cert.Nonce || got.PeerID != "peer-1" {
		t.Errorf("validated cert mismatch: %+v", got)
	}
	if got.RuntimeSessionID == nil || *got.RuntimeSessionID != "rt-sess-9" {
		t.Errorf("runtime_session_id = %v", got.RuntimeSessionID)
	}
	if v, ok := got.Metadata["k"]; !ok || v != "v" {
		t.Errorf("metadata = %+v", got.Metadata)
	}
}

func TestMintNilOptionalsAndEmptyPath(t *testing.T) {
	s := newCertStore(t)
	ctx := context.Background()

	cert, err := s.MintBirthCertificate(ctx,
		"peer-2", "bob", "codex",
		nil, nil, nil, nil, nil,
		nil, 0, time.Time{},
	)
	if err != nil {
		t.Fatalf("MintBirthCertificate: %v", err)
	}
	if cert.ProjectPath != "" {
		t.Errorf("nil project_path should coerce to empty, got %q", cert.ProjectPath)
	}
	if cert.Metadata == nil {
		t.Error("nil metadata should default to {}")
	}

	got, err := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"codex", nil, nil, nil)
	if err != nil {
		t.Fatalf("ValidateBirthCertificate: %v", err)
	}
	if got == nil {
		t.Fatal("all-nil-optionals cert should validate")
	}
	if got.RuntimeSessionID != nil || got.PaneID != nil || got.AgentPID != nil || got.ParentPID != nil {
		t.Errorf("optionals should round-trip nil: %+v", got)
	}
}

func TestValidateRejectsMismatches(t *testing.T) {
	s := newCertStore(t)
	ctx := context.Background()
	cert, err := s.MintBirthCertificate(ctx,
		"peer-3", "carol", "claude-code",
		strp("/work/repo"), strp("rt-1"), strp("%2"),
		intp(100), intp(50), nil, DefaultCertificateTTL, time.Time{},
	)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Unknown nonce.
	if got, _ := s.ValidateBirthCertificate(ctx,
		CertEnvelope{Nonce: "does-not-exist"}, "claude-code", strp("/work/repo"), strp("%2"), intp(100)); got != nil {
		t.Error("unknown nonce should not validate")
	}
	// Empty nonce.
	if got, _ := s.ValidateBirthCertificate(ctx,
		CertEnvelope{}, "claude-code", strp("/work/repo"), strp("%2"), intp(100)); got != nil {
		t.Error("empty nonce should not validate")
	}
	// Wrong process backend.
	if got, _ := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"codex", strp("/work/repo"), strp("%2"), intp(100)); got != nil {
		t.Error("backend mismatch should not validate")
	}
	// Wrong process project path.
	if got, _ := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"claude-code", strp("/other"), strp("%2"), intp(100)); got != nil {
		t.Error("project_path mismatch should not validate")
	}
	// Contradicting live pane.
	if got, _ := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"claude-code", strp("/work/repo"), strp("%99"), intp(100)); got != nil {
		t.Error("contradicting pane_id should not validate")
	}
	// Contradicting live pid.
	if got, _ := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"claude-code", strp("/work/repo"), strp("%2"), intp(777)); got != nil {
		t.Error("contradicting agent_pid should not validate")
	}
	// Tampered envelope (wrong display_name).
	bad := envelopeFrom(cert)
	bad.DisplayName = "mallory"
	if got, _ := s.ValidateBirthCertificate(ctx, bad,
		"claude-code", strp("/work/repo"), strp("%2"), intp(100)); got != nil {
		t.Error("tampered display_name should not validate")
	}
}

func TestValidateExpiredFailsClosed(t *testing.T) {
	s := newCertStore(t)
	ctx := context.Background()
	// Issue in the past with a 1s TTL so it is already expired.
	past := time.Now().UTC().Add(-time.Hour)
	cert, err := s.MintBirthCertificate(ctx,
		"peer-4", "dave", "codex",
		strp("/repo"), nil, nil, nil, nil,
		nil, time.Second, past,
	)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := s.ValidateBirthCertificate(ctx, envelopeFrom(cert),
		"codex", strp("/repo"), nil, nil)
	if err != nil {
		t.Fatalf("ValidateBirthCertificate: %v", err)
	}
	if got != nil {
		t.Error("expired certificate should fail closed")
	}
}
