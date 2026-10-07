package relayserver

import (
	"net"
	"net/http"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// Bounded client-side buckets are checked before the global ceiling, so one
// noisy address cannot consume every registration slot. No timers or sweeper.
type oauthRegistrationLimit struct {
	mu                sync.Mutex
	clients           *lru.Cache[string, *rate.Limiter]
	global            *rate.Limiter
	trustCloudflareIP bool
}

func newOAuthRegistrationLimit() *oauthRegistrationLimit {
	clients, _ := lru.New[string, *rate.Limiter](4096)
	return &oauthRegistrationLimit{clients: clients, global: rate.NewLimiter(5, 20)}
}
func (l *oauthRegistrationLimit) allow(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Only explicitly configured, access-controlled proxies may supply identity.
	// Never trust a forwarded header merely because a caller sent one.
	if l.trustCloudflareIP {
		if ip := net.ParseIP(r.Header.Get("CF-Connecting-IP")); ip != nil {
			host = ip.String()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.clients.Get(host)
	if !ok {
		bucket = rate.NewLimiter(rate.Every(time.Minute), 5)
		l.clients.Add(host, bucket)
	}
	return bucket.Allow() && l.global.Allow()
}
