package relayserver

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func (f *authFixture) consentPage(t *testing.T, redirect string, headers map[string]string) (string, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(f.verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {f.clientID}, "redirect_uri": {redirect}, "scope": {oauthRead}, "resource": {f.s.oauth.issuer + "/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	res := f.request(t, "GET", "/oauth/authorize?"+q.Encode(), "", headers)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	match := regexp.MustCompile(`name="request" value="([^"]+)"`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("consent %d: %s", res.StatusCode, body)
	}
	for _, c := range res.Cookies() {
		if c.Name == "rw_oauth_csrf" {
			if c.MaxAge != 600 {
				t.Fatal("nonce not renewed")
			}
			return string(match[1]), c.Name + "=" + c.Value
		}
	}
	t.Fatal("missing renewed nonce cookie")
	return "", ""
}
func (f *authFixture) submitConsent(t *testing.T, sealed, cookie, decision string) *http.Response {
	return f.request(t, "POST", "/oauth/authorize", url.Values{"request": {sealed}, "decision": {decision}, "relay_secret": {f.key}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": cookie, "Origin": f.s.oauth.issuer})
}
func TestOAuthStatelessConsentAndReplay(t *testing.T) {
	f := newAuthFixture(t)
	var before int
	if err := f.s.oauth.store.db.QueryRow("SELECT count(*) FROM oauth_records").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var sealed, cookie string
	// Previously 1000 anonymous GETs locked every user out for ten minutes.
	for i := 0; i < 1005; i++ {
		sealed, cookie = f.consentPage(t, "https://client.example/callback", nil)
	}
	var after int
	_ = f.s.oauth.store.db.QueryRow("SELECT count(*) FROM oauth_records").Scan(&after)
	if before != after {
		t.Fatalf("anonymous GET grew state: %d -> %d", before, after)
	}
	renewed, renewedCookie := f.consentPage(t, "https://client.example/callback", map[string]string{"Cookie": cookie})
	if renewedCookie != cookie {
		t.Fatal("nonce rotation invalidated another open tab")
	}
	for _, decision := range []string{"deny", "deny"} {
		res := f.submitConsent(t, renewed, cookie, decision)
		res.Body.Close()
		if res.StatusCode != 303 {
			t.Fatal("deny failed")
		}
	}
	_ = f.s.oauth.store.db.QueryRow("SELECT count(*) FROM oauth_records").Scan(&after)
	if before != after {
		t.Fatal("anonymous denials grew state")
	}
	for _, bad := range []struct{ value, cookie string }{{sealed + "tampered", cookie}, {sealed, "rw_oauth_csrf=" + strings.Repeat("x", 43)}} {
		res := f.submitConsent(t, bad.value, bad.cookie, "allow")
		res.Body.Close()
		if res.StatusCode < 400 {
			t.Fatal("forged consent accepted")
		}
	}
	res := f.submitConsent(t, sealed, cookie, "allow")
	res.Body.Close()
	if res.StatusCode != 303 {
		t.Fatal("consent failed")
	}
	res = f.submitConsent(t, sealed, cookie, "allow")
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Fatal("consent replay minted another grant")
	}
	key, _ := f.s.tokens.validate(f.key)
	if err := f.s.oauth.store.view(func(d *oauthData) error {
		grants := d.Grants.list(key.UserID)
		if len(grants) != 1 || grants[0].Active || time.Until(grants[0].Expires) > time.Minute {
			t.Fatal("abandoned flow left an active long-lived grant")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestOAuthRegistrationCapacityRecoversAndReadsStayLocal(t *testing.T) {
	f := newAuthFixture(t)
	tokens := f.tokens(t, oauthRead)
	if err := f.s.oauth.store.update(func(d *oauthData) error {
		for i := 0; i < 4096; i++ {
			id := fmt.Sprintf("unused-%04d", i)
			d.Clients.put(id, storedOAuthClient{oauthClient: oauthClient{ID: id}, Expires: time.Now().Add(time.Hour + time.Duration(i+1)*time.Second)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := f.request(t, "POST", "/oauth/register", `{"redirect_uris":["https://new.example/callback"]}`, nil)
	data := jsonBody(t, res)
	if res.StatusCode != 201 {
		t.Fatalf("capacity permanently exhausted: %v", data)
	}
	if err := f.s.oauth.store.view(func(d *oauthData) error {
		if len(d.Clients.list("unlinked")) != 4096 {
			t.Fatal("unused registration limit not enforced")
		}
		if _, ok := d.Clients.get("unused-0000"); ok {
			t.Fatal("oldest unused client not evicted")
		}
		if _, ok := d.Clients.get(f.clientID); !ok {
			t.Fatal("active client evicted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// An unrelated corrupt registration must not even be decoded during access
	// validation. A trigger also proves it doesn't opportunistically rewrite state.
	_, err := f.s.oauth.store.db.Exec("UPDATE oauth_records SET data='broken JSON' WHERE kind='client' AND key='unused-0001'")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
		_, err = f.s.oauth.store.db.Exec("CREATE TRIGGER deny_" + action + " BEFORE " + action + " ON oauth_records BEGIN SELECT RAISE(FAIL,'read must not write'); END")
		if err != nil {
			t.Fatal(err)
		}
	}
	status, result := f.rpc(t, tokens["access_token"].(string), "tools/list", map[string]any{})
	if status != 200 || result["error"] != nil {
		t.Fatalf("access depended on unrelated state: %d %v", status, result)
	}
}
func TestOAuthLoopbackPortAndScopeNarrowing(t *testing.T) {
	f := newAuthFixture(t)
	res := f.request(t, "POST", "/oauth/register", `{"redirect_uris":["http://127.0.0.1:12345/callback?fixed=1"]}`, nil)
	f.clientID = jsonBody(t, res)["client_id"].(string)
	redirect := "http://127.0.0.1:23456/callback?fixed=1"
	sealed, cookie := f.consentPage(t, redirect, nil)
	res = f.submitConsent(t, sealed, cookie, "allow")
	res.Body.Close()
	location, _ := url.Parse(res.Header.Get("Location"))
	status, data := f.exchange(t, url.Values{"grant_type": {"authorization_code"}, "code": {location.Query().Get("code")}, "redirect_uri": {redirect}, "code_verifier": {f.verifier}})
	if status != 200 {
		t.Fatalf("cached registration with new loopback port failed: %v", data)
	}
	for _, bad := range []string{"http://localhost:23456/callback?fixed=1", "http://127.0.0.1:23456/evil?fixed=1", "http://127.0.0.1:23456/callback?fixed=2", "http://evil.example:23456/callback?fixed=1"} {
		if matchesRedirect([]string{redirect}, bad) {
			t.Fatalf("redirect widened to %s", bad)
		}
	}
	// Use the fixture's original HTTPS client for a separate read+write grant.
	res = f.request(t, "POST", "/oauth/register", `{"redirect_uris":["https://client.example/callback"]}`, nil)
	f.clientID = jsonBody(t, res)["client_id"].(string)
	tokens := f.tokens(t, oauthRead+" "+oauthWrite)
	status, data = f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "scope": {oauthRead}})
	if status != 200 || data["scope"] != oauthRead {
		t.Fatalf("narrowing failed: %v", data)
	}
	status, result := f.rpc(t, data["access_token"].(string), "tools/call", map[string]any{"name": "ask_agent", "arguments": map[string]any{"daemon_id": "laptop", "peer_id": "p", "message": "hi"}})
	if status != 200 || result["result"].(map[string]any)["isError"] != true {
		t.Fatal("narrowed access retained write permission")
	}
	status, _ = f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {data["refresh_token"].(string)}, "scope": {oauthRead + " " + oauthWrite}})
	if status == 200 {
		t.Fatal("refresh widened scope again")
	}
}

func TestOAuthUnusedClientExpiryAndRedirectBudget(t *testing.T) {
	f := newAuthFixture(t)
	err := f.s.oauth.store.update(func(d *oauthData) error {
		c, _ := d.Clients.get(f.clientID)
		c.Expires = time.Now().Add(-time.Second)
		d.Clients.put(c.ID, c)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.oauth.store.view(func(d *oauthData) error {
		if _, ok := d.Clients.get(f.clientID); ok {
			t.Fatal("expired unused registration accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	uri := "https://client.example/" + strings.Repeat("x", 1400)
	res := f.request(t, "POST", "/oauth/register", fmt.Sprintf(`{"redirect_uris":[%q,%q,%q]}`, uri, uri, uri), nil)
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal("aggregate redirect budget not enforced")
	}
}

func TestOAuthRegistrationRateFairnessAndProxyTrust(t *testing.T) {
	limit := newOAuthRegistrationLimit()
	request := func(remote, forwarded string) *http.Request {
		r, _ := http.NewRequest("POST", "https://relay.example/oauth/register", nil)
		r.RemoteAddr = remote
		r.Header.Set("CF-Connecting-IP", forwarded)
		return r
	}
	for i := 0; i < 5; i++ {
		if !limit.allow(request("192.0.2.1:1000", "")) {
			t.Fatal("burst rejected")
		}
	}
	for i := 0; i < 100; i++ {
		if limit.allow(request("192.0.2.1:1001", fmt.Sprintf("198.51.100.%d", i))) {
			t.Fatal("untrusted header bypassed per-IP limit")
		}
	}
	if !limit.allow(request("192.0.2.2:1000", "")) {
		t.Fatal("attacker exhausted another client's budget")
	}
	limit = newOAuthRegistrationLimit()
	limit.trustCloudflareIP = true
	for i := 0; i < 5; i++ {
		if !limit.allow(request("192.0.2.1:1000", "198.51.100.1")) {
			t.Fatal("trusted proxy burst failed")
		}
	}
	if limit.allow(request("192.0.2.1:1000", "198.51.100.1")) {
		t.Fatal("trusted client limit failed")
	}
	if !limit.allow(request("192.0.2.1:1000", "198.51.100.2")) {
		t.Fatal("trusted proxy clients share bucket")
	}
}
func TestOAuthCachedClientOutlivesGrantAndExpiredRecovery(t *testing.T) {
	f := newAuthFixture(t)
	f.tokens(t, oauthRead)
	key, _ := f.s.tokens.validate(f.key)
	err := f.s.oauth.store.update(func(d *oauthData) error {
		c, ok := d.Clients.get(f.clientID)
		if !ok || time.Until(c.Expires) < 89*24*time.Hour {
			t.Fatal("cached linked client expires too early")
		}
		for _, g := range d.Grants.list(key.UserID) {
			g.Expires = time.Now().Add(-time.Second)
			d.Grants.put(g.ID, g)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same cached client ID obtains fresh consent after its grant has expired.
	f.tokens(t, oauthRead)
	err = f.s.oauth.store.update(func(d *oauthData) error {
		c, _ := d.Clients.get(f.clientID)
		c.Expires = time.Now().Add(-time.Second)
		d.Clients.put(c.ID, c)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(f.verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {f.clientID}, "redirect_uri": {"https://client.example/callback"}, "resource": {f.s.oauth.issuer + "/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	res := f.request(t, "GET", "/oauth/authorize?"+q.Encode(), "", nil)
	data := jsonBody(t, res)
	if res.StatusCode != 400 || !strings.Contains(data["error_description"].(string), "Remove and re-add") {
		t.Fatal("expired registration lacks actionable recovery")
	}
	res = f.request(t, "POST", "/oauth/register", `{"redirect_uris":["https://client.example/callback"]}`, nil)
	f.clientID = jsonBody(t, res)["client_id"].(string)
	f.tokens(t, oauthRead)
}
