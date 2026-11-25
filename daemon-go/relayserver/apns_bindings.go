package relayserver

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pushBindings records which relay user holds each APNs device token, and
// the relay sends a daemon's push only to tokens bound to its own user, so one
// tenant cannot put alerts on another tenant's phone.
//
// Knowing a token proves nothing (Apple treats tokens as identifiers), so a
// registration alone never binds. When a tunneled POST /push/devices
// succeeds, the relay opens a challenge: a background push carrying a random
// nonce, which only the device holding the token receives. The app echoes it
// to POST /push/devices/verify under its own relay key, and the token binds to
// the user that opened the challenge only if that is the verifier's user. A
// later verified registration by another user moves the token: only the
// device's real holder can complete one. With a path the bindings survive
// relay restarts; without one they live in memory and the app's next launch
// re-registers and re-verifies them.
type pushBindings struct {
	mu         sync.Mutex
	owners     map[string]binding
	challenges map[string]challenge // keyed by token
	db         *sql.DB
}

type binding struct {
	user string
	at   time.Time
}

type challenge struct {
	user    string
	nonce   string
	expires time.Time
}

const (
	// maxTokensPerUser bounds one user's bindings; the oldest is dropped first.
	maxTokensPerUser = 20
	// challengeTTL is how long a device has to echo its nonce.
	challengeTTL = 10 * time.Minute
	// maxChallenges bounds open challenges relay-wide; a full table refuses.
	maxChallenges = 4096
)

func newPushBindings() *pushBindings {
	return &pushBindings{owners: map[string]binding{}, challenges: map[string]challenge{}}
}

func openPushBindings(path string) (*pushBindings, error) {
	b := newPushBindings()
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
	if b.db == nil {
		return
	}
	if _, err := b.db.Exec(query, args...); err != nil {
		// The in-memory binding still holds; it is lost on the next restart.
		log.Printf("relay APNs: persist token binding: %v", err)
	}
}

// open starts a challenge for token on behalf of user and returns its nonce.
// A newer challenge for the same token replaces an older one.
func (b *pushBindings) open(token, user string, now time.Time) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for t, c := range b.challenges {
		if now.After(c.expires) {
			delete(b.challenges, t)
		}
	}
	if _, exists := b.challenges[token]; !exists && len(b.challenges) >= maxChallenges {
		return "", false
	}
	nonce := randomID("pv_", 24)
	b.challenges[token] = challenge{user: user, nonce: nonce, expires: now.Add(challengeTTL)}
	return nonce, true
}

// verify binds token to user when user opened the token's live challenge and
// nonce matches it. The challenge is spent either way it matches.
func (b *pushBindings) verify(token, nonce, user string, now time.Time) bool {
	b.mu.Lock()
	c, ok := b.challenges[token]
	matched := ok && now.Before(c.expires) && c.user == user &&
		subtle.ConstantTimeCompare([]byte(c.nonce), []byte(nonce)) == 1
	if matched {
		delete(b.challenges, token)
	}
	b.mu.Unlock()
	if matched {
		b.bind(token, user, now)
	}
	return matched
}

func (b *pushBindings) close() {
	if b.db != nil {
		_ = b.db.Close()
	}
}

// registration reports the device token a tunneled request registers (POST
// /push/devices) or removes (DELETE /push/devices/{token}), once the daemon
// accepted it.
func registration(method, path string, body []byte, status int) (token, environment string, removed bool) {
	if status < 200 || status >= 300 {
		return "", "", false
	}
	switch {
	case method == http.MethodPost && path == "/push/devices":
		var req struct {
			Token       string `json:"token"`
			Environment string `json:"environment"`
		}
		if json.Unmarshal(body, &req) != nil || !validDeviceToken(normalToken(req.Token)) {
			return "", "", false
		}
		if req.Environment == "" {
			req.Environment = "production"
		}
		return normalToken(req.Token), req.Environment, false
	case method == http.MethodDelete && strings.HasPrefix(path, "/push/devices/"):
		return normalToken(strings.TrimPrefix(path, "/push/devices/")), "", true
	}
	return "", "", false
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
