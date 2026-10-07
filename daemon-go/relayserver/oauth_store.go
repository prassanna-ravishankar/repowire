package relayserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/go-oauth2/oauth2/v4/models"
	_ "modernc.org/sqlite"
)

type oauthClient struct {
	ID        string   `json:"client_id"`
	Name      string   `json:"client_name"`
	Redirects []string `json:"redirect_uris"`
	Method    string   `json:"token_endpoint_auth_method"`
}

// Internal client metadata stays out of the DCR response.
type storedOAuthClient struct {
	oauthClient
	Expires time.Time
	Linked  bool
}
type oauthGrant struct {
	ID, UserID, ClientID, Scope string
	Expires                     time.Time
	Revoked, Active             bool
}
type oauthPending struct {
	ID, ClientID, Redirect, Challenge, Scope, State, Browser string
	Expires                                                  time.Time
}
type oauthReplay struct {
	GrantID string
	Expires time.Time
}
type oauthData struct {
	tx                     *sql.Tx
	err                    error
	Clients                oauthRecords[storedOAuthClient]
	Grants                 oauthRecords[oauthGrant]
	Access, Refresh, Codes oauthRecords[models.Token]
	UsedRefresh            oauthRecords[oauthReplay]
	Consumed               oauthRecords[oauthPending]
}

// Each credential is an independently indexed row. Authentication reads only its
// token and grant; unrelated registrations never amplify that read into a scan.
// Transactions still cover consume+mint, refresh rotation and family revocation.
type oauthStore struct{ db *sql.DB }
type oauthRecords[T any] struct {
	d        *oauthData
	kind     string
	metadata func(T) (time.Time, string)
}

func (r oauthRecords[T]) get(key string) (v T, ok bool) {
	if r.d.err != nil {
		return
	}
	var raw string
	err := r.d.tx.QueryRow("SELECT data FROM oauth_records WHERE kind=? AND key=? AND expires>?", r.kind, key, time.Now().UnixNano()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &v)
	}
	if err != nil {
		r.d.err = err
		return
	}
	return v, true
}
func (r oauthRecords[T]) put(key string, v T) {
	if r.d.err != nil {
		return
	}
	raw, err := json.Marshal(v)
	if err == nil {
		expires, owner := r.metadata(v)
		_, err = r.d.tx.Exec("INSERT INTO oauth_records(kind,key,data,expires,owner) VALUES(?,?,?,?,?) ON CONFLICT(kind,key) DO UPDATE SET data=excluded.data,expires=excluded.expires,owner=excluded.owner", r.kind, key, string(raw), expires.UnixNano(), owner)
	}
	r.d.err = err
}
func (r oauthRecords[T]) remove(key string) {
	if r.d.err == nil {
		_, r.d.err = r.d.tx.Exec("DELETE FROM oauth_records WHERE kind=? AND key=?", r.kind, key)
	}
}
func (r oauthRecords[T]) list(owner string) []T {
	if r.d.err != nil {
		return nil
	}
	rows, err := r.d.tx.Query("SELECT data FROM oauth_records WHERE kind=? AND owner=? AND expires>?", r.kind, owner, time.Now().UnixNano())
	if err != nil {
		r.d.err = err
		return nil
	}
	defer rows.Close()
	var result []T
	for rows.Next() {
		var raw string
		var v T
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal([]byte(raw), &v)
		}
		if err != nil {
			r.d.err = err
			return nil
		}
		result = append(result, v)
	}
	r.d.err = rows.Err()
	return result
}
func newOAuthData(tx *sql.Tx) *oauthData {
	d := &oauthData{tx: tx}
	d.Clients = oauthRecords[storedOAuthClient]{d, "client", func(c storedOAuthClient) (time.Time, string) {
		if c.Linked {
			return c.Expires, "linked"
		}
		return c.Expires, "unlinked"
	}}
	d.Grants = oauthRecords[oauthGrant]{d, "grant", func(g oauthGrant) (time.Time, string) { return g.Expires, g.UserID }}
	d.Access = oauthRecords[models.Token]{d, "access", func(t models.Token) (time.Time, string) { return t.AccessCreateAt.Add(t.AccessExpiresIn), t.UserID }}
	d.Refresh = oauthRecords[models.Token]{d, "refresh", func(t models.Token) (time.Time, string) { return t.RefreshCreateAt.Add(t.RefreshExpiresIn), t.UserID }}
	d.Codes = oauthRecords[models.Token]{d, "code", func(t models.Token) (time.Time, string) { return t.CodeCreateAt.Add(t.CodeExpiresIn), t.UserID }}
	d.UsedRefresh = oauthRecords[oauthReplay]{d, "replay", func(v oauthReplay) (time.Time, string) { return v.Expires, v.GrantID }}
	d.Consumed = oauthRecords[oauthPending]{d, "consent", func(p oauthPending) (time.Time, string) { return p.Expires, "" }}
	return d
}
func openOAuthStore(path string) (*oauthStore, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		"PRAGMA busy_timeout=5000",
		"CREATE TABLE IF NOT EXISTS oauth_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"CREATE TABLE IF NOT EXISTS oauth_records (kind TEXT NOT NULL,key TEXT NOT NULL,data TEXT NOT NULL,expires INTEGER NOT NULL,owner TEXT NOT NULL,PRIMARY KEY(kind,key))",
		"CREATE INDEX IF NOT EXISTS oauth_expiry ON oauth_records(expires)",
		"CREATE INDEX IF NOT EXISTS oauth_owner ON oauth_records(kind,owner,expires)",
	} {
		if _, err = db.Exec(q); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &oauthStore{db}, nil
}

// SQLite treats TxOptions.ReadOnly as advisory. view callbacks must only read;
// the access-check regression test enforces this with write-rejecting triggers.
func (s *oauthStore) transact(readOnly bool, fn func(*oauthData) error) error {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !readOnly {
		if _, err = tx.Exec("DELETE FROM oauth_records WHERE expires<=?", time.Now().UnixNano()); err != nil {
			return err
		}
	}
	d := newOAuthData(tx)
	if err = fn(d); err != nil {
		return err
	}
	if d.err != nil {
		return d.err
	}
	return tx.Commit()
}
func (s *oauthStore) update(fn func(*oauthData) error) error { return s.transact(false, fn) }
func (s *oauthStore) view(fn func(*oauthData) error) error   { return s.transact(true, fn) }
