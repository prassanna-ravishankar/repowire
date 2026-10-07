package relayserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type authFixture struct {
	s                           *Server
	http                        *httptest.Server
	client                      *http.Client
	key, clientID, verifier, db string
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	s := New("")
	hs := httptest.NewUnstartedServer(s.Handler())
	issuer := "http://" + hs.Listener.Addr().String()
	db := filepath.Join(t.TempDir(), "oauth.db")
	if err := s.EnableMCP(issuer, db); err != nil {
		t.Fatal(err)
	}
	hs.Start()
	t.Cleanup(func() { hs.Close(); _ = s.Close() })
	f := &authFixture{s: s, http: hs, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, key: "rw_fixture-secret-with-entropy", verifier: strings.Repeat("v", 43), db: db}
	key, _ := s.tokens.validate(f.key)
	s.registerConn(&daemonConn{userID: key.UserID, daemonID: "laptop", connectedAt: time.Now()})
	res := f.request(t, "POST", "/oauth/register", `{"client_name":"Test client","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none"}`, nil)
	f.clientID = jsonBody(t, res)["client_id"].(string)
	return f
}
func (f *authFixture) request(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, e := http.NewRequest(method, f.http.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, e := f.client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	return res
}
func jsonBody(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		t.Fatalf("status %d body %s", r.StatusCode, b)
	}
	return m
}
func (f *authFixture) authorize(t *testing.T, scope string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(f.verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {f.clientID}, "redirect_uri": {"https://client.example/callback"}, "scope": {scope}, "resource": {f.s.oauth.issuer + "/mcp"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "state": {"client-state"}}
	res := f.request(t, "GET", "/oauth/authorize?"+q.Encode(), "", nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	match := regexp.MustCompile(`name="request" value="([^"]+)"`).FindSubmatch(b)
	if len(match) != 2 {
		t.Fatalf("consent %d: %s", res.StatusCode, b)
	}
	form := url.Values{"request": {string(match[1])}, "decision": {"allow"}, "relay_secret": {f.key}}
	cookie := ""
	for _, c := range res.Cookies() {
		if c.Name == "rw_oauth_csrf" {
			cookie = c.Name + "=" + c.Value
		}
	}
	res = f.request(t, "POST", "/oauth/authorize", form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": cookie, "Origin": f.s.oauth.issuer})
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 303 || loc.Query().Get("code") == "" {
		t.Fatalf("consent failed %d %s", res.StatusCode, loc)
	}
	if loc.Query().Get("state") != "client-state" || loc.Query().Get("iss") != f.s.oauth.issuer {
		t.Fatal("lost state or issuer")
	}
	return loc.Query().Get("code")
}
func (f *authFixture) exchange(t *testing.T, values url.Values) (int, map[string]any) {
	t.Helper()
	values.Set("client_id", f.clientID)
	values.Set("resource", f.s.oauth.issuer+"/mcp")
	res := f.request(t, "POST", "/oauth/token", values.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	return res.StatusCode, jsonBody(t, res)
}
func (f *authFixture) tokens(t *testing.T, scope string) map[string]any {
	t.Helper()
	code := f.authorize(t, scope)
	status, data := f.exchange(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://client.example/callback"}, "code_verifier": {f.verifier}})
	if status != 200 {
		t.Fatalf("exchange %d: %v", status, data)
	}
	return data
}
func (f *authFixture) rpc(t *testing.T, token, method string, params any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	res := f.request(t, "POST", "/mcp", string(b), map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + token})
	return res.StatusCode, jsonBody(t, res)
}
func TestOAuthLifecycleAndRestart(t *testing.T) {
	f := newAuthFixture(t)
	tokens := f.tokens(t, oauthRead+" "+oauthWrite)
	access := tokens["access_token"].(string)
	refresh := tokens["refresh_token"].(string)
	status, init := f.rpc(t, access, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	if status != 200 || init["error"] != nil {
		t.Fatalf("initialize %d %v", status, init)
	}
	status, tools := f.rpc(t, access, "tools/list", map[string]any{})
	if status != 200 || len(tools["result"].(map[string]any)["tools"].([]any)) != 6 {
		t.Fatalf("tools %v", tools)
	}
	if e := f.s.oauth.store.db.Close(); e != nil {
		t.Fatal(e)
	}
	store, e := openOAuthStore(f.db)
	if e != nil {
		t.Fatal(e)
	}
	f.s.oauth.store = store
	status, next := f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != 200 || next["refresh_token"] == refresh {
		t.Fatalf("refresh %d %v", status, next)
	}
	status, _ = f.rpc(t, next["access_token"].(string), "tools/list", map[string]any{})
	if status != 200 {
		t.Fatal("refreshed token failed")
	}
	status, _ = f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != 400 {
		t.Fatal("refresh replay accepted")
	}
	for _, token := range []string{access, next["access_token"].(string)} {
		status, _ = f.rpc(t, token, "tools/list", map[string]any{})
		if status != 401 {
			t.Fatal("replay did not revoke family")
		}
	}
	var raw string
	if e := store.db.QueryRow("SELECT group_concat(data) FROM oauth_records").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{f.key, refresh, access, next["refresh_token"].(string)} {
		if strings.Contains(raw, secret) {
			t.Fatal("plaintext credential persisted")
		}
	}
}
func TestOAuthPKCEClientResourceAndScope(t *testing.T) {
	f := newAuthFixture(t)
	code := f.authorize(t, oauthRead)
	status, _ := f.exchange(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://client.example/callback"}, "code_verifier": {strings.Repeat("x", 43)}})
	if status == 200 {
		t.Fatal("bad verifier accepted")
	}
	tokens := f.tokens(t, oauthRead)
	status, result := f.rpc(t, tokens["access_token"].(string), "tools/call", map[string]any{"name": "ask_agent", "arguments": map[string]any{"daemon_id": "laptop", "peer_id": "p", "message": "hi"}})
	if status != 200 || result["result"].(map[string]any)["isError"] != true {
		t.Fatalf("write scope bypass: %v", result)
	}
	oldClient := f.clientID
	f.clientID = "wrong-client"
	status, _ = f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}})
	if status == 200 {
		t.Fatal("cross-client refresh accepted")
	}
	f.clientID = oldClient
	res := f.request(t, "POST", "/oauth/token", url.Values{"client_id": {f.clientID}, "grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "resource": {"https://wrong.example/mcp"}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal("wrong resource accepted")
	}
	status, _ = f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "scope": {oauthRead + " " + oauthWrite}})
	if status == 200 {
		t.Fatal("scope escalation accepted")
	}
}
func TestOAuthConcurrentRotationAndRevocation(t *testing.T) {
	f := newAuthFixture(t)
	tokens := f.tokens(t, oauthRead)
	refresh := tokens["refresh_token"].(string)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
			statuses <- status
		}()
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == 200 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent rotation successes %d", successes)
	}
	fresh := f.tokens(t, oauthRead)
	res := f.request(t, "POST", "/oauth/revoke", url.Values{"client_id": {f.clientID}, "token": {fresh["refresh_token"].(string)}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	status, _ := f.rpc(t, fresh["access_token"].(string), "tools/list", map[string]any{})
	if status != 401 {
		t.Fatal("revoked access accepted")
	}
}
func TestFullRelaySecretIdentity(t *testing.T) {
	s := newTokenStore()
	a, _ := s.validate("rw_alpha_same-tail")
	b, _ := s.validate("rw_bravo_same-tail")
	if a.UserID == b.UserID {
		t.Fatal("suffix collision")
	}
	again, _ := newTokenStore().validate(a.Key)
	if again.UserID != a.UserID {
		t.Fatal("identity changes on restart")
	}
	registered := s.register(a.UserID)
	if registered.UserID == a.UserID {
		t.Fatal("public registration impersonates principal")
	}
}
func TestOAuthCSRFAndRedirectValidation(t *testing.T) {
	f := newAuthFixture(t)
	for _, uri := range []string{"https://client.example/callback#fragment", "https://user:pass@client.example/callback", "http://remote.example/callback"} {
		b, _ := json.Marshal(map[string]any{"redirect_uris": []string{uri}})
		res := f.request(t, "POST", "/oauth/register", string(b), nil)
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("unsafe redirect %s", uri)
		}
	}
	res := f.request(t, "POST", "/oauth/authorize", url.Values{"decision": {"allow"}, "relay_secret": {f.key}, "request": {"forged"}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Fatal("missing CSRF accepted")
	}
	// Metadata must use the configured issuer, never Host or forwarded headers.
	res = f.request(t, "GET", "/.well-known/oauth-authorization-server", "", map[string]string{"X-Forwarded-Host": "evil.example"})
	data := jsonBody(t, res)
	if data["issuer"] != f.s.oauth.issuer {
		t.Fatal("issuer spoofed")
	}
	res = f.request(t, "POST", "/mcp", `{}`, nil)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 401 || !strings.Contains(res.Header.Get("WWW-Authenticate"), "resource_metadata") || bytes.Contains(b, []byte(f.key)) {
		t.Fatal("bad auth challenge")
	}
}

func TestOAuthExpiryOfflineRefreshAndStorageFailure(t *testing.T) {
	f := newAuthFixture(t)
	tokens := f.tokens(t, oauthRead)
	access, refresh := tokens["access_token"].(string), tokens["refresh_token"].(string)
	err := f.s.oauth.store.update(func(d *oauthData) error {
		token, _ := d.Access.get(secretHash(access))
		token.AccessCreateAt = time.Now().Add(-time.Hour)
		d.Access.put(secretHash(access), token)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ := f.rpc(t, access, "tools/list", map[string]any{})
	if status != 401 {
		t.Fatal("expired access accepted")
	}
	key, _ := f.s.tokens.validate(f.key)
	f.s.unregisterConn(f.s.daemon(key.UserID, "laptop"))
	status, next := f.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != 200 {
		t.Fatalf("offline refresh failed: %v", next)
	}
	err = f.s.oauth.store.update(func(d *oauthData) error {
		for _, g := range d.Grants.list(key.UserID) {
			g.Expires = time.Now().Add(-time.Second)
			d.Grants.put(g.ID, g)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ = f.rpc(t, next["access_token"].(string), "tools/list", map[string]any{})
	if status != 401 {
		t.Fatal("expired grant accepted")
	}
	_ = f.s.oauth.store.db.Close()
	status, _ = f.rpc(t, next["access_token"].(string), "tools/list", map[string]any{})
	if status != 503 {
		t.Fatalf("storage failure status %d", status)
	}
}
func TestOAuthStateBindsIssuer(t *testing.T) {
	f := newAuthFixture(t)
	s := New("")
	if err := s.EnableMCP("https://other.example", f.db); err == nil {
		_ = s.Close()
		t.Fatal("state reused with different issuer")
	}
}
