package relayserver

import (
	"fmt"
	"golang.org/x/time/rate"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func proxyRequest(remote string, xff, cf []string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "https://relay.example/oauth/register", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	for _, v := range cf {
		r.Header.Add("CF-Connecting-IP", v)
	}
	return r
}
func TestOAuthRegistrationClientIPTrustChain(t *testing.T) {
	lb := netip.MustParseAddr("192.0.2.10")
	tests := []struct {
		name, mode, remote string
		xff, cf            []string
		want               string
	}{
		{"default ignores headers", "", "127.0.0.1:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8"}, "127.0.0.1"},
		{"direct ignores even GFE headers", "direct", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8"}, "35.191.1.2"},
		{"untrusted socket cannot forge suffix", "gclb", "198.51.100.9:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8"}, "198.51.100.9"},
		{"CF through GFE", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8"}, "198.51.100.8"},
		{"direct origin ignores forged CF", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 198.51.100.9, 192.0.2.10"}, []string{"198.51.100.8"}, "198.51.100.9"},
		{"prefix junk ignored", "gclb", "130.211.1.2:1234", []string{"unknown, nonsense:443, 172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8"}, "198.51.100.8"},
		{"repeated XFF lines", "gclb", "35.191.1.2:1234", []string{"forged, 203.0.113.8", " 172.64.1.2 , 192.0.2.10 "}, []string{"198.51.100.8"}, "198.51.100.8"},
		{"IPv6 chain", "gclb", "[2600:2d00:1:1::8]:1234", []string{"2606:4700::8, 192.0.2.10"}, []string{"2001:db8:1::9"}, "2001:db8:1::9"},
		{"IPv4 mapped normalization", "gclb", "[::ffff:35.191.1.2]:1234", []string{"::ffff:172.64.1.2, 192.0.2.10"}, []string{"::ffff:198.51.100.8"}, "198.51.100.8"},
		{"wrong forwarding rule", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.11"}, []string{"198.51.100.8"}, "35.191.1.2"},
		{"GFE health check no XFF", "gclb", "35.191.1.2:1234", nil, nil, "35.191.1.2"},
		{"short XFF", "gclb", "35.191.1.2:1234", []string{"172.64.1.2"}, []string{"198.51.100.8"}, "35.191.1.2"},
		{"malformed source", "gclb", "35.191.1.2:1234", []string{"172.64.1.2:443, 192.0.2.10"}, []string{"198.51.100.8"}, "35.191.1.2"},
		{"malformed trailing entry", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, garbage"}, []string{"198.51.100.8"}, "35.191.1.2"},
		{"missing CF header", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, nil, "172.64.1.2"},
		{"multiple CF values", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8", "198.51.100.9"}, "172.64.1.2"},
		{"comma CF value", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"198.51.100.8, 198.51.100.9"}, "172.64.1.2"},
		{"zoned CF address rejected", "gclb", "35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"fe80::1%eth0"}, "172.64.1.2"},
		{"sidecar fails to socket", "gclb", "127.0.0.1:1234", []string{"172.64.1.2, 192.0.2.10, 35.191.1.2"}, []string{"198.51.100.8"}, "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registrationClientIP(proxyRequest(tt.remote, tt.xff, tt.cf), tt.mode, lb)
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
func TestOAuthProxyConfigurationAndIPv6Budgets(t *testing.T) {
	for _, tt := range []struct {
		mode, ip string
		valid    bool
	}{{"", "", true}, {"direct", "", true}, {"gclb", "192.0.2.10", true}, {"gclb", "", false}, {"gclb", "garbage", false}, {"gclb", "192.0.2.10:443", false}, {"anything", "", false}} {
		l := newOAuthRegistrationLimit()
		if err := l.configureProxy(tt.mode, tt.ip); (err == nil) != tt.valid {
			t.Fatalf("mode=%q ip=%q err=%v", tt.mode, tt.ip, err)
		}
	}
	l := newOAuthRegistrationLimit()
	if err := l.configureProxy("gclb", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		r := proxyRequest("35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{fmt.Sprintf("2001:db8:1::%x", i+1)})
		if got := l.allow(r); got != (i < 5) {
			t.Fatalf("IPv6 rotation bypassed network budget at %d", i)
		}
	}
	if !l.allow(proxyRequest("35.191.1.2:1234", []string{"172.64.1.2, 192.0.2.10"}, []string{"2001:db8:2::1"})) {
		t.Fatal("unrelated IPv6 network shared bucket")
	}
	// A direct-origin caller cannot reset its bucket by rotating a forged CF header.
	l = newOAuthRegistrationLimit()
	_ = l.configureProxy("gclb", "192.0.2.10")
	for i := 0; i < 6; i++ {
		if got := l.allow(proxyRequest("35.191.1.2:1234", []string{"198.51.100.9, 192.0.2.10"}, []string{fmt.Sprintf("203.0.113.%d", i+1)})); got != (i < 5) {
			t.Fatal("direct origin spoof reset bucket")
		}
	}
}

func TestOAuthProxyRegistrationHTTPBudgets(t *testing.T) {
	f := newAuthFixture(t)
	// Simulate the trusted proxy's socket while exercising the actual HTTP routes.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "35.191.1.2:1234"
		f.s.Handler().ServeHTTP(w, r)
	}))
	defer proxy.Close()
	if err := f.s.oauth.registrationLimit.configureProxy("gclb", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	post := func(xff, cf string) int {
		r, _ := http.NewRequest("POST", proxy.URL+"/oauth/register", strings.NewReader(`{"redirect_uris":["https://client.example/callback"]}`))
		r.Header.Set("X-Forwarded-For", xff)
		r.Header.Set("CF-Connecting-IP", cf)
		res, err := f.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	for i := 0; i < 6; i++ {
		want := 201
		if i == 5 {
			want = 429
		}
		if got := post("172.64.1.2, 192.0.2.10", "198.51.100.8"); got != want {
			t.Fatalf("CF registration %d: %d", i, got)
		}
	}
	if got := post("172.64.1.2, 192.0.2.10", "198.51.100.9"); got != 201 {
		t.Fatal("second visitor behind same edge blocked")
	}
	for i := 0; i < 6; i++ {
		want := 201
		if i == 5 {
			want = 429
		}
		if got := post("203.0.113.9, 192.0.2.10", fmt.Sprintf("198.51.100.%d", i+1)); got != want {
			t.Fatalf("direct-origin spoof %d: %d", i, got)
		}
	}
	// Neither missing proxy headers nor exhausted registration buckets affect health.
	res, err := http.Get(proxy.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("registration proxy configuration affected health")
	}
}

func TestOAuthGlobalLimitPreservesClientBudget(t *testing.T) {
	l := newOAuthRegistrationLimit()
	l.global = rate.NewLimiter(0, 0)
	r := proxyRequest("198.51.100.8:1234", nil, nil)
	for i := 0; i < 10; i++ {
		if l.allow(r) {
			t.Fatal("global limit bypassed")
		}
	}
	l.global = rate.NewLimiter(5, 20)
	for i := 0; i < 6; i++ {
		if got := l.allow(r); got != (i < 5) {
			t.Fatal("global denial consumed client budget")
		}
	}
}
