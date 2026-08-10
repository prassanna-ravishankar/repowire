package relayserver

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

type APIKey struct {
	Key    string `json:"api_key"`
	UserID string `json:"user_id"`
}

type ShareToken struct {
	ShareID     string     `json:"share_id"`
	UserID      string     `json:"-"`
	PeerName    string     `json:"peer_name"`
	Permissions string     `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

func (t ShareToken) expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

type tokenStore struct {
	mu     sync.Mutex
	keys   map[string]APIKey
	shares map[string]ShareToken
}

func newTokenStore() *tokenStore {
	return &tokenStore{keys: map[string]APIKey{}, shares: map[string]ShareToken{}}
}

func randomID(prefix string, bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func (s *tokenStore) register(userID string) APIKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range s.keys {
		if key.UserID == userID {
			return key
		}
	}
	key := APIKey{Key: randomID("rw_", 24), UserID: userID}
	s.keys[key.Key] = key
	return key
}

func (s *tokenStore) validate(key string) (APIKey, bool) {
	if !strings.HasPrefix(key, "rw_") || len(key) < 10 {
		return APIKey{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if found, ok := s.keys[key]; ok {
		return found, true
	}
	userID := "token-" + key[len(key)-8:]
	found := APIKey{Key: key, UserID: userID}
	s.keys[key] = found
	return found, true
}

func (s *tokenStore) createShare(userID, peerName, permissions string, ttl time.Duration) ShareToken {
	now := time.Now().UTC()
	share := ShareToken{ShareID: randomID("sh_", 12), UserID: userID, PeerName: peerName, Permissions: permissions, CreatedAt: now}
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
