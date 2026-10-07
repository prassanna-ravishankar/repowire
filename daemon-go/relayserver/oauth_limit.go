package relayserver

import (
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// Bounded client-side buckets are checked before the global ceiling, so one
// noisy address cannot consume every registration slot. No timers or sweeper.
type oauthRegistrationLimit struct {
	mu               sync.Mutex
	clients          *lru.Cache[string, *rate.Limiter]
	global           *rate.Limiter
	proxyMode        string
	gclbForwardingIP netip.Addr
}

func newOAuthRegistrationLimit() *oauthRegistrationLimit {
	clients, _ := lru.New[string, *rate.Limiter](4096)
	return &oauthRegistrationLimit{clients: clients, global: rate.NewLimiter(5, 20)}
}
func (l *oauthRegistrationLimit) allow(r *http.Request) bool {
	host := registrationClientIP(r, l.proxyMode, l.gclbForwardingIP)
	// Keep IPv6 privacy-address rotation within one network bucket.
	if ip, ok := plainIP(host); ok && ip.Is6() {
		host = netip.PrefixFrom(ip, 64).Masked().String()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.clients.Get(host)
	if !ok {
		bucket = rate.NewLimiter(rate.Every(time.Minute), 5)
		l.clients.Add(host, bucket)
	}
	now := time.Now()
	reservation := bucket.ReserveN(now, 1)
	if !reservation.OK() || reservation.DelayFrom(now) > 0 {
		reservation.CancelAt(now)
		return false
	}
	if !l.global.AllowN(now, 1) {
		reservation.CancelAt(now)
		return false
	}
	return true
}

func (l *oauthRegistrationLimit) configureProxy(mode, forwardingIP string) error {
	if mode == "" {
		mode = "direct"
	}
	if mode != "direct" && mode != "gclb" {
		return fmt.Errorf("REPOWIRE_RELAY_OAUTH_PROXY_MODE must be direct or gclb")
	}
	if mode == "gclb" {
		ip, ok := plainIP(forwardingIP)
		if !ok {
			return fmt.Errorf("gclb OAuth proxy mode requires REPOWIRE_RELAY_OAUTH_GCLB_FORWARDING_IP")
		}
		l.gclbForwardingIP = ip
	}
	l.proxyMode = mode
	return nil
}
