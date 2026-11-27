package relayserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type APIKey struct {
	Key    string `json:"api_key"`
	UserID string `json:"user_id"`
}

type ShareToken struct {
	ShareID  string `json:"share_id"`
	UserID   string `json:"-"`
	PeerName string `json:"peer_name"`
	// PeerID is the shared peer's identity when the daemon sent one (#652). A
	// peer's name follows its runtime's session name and can change, or pass
	// to another peer, while the link is live; its peer_id cannot. Empty for
	// a share made by a daemon that predates it: the name is then the scope.
	PeerID      string     `json:"peer_id,omitempty"`
	Permissions string     `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

func (t ShareToken) expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

type tokenStore struct {
	mu     sync.Mutex
	shares map[string]ShareToken
}

func newTokenStore() *tokenStore {
	return &tokenStore{shares: map[string]ShareToken{}}
}

func randomID(prefix string, bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic("relay: cryptographic randomness unavailable")
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

// Relay keys are bearer capabilities: the entire secret defines the namespace.
// Never let the public registration endpoint choose another user's identity.
func (s *tokenStore) register(_ string) APIKey {
	key := randomID("rw_", 24)
	result, _ := s.validate(key)
	return result
}

func secretHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *tokenStore) validate(key string) (APIKey, bool) {
	if !strings.HasPrefix(key, "rw_") || len(key) < 10 || len(key) > 512 {
		return APIKey{}, false
	}
	return APIKey{Key: key, UserID: "token-" + secretHash(key)}, true
}

func (s *tokenStore) createShare(userID, peerName, peerID, permissions string, ttl time.Duration) ShareToken {
	now := time.Now().UTC()
	share := ShareToken{ShareID: randomID("sh_", 12), UserID: userID, PeerName: peerName, PeerID: peerID, Permissions: permissions, CreatedAt: now}
	if ttl > 0 {
		expires := now.Add(ttl)
		share.ExpiresAt = &expires
	}
	s.mu.Lock()
	s.shares[share.ShareID] = share
	s.mu.Unlock()
	return share
}

func (s *tokenStore) share(id string) (ShareToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.shares[id]
	if ok && share.expired(time.Now().UTC()) {
		delete(s.shares, id)
		return ShareToken{}, false
	}
	return share, ok
}

func (s *tokenStore) listShares(userID string) []ShareToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	out := []ShareToken{}
	for id, share := range s.shares {
		if share.expired(now) {
			delete(s.shares, id)
			continue
		}
		if share.UserID == userID {
			out = append(out, share)
		}
	}
	return out
}

func (s *tokenStore) revokeShare(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.shares[id]; !ok {
		return false
	}
	delete(s.shares, id)
	return true
}

// address is how the relay names the shared peer to the daemon: its peer_id
// when known, else the name the share was made with.
func (t ShareToken) address() string {
	if t.PeerID != "" {
		return t.PeerID
	}
	return t.PeerName
}

// shows decides what a share viewer sees: events about the shared peer. The
// human's inbox is never part of a shared session. With a peer_id the match is
// by identity only, so a rename keeps the stream and a peer that later takes
// the old name never shows up in it; without one it is by name, as before.
func (t ShareToken) shows(event map[string]any) bool {
	if strings.HasPrefix(stringValue(event["type"]), "inbox_") {
		return false
	}
	keys, want := []string{"peer", "peer_name", "from", "from_peer", "to", "to_peer", "display_name"}, t.PeerName
	if t.PeerID != "" {
		keys, want = []string{"peer_id", "from_peer_id", "to_peer_id", "session_id", "from_peer", "to_peer"}, t.PeerID
	}
	for _, key := range keys {
		if stringValue(event[key]) == want {
			return true
		}
	}
	return false
}
