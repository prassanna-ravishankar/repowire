package relayserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pushBindings records which relay user registered each APNs device token.
// The relay sees registrations pass through the tunnel (POST and DELETE
// /push/devices) and only sends a daemon's push to tokens its own user
// registered, so one tenant cannot put alerts on another tenant's phone. A
// re-registration by a different user moves the token: the latest owner of
// the device wins. With a path the bindings survive relay restarts; without
// one they live in memory and the app's next launch re-registers them.
type pushBindings struct {
	mu     sync.Mutex
	owners map[string]binding
	db     *sql.DB
}

type binding struct {
	user string
	at   time.Time
}

// maxTokensPerUser bounds one user's bindings; the oldest is dropped first.
const maxTokensPerUser = 20

func openPushBindings(path string) (*pushBindings, error) {
	b := &pushBindings{owners: map[string]binding{}}
	if path == "" {
		return b, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS push_bindings (token TEXT PRIMARY KEY, user_id TEXT NOT NULL, bound_at INTEGER NOT NULL)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	rows, err := db.Query(`SELECT token, user_id, bound_at FROM push_bindings`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var token, user string
		var at int64
		if err := rows.Scan(&token, &user, &at); err != nil {
			_ = db.Close()
			return nil, err
		}
		b.owners[token] = binding{user: user, at: time.Unix(at, 0)}
	}
	b.db = db
	return b, rows.Err()
}

func (b *pushBindings) bind(token, user string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.owners[token] = binding{user: user, at: now}
	b.persist(`INSERT INTO push_bindings(token, user_id, bound_at) VALUES (?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET user_id = excluded.user_id, bound_at = excluded.bound_at`, token, user, now.Unix())
	var mine []string
	for t, o := range b.owners {
		if o.user == user {
			mine = append(mine, t)
		}
	}
	for len(mine) > maxTokensPerUser {
		oldest := 0
		for i, t := range mine {
			if b.owners[t].at.Before(b.owners[mine[oldest]].at) {
				oldest = i
			}
		}
		b.dropLocked(mine[oldest])
		mine = append(mine[:oldest], mine[oldest+1:]...)
	}
}

// unbind forgets token only when user owns it.
func (b *pushBindings) unbind(token, user string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.owners[token].user == user {
		b.dropLocked(token)
	}
}

func (b *pushBindings) owner(token string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.owners[token].user
}

func (b *pushBindings) dropLocked(token string) {
	delete(b.owners, token)
	b.persist(`DELETE FROM push_bindings WHERE token = ?`, token)
}

func (b *pushBindings) persist(query string, args ...any) {
	if b.db != nil {
		_, _ = b.db.Exec(query, args...)
	}
}

func (b *pushBindings) close() {
	if b.db != nil {
		_ = b.db.Close()
	}
}

// observe binds or unbinds a token from a tunneled registration the daemon
// accepted. It reads only the routes the iOS app's device registry uses.
func (b *pushBindings) observe(method, path string, body []byte, status int, user string) {
	if status < 200 || status >= 300 || user == "" {
		return
	}
	switch {
	case method == http.MethodPost && path == "/push/devices":
		var req struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(body, &req) == nil && validDeviceToken(normalToken(req.Token)) {
			b.bind(normalToken(req.Token), user, time.Now())
		}
	case method == http.MethodDelete && strings.HasPrefix(path, "/push/devices/"):
		b.unbind(normalToken(strings.TrimPrefix(path, "/push/devices/")), user)
	}
}

func normalToken(token string) string {
	return strings.ToLower(strings.TrimSpace(token))
}

// validDeviceToken accepts lowercase hex APNs tokens, the same rule the
// daemon's registry applies.
func validDeviceToken(token string) bool {
	if len(token) < 32 || len(token) > 200 {
		return false
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
