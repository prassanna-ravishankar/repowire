package relayserver

import (
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-oauth2/oauth2/v4"
	"github.com/gorilla/securecookie"
)

var errInvalidAccess = errors.New("invalid access token")

const oauthRead = "agents:read"
const oauthWrite = "agents:write"
const accessLifetime = 15 * time.Minute
const grantLifetime = 30 * 24 * time.Hour

type relayOAuth struct {
	issuer            string
	store             *oauthStore
	registrationLimit *oauthRegistrationLimit
	consent           *securecookie.SecureCookie
}

// EnableMCP must run before serving. A stable externally visible issuer and
// durable file are explicit deployment requirements; New leaves MCP disabled.
func (s *Server) EnableMCP(issuer, statePath string) error {
	issuer = strings.TrimRight(issuer, "/")
	u, err := url.Parse(issuer)
	if err != nil || !validHTTPSOrLoopback(u) || u.Path != "" || u.RawQuery != "" {
		return fmt.Errorf("OAuth issuer must be an HTTPS origin (HTTP loopback allowed for development)")
	}
	if statePath == "" {
		return fmt.Errorf("OAuth state path is required")
	}
	store, err := openOAuthStore(statePath)
	if err != nil {
		return err
	}
	var sealKey string
	if err := store.update(func(d *oauthData) error {
		var previous string
		err := d.tx.QueryRow("SELECT value FROM oauth_settings WHERE key='issuer'").Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previous != "" && previous != issuer {
			return fmt.Errorf("OAuth database belongs to a different issuer")
		}
		if _, err = d.tx.Exec("INSERT OR IGNORE INTO oauth_settings(key,value) VALUES('issuer',?)", issuer); err != nil {
			return err
		}
		err = d.tx.QueryRow("SELECT value FROM oauth_settings WHERE key='consent_key'").Scan(&sealKey)
		if errors.Is(err, sql.ErrNoRows) {
			sealKey = randomID("", 32)
			_, err = d.tx.Exec("INSERT INTO oauth_settings(key,value) VALUES('consent_key',?)", sealKey)
		}
		return err
	}); err != nil {
		_ = store.db.Close()
		return err
	}
	codec := securecookie.New([]byte(sealKey), nil).MaxAge(600).MaxLength(12 << 10).SetSerializer(securecookie.JSONEncoder{})
	s.oauth = &relayOAuth{issuer: issuer, store: store, registrationLimit: newOAuthRegistrationLimit(), consent: codec}
	s.oauthRoutes()
	s.registerRelayMCP()
	return nil
}
func validHTTPSOrLoopback(u *url.URL) bool {
	if u == nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())))
}
func (s *Server) Close() error {
	if s.oauth != nil {
		return s.oauth.store.db.Close()
	}
	return nil
}
func (s *Server) oauthRoutes() {
	s.mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.oauthResource)
	s.mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.oauthResource)
	s.mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.oauthMetadata)
	s.mux.HandleFunc("POST /oauth/register", s.oauthRegister)
	s.mux.HandleFunc("GET /oauth/authorize", s.oauthAuthorize)
	s.mux.HandleFunc("POST /oauth/authorize", s.oauthConsent)
	s.mux.HandleFunc("POST /oauth/token", s.oauthExchange)
	s.mux.HandleFunc("POST /oauth/revoke", s.oauthRevoke)
	s.mux.HandleFunc("GET /oauth/connections", s.oauthConnections)
	s.mux.HandleFunc("POST /oauth/connections", s.oauthDisconnect)
}
func oauthHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	// no-referrer turns same-origin form POSTs into Origin:null in browsers.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// form-action is intentionally not restricted to self: a successful OAuth
	// POST redirects to a separately validated external client callback.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
}
func oauthError(w http.ResponseWriter, status int, code, description string) {
	oauthHeaders(w)
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
func (s *Server) oauthResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"resource": s.oauth.issuer + "/mcp", "authorization_servers": []string{s.oauth.issuer}, "scopes_supported": []string{oauthRead, oauthWrite}})
}
func (s *Server) oauthMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.oauth.issuer
	writeJSON(w, 200, map[string]any{"issuer": base, "authorization_endpoint": base + "/oauth/authorize", "token_endpoint": base + "/oauth/token", "registration_endpoint": base + "/oauth/register", "revocation_endpoint": base + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "revocation_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{oauthRead, oauthWrite}, "authorization_response_iss_parameter_supported": true})
}
func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.oauth.registrationLimit.allow(r) {
		w.Header().Set("Retry-After", "60")
		oauthError(w, 429, "temporarily_unavailable", "Too many client registrations")
		return
	}
	oauthHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var c oauthClient
	if json.NewDecoder(r.Body).Decode(&c) != nil || len(c.Redirects) == 0 || len(c.Redirects) > 8 || len(c.Name) > 120 || (c.Method != "" && c.Method != "none") {
		oauthError(w, 400, "invalid_client_metadata", "Public clients with redirect_uris are required")
		return
	}
	redirectBytes := 0
	for _, raw := range c.Redirects {
		redirectBytes += len(raw)
		u, e := url.Parse(raw)
		if e != nil || !validHTTPSOrLoopback(u) || len(raw) > 2048 || redirectBytes > 4096 {
			oauthError(w, 400, "invalid_redirect_uri", "Use HTTPS or a loopback HTTP callback without fragments or credentials")
			return
		}
	}
	c.ID = randomID("rwc_", 24)
	c.Method = "none"
	if c.Name == "" {
		c.Name = "MCP client"
	}
	err := s.oauth.store.update(func(d *oauthData) error {
		// Unused registrations expire after a day. Evict the oldest unlinked
		// registrations at capacity; anonymous clients cannot permanently fill it.
		_, d.err = d.tx.Exec(`DELETE FROM oauth_records WHERE kind='client' AND owner='unlinked' AND key IN (SELECT key FROM oauth_records WHERE kind='client' AND owner='unlinked' ORDER BY expires DESC LIMIT -1 OFFSET 4095)`)
		d.Clients.put(c.ID, storedOAuthClient{oauthClient: c, Expires: time.Now().Add(24 * time.Hour)})
		return nil
	})
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable", "Client registration unavailable")
		return
	}
	writeJSON(w, 201, c)
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func validScopes(scope string) bool {
	parts := strings.Fields(scope)
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if p != oauthRead && p != oauthWrite {
			return false
		}
	}
	return contains(parts, oauthRead)
}
func parseOAuthForm(w http.ResponseWriter, r *http.Request) bool {
	oauthHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.URL.RawQuery != "" || r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request", "Invalid form")
		return false
	}
	return true
}
func (s *Server) browserNonce(w http.ResponseWriter, r *http.Request) string {
	nonce := ""
	if c, e := r.Cookie("rw_oauth_csrf"); e == nil && len(c.Value) == 43 {
		nonce = c.Value
	}
	if nonce == "" {
		nonce = randomID("", 32)
	}
	// Renew the same nonce so other open consent tabs remain valid.
	http.SetCookie(w, &http.Cookie{Name: "rw_oauth_csrf", Value: nonce, Path: "/oauth", HttpOnly: true, Secure: strings.HasPrefix(s.oauth.issuer, "https:"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	return nonce
}
func browserProof(r *http.Request, hash string) bool {
	c, e := r.Cookie("rw_oauth_csrf")
	if e != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secretHash(c.Value)), []byte(hash)) == 1
}
func (s *Server) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == "" || r.Header.Get("Origin") == s.oauth.issuer
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Connect Repowire</title><style>body{font:17px system-ui;max-width:520px;margin:8vh auto;padding:24px;color:#19283a;background:#f6f8fb}input,button{font:inherit;padding:12px;margin:8px 0}input{box-sizing:border-box;width:100%}button{cursor:pointer}small{color:#536275}</style></head><body><h1>Connect to Repowire</h1><p><strong>{{.Name}}</strong> requests access to your relay-connected machines.</p><p>Read agents and replies{{if .Write}}; send messages and work requests{{end}}.</p><p><small>Client-provided name. Callback: {{.Redirect}}</small></p><form method="post" action="/oauth/authorize"><input type="hidden" name="request" value="{{.Request}}">{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}{{if .LoggedIn}}<p>You are signed in to the relay.</p>{{else}}<label>Relay secret<input type="password" name="relay_secret" autocomplete="current-password" placeholder="rw_…" required></label>{{end}}<p>Access lasts up to 30 days. You can revoke it from Connected apps. Allowing access also signs this browser into your relay dashboard for 30 days.</p><button name="decision" value="allow">Allow access</button> <button name="decision" value="deny" formnovalidate>Cancel</button></form></body></html>`))

func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	oauthHeaders(w)
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		scope = oauthRead
	}
	challenge, e := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if q.Get("response_type") != "code" || q.Get("resource") != s.oauth.issuer+"/mcp" || q.Get("code_challenge_method") != "S256" || e != nil || len(challenge) != 32 || !validScopes(scope) || len(q.Get("state")) > 2048 {
		oauthError(w, 400, "invalid_request", "Require code, S256 PKCE, relay MCP resource and supported scopes")
		return
	}
	nonce := s.browserNonce(w, r)
	var c storedOAuthClient
	err := s.oauth.store.view(func(d *oauthData) error {
		var ok bool
		c, ok = d.Clients.get(q.Get("client_id"))
		if !ok {
			return fmt.Errorf("Client registration expired or is unknown. Remove and re-add this connector in your MCP client, then reconnect")
		}
		if !matchesRedirect(c.Redirects, q.Get("redirect_uri")) {
			return fmt.Errorf("Redirect URI is not registered for this client")
		}
		return nil
	})
	if err != nil {
		oauthError(w, 400, "invalid_request", err.Error())
		return
	}
	p := oauthPending{ID: randomID("", 32), ClientID: c.ID, Redirect: q.Get("redirect_uri"), Challenge: q.Get("code_challenge"), Scope: scope, State: q.Get("state"), Browser: secretHash(nonce), Expires: time.Now().Add(10 * time.Minute)}
	id, err := s.oauth.consent.Encode("consent", p)
	if err != nil {
		oauthError(w, 500, "server_error", "Cannot prepare consent")
		return
	}
	_, logged := s.cookieKey(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentTemplate.Execute(w, map[string]any{"Name": c.Name, "Redirect": q.Get("redirect_uri"), "Request": id, "LoggedIn": logged, "Write": contains(strings.Fields(scope), oauthWrite)})
}
func (s *Server) oauthConsent(w http.ResponseWriter, r *http.Request) {
	if !parseOAuthForm(w, r) {
		return
	}
	if !s.sameOrigin(r) {
		oauthError(w, 403, "access_denied", "Invalid origin")
		return
	}
	key, ok := s.cookieKey(r)
	if secret := r.PostForm.Get("relay_secret"); secret != "" {
		key, ok = s.tokens.validate(secret)
	}
	decision := r.PostForm.Get("decision")
	if decision != "allow" && decision != "deny" {
		oauthError(w, 400, "invalid_request", "Choose allow or deny")
		return
	}
	var p oauthPending
	if s.oauth.consent.Decode("consent", r.PostForm.Get("request"), &p) != nil || !time.Now().Before(p.Expires) || !browserProof(r, p.Browser) {
		oauthError(w, 400, "invalid_request", "Authorization expired or browser session changed")
		return
	}
	// An arbitrary well-shaped key is a namespace, not proof of an existing mesh.
	// First authorization requires the daemon holding that exact secret online.
	if decision == "allow" && (!ok || s.anyDaemon(key.UserID) == nil) {
		var client storedOAuthClient
		err := s.oauth.store.view(func(d *oauthData) error {
			var exists bool
			client, exists = d.Clients.get(p.ClientID)
			if !exists {
				return fmt.Errorf("Unknown client")
			}
			return nil
		})
		if err != nil {
			oauthError(w, 400, "invalid_request", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_ = consentTemplate.Execute(w, map[string]any{"Name": client.Name, "Redirect": p.Redirect, "Request": r.PostForm.Get("request"), "Write": contains(strings.Fields(p.Scope), oauthWrite), "Error": "No connected daemon matches this secret. Check the secret and ensure your daemon is online, then try again."})
		return
	}
	var code string
	err := s.oauth.store.update(func(d *oauthData) error {
		if _, exists := d.Consumed.get(p.ID); exists {
			return fmt.Errorf("Consent was already used")
		}
		if _, exists := d.Clients.get(p.ClientID); !exists {
			return fmt.Errorf("Unknown client")
		}
		if decision == "allow" {
			d.Consumed.put(p.ID, p)
			id := randomID("rwg_", 24)
			d.Grants.put(id, oauthGrant{ID: id, UserID: key.UserID, ClientID: p.ClientID, Scope: p.Scope, Expires: time.Now().Add(time.Minute)})
			manager, _ := oauthEngine(d)
			ti, err := manager.GenerateAuthToken(r.Context(), oauth2.Code, &oauth2.TokenGenerateRequest{ClientID: p.ClientID, UserID: id, RedirectURI: p.Redirect, Scope: p.Scope, CodeChallenge: p.Challenge, CodeChallengeMethod: oauth2.CodeChallengeS256})
			if err != nil {
				return err
			}
			code = ti.GetCode()
		}
		return nil
	})
	if err != nil {
		oauthError(w, 400, "invalid_request", err.Error())
		return
	}
	u, _ := url.Parse(p.Redirect)
	q := u.Query()
	q.Set("state", p.State)
	q.Set("iss", s.oauth.issuer)
	if decision == "allow" {
		q.Set("code", code)
		http.SetCookie(w, &http.Cookie{Name: "rw_token", Value: key.Key, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.oauth.issuer, "https:"), SameSite: http.SameSiteLaxMode, MaxAge: int(grantLifetime.Seconds())})
	} else {
		q.Set("error", "access_denied")
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}
func (s *Server) oauthExchange(w http.ResponseWriter, r *http.Request) {
	if !parseOAuthForm(w, r) {
		return
	}
	f := r.PostForm
	if f.Get("resource") != s.oauth.issuer+"/mcp" {
		oauthError(w, 400, "invalid_target", "resource must identify this MCP server")
		return
	}

	var result map[string]any
	status := http.StatusOK
	err := s.oauth.store.update(func(d *oauthData) error {
		reject := func(description string) {
			status = 400
			result = map[string]any{"error": "invalid_grant", "error_description": description}
		}
		// Tombstones retain the grant linkage until absolute expiry. A replay
		// revokes the whole family in the same transaction as its detection.
		if f.Get("grant_type") == "refresh_token" {
			if replay, used := d.UsedRefresh.get(secretHash(f.Get("refresh_token"))); used {
				g, ok := d.Grants.get(replay.GrantID)
				if ok && g.ClientID == f.Get("client_id") {
					g.Revoked = true
					d.Grants.put(g.ID, g)
				}
				reject("Refresh token was already used; reconnect")
				return nil
			}
			t, ok := d.Refresh.get(secretHash(f.Get("refresh_token")))
			if !ok || t.ClientID != f.Get("client_id") {
				reject("Refresh token does not belong to this client")
				return nil
			}
		}
		_, engine := oauthEngine(d)
		grantType, request, err := engine.ValidationTokenRequest(r)
		if err == nil {
			var token oauth2.TokenInfo
			token, err = engine.GetAccessToken(r.Context(), grantType, request)
			if err == nil {
				result = engine.GetTokenData(token)
				return nil
			}
		}
		result, status, _ = engine.GetErrorData(r.Context(), err)
		return nil
	})
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable", "OAuth storage unavailable")
		return
	}
	// Return credentials only after the complete library exchange is committed.
	writeJSON(w, status, result)
}

func (s *Server) oauthRevoke(w http.ResponseWriter, r *http.Request) {
	if !parseOAuthForm(w, r) {
		return
	}
	err := s.oauth.store.update(func(d *oauthData) error {
		hash := secretHash(r.PostForm.Get("token"))
		t, ok := d.Refresh.get(hash)
		if !ok {
			t, ok = d.Access.get(hash)
		}
		if ok {
			g, exists := d.Grants.get(t.UserID)
			if exists && g.ClientID == r.PostForm.Get("client_id") {
				g.Revoked = true
				d.Grants.put(g.ID, g)
			}
		}
		return nil
	})
	if err != nil {
		oauthError(w, 503, "temporarily_unavailable", "OAuth storage unavailable")
		return
	}
	w.WriteHeader(200)
}
func (s *Server) accessGrant(r *http.Request) (oauthGrant, error) {
	var grant oauthGrant
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return grant, errInvalidAccess
	}
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	err := s.oauth.store.view(func(d *oauthData) error {
		manager, _ := oauthEngine(d)
		ti, err := manager.LoadAccessToken(r.Context(), raw)
		if err != nil {
			return nil
		}
		g, ok := d.Grants.get(ti.GetUserID())
		if ok && !g.Revoked {
			grant = g
			grant.Scope = ti.GetScope()
		}
		return nil
	})
	if err != nil {
		return grant, err
	}
	if grant.ID == "" {
		return grant, errInvalidAccess
	}
	return grant, nil
}

var connectionsTemplate = template.Must(template.New("connections").Parse(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Connected apps · Repowire</title></head><body><h1>Connected apps</h1>{{if .LoggedIn}}{{range .Grants}}<form method="post" action="/oauth/connections"><p>{{.Name}} — {{.Scope}} — expires {{.Expires}}</p><input type="hidden" name="grant" value="{{.ID}}"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button>Disconnect</button></form>{{else}}<p>No connected apps.</p>{{end}}{{else}}<p><a href="/">Sign in to the relay</a>, then return here to manage connections.</p>{{end}}</body></html>`))

func (s *Server) oauthConnections(w http.ResponseWriter, r *http.Request) {
	oauthHeaders(w)
	key, ok := s.cookieKey(r)
	nonce := s.browserNonce(w, r)
	rows := []map[string]string{}
	if ok {
		err := s.oauth.store.view(func(d *oauthData) error {
			for _, g := range d.Grants.list(key.UserID) {
				if g.UserID == key.UserID && !g.Revoked && g.Active {
					client, _ := d.Clients.get(g.ClientID)
					rows = append(rows, map[string]string{"ID": g.ID, "Name": client.Name, "Scope": g.Scope, "Expires": g.Expires.Format(time.RFC3339)})
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, "OAuth storage unavailable", 503)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = connectionsTemplate.Execute(w, map[string]any{"LoggedIn": ok, "Grants": rows, "CSRF": nonce})
}
func (s *Server) oauthDisconnect(w http.ResponseWriter, r *http.Request) {
	if !parseOAuthForm(w, r) {
		return
	}
	key, ok := s.cookieKey(r)
	if !ok || !s.sameOrigin(r) || !browserProof(r, secretHash(r.PostForm.Get("csrf"))) {
		oauthError(w, 403, "access_denied", "Invalid browser session")
		return
	}
	err := s.oauth.store.update(func(d *oauthData) error {
		g, ok := d.Grants.get(r.PostForm.Get("grant"))
		if ok && g.UserID == key.UserID {
			g.Revoked = true
			d.Grants.put(g.ID, g)
		}
		return nil
	})
	if err != nil {
		http.Error(w, "OAuth storage unavailable", 503)
		return
	}
	http.Redirect(w, r, "/oauth/connections", 303)
}

// RFC 8252 permits native clients to choose a fresh ephemeral loopback port.
// All other URI components still match exactly; code exchange remains bound to
// the actual redirect URI used for that authorization.
func matchesRedirect(registered []string, actual string) bool {
	for _, raw := range registered {
		if raw == actual {
			return true
		}
		want, e1 := url.Parse(raw)
		got, e2 := url.Parse(actual)
		if e1 != nil || e2 != nil || want.Scheme != "http" || got.Scheme != "http" || !validHTTPSOrLoopback(want) || !validHTTPSOrLoopback(got) {
			continue
		}
		if want.Hostname() != got.Hostname() {
			continue
		}
		want.Host = want.Hostname()
		got.Host = got.Hostname()
		if want.String() == got.String() {
			return true
		}
	}
	return false
}
