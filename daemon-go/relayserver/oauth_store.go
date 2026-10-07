package relayserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
type oauthGrant struct {
	ID, UserID, ClientID, Scope string
	Expires                     time.Time
	Revoked                     bool
}
type oauthPending struct {
	ClientID, Redirect, Challenge, Scope, State, Browser string
	Expires                                              time.Time
}
type oauthData struct {
	Issuer      string
	Clients     map[string]oauthClient
	Grants      map[string]oauthGrant
	Access      map[string]models.Token
	Refresh     map[string]models.Token
	Codes       map[string]models.Token
	UsedRefresh map[string]string
	Pending     map[string]oauthPending
}

func emptyOAuthData() oauthData {
	return oauthData{Clients: map[string]oauthClient{}, Grants: map[string]oauthGrant{}, Access: map[string]models.Token{}, Refresh: map[string]models.Token{}, Codes: map[string]models.Token{}, UsedRefresh: map[string]string{}, Pending: map[string]oauthPending{}}
}

// A single durable transaction covers code consumption, rotation, and revocation.
// Only hashes of bearer credentials are stored. No timers or in-memory authority.
type oauthStore struct{ db *sql.DB }

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
		if err := os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA busy_timeout=5000", "CREATE TABLE IF NOT EXISTS oauth_state (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL)"} {
		if _, err = db.Exec(q); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	initial, _ := json.Marshal(emptyOAuthData())
	if _, err = db.Exec("INSERT OR IGNORE INTO oauth_state(id,data) VALUES(1,?)", string(initial)); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &oauthStore{db}, nil
}
func (s *oauthStore) update(fn func(*oauthData) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRow("SELECT data FROM oauth_state WHERE id=1").Scan(&raw); err != nil {
		return err
	}
	d := emptyOAuthData()
	if err = json.Unmarshal([]byte(raw), &d); err != nil {
		return fmt.Errorf("invalid OAuth state: %w", err)
	}
	now := time.Now()
	for k, v := range d.Pending {
		if !now.Before(v.Expires) {
			delete(d.Pending, k)
		}
	}
	for k, v := range d.Codes {
		if !now.Before(v.CodeCreateAt.Add(v.CodeExpiresIn)) {
			delete(d.Codes, k)
		}
	}
	for k, v := range d.Access {
		if !now.Before(v.AccessCreateAt.Add(v.AccessExpiresIn)) {
			delete(d.Access, k)
		}
	}
	for k, v := range d.Refresh {
		if !now.Before(v.RefreshCreateAt.Add(v.RefreshExpiresIn)) {
			delete(d.Refresh, k)
		}
	}
	for k, v := range d.Grants {
		if !now.Before(v.Expires) {
			delete(d.Grants, k)
		}
	}
	for k, id := range d.UsedRefresh {
		if _, ok := d.Grants[id]; !ok {
			delete(d.UsedRefresh, k)
		}
	}
	if err = fn(&d); err != nil {
		return err
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if string(encoded) == raw {
		return tx.Commit()
	}
	if _, err = tx.Exec("UPDATE oauth_state SET data=? WHERE id=1", string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}
