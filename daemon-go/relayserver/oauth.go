package relayserver

import (
	"crypto/subtle"
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
	"golang.org/x/time/rate"
)

var errInvalidAccess = errors.New("invalid access token")

const oauthRead = "agents:read"
const oauthWrite = "agents:write"
const accessLifetime = 15 * time.Minute
const grantLifetime = 30 * 24 * time.Hour

type relayOAuth struct {
	issuer            string
	store             *oauthStore
	registrationLimit *rate.Limiter
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
	if err := store.update(func(d *oauthData) error {
		if d.Issuer != "" && d.Issuer != issuer {
			return fmt.Errorf("OAuth database belongs to a different issuer")
		}
		d.Issuer = issuer
		return nil
	}); err != nil {
		_ = store.db.Close()
		return err
	}
	s.oauth = &relayOAuth{issuer: issuer, store: store, registrationLimit: rate.NewLimiter(rate.Every(time.Second), 20)}
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
	if !s.oauth.registrationLimit.Allow() {
		w.Header().Set("Retry-After", "1")
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
	for _, raw := range c.Redirects {
		u, e := url.Parse(raw)
		if e != nil || !validHTTPSOrLoopback(u) || len(raw) > 2048 {
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
		if len(d.Clients) >= 10000 {
			return fmt.Errorf("registration capacity reached")
		}
		d.Clients[c.ID] = c
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
	if c, e := r.Cookie("rw_oauth_csrf"); e == nil && len(c.Value) == 43 {
		return c.Value
	}
	nonce := randomID("", 32)
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

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html><html><head><meta name="viewport" content="width=device-width"><title>Connect Repowire</title><style>body{font:17px system-ui;max-width:520px;margin:8vh auto;padding:24px;color:#19283a;background:#f6f8fb}input,button{font:inherit;padding:12px;margin:8px 0}input{box-sizing:border-box;width:100%}button{cursor:pointer}small{color:#536275}</style></head><body><h1>Connect to Repowire</h1><p><strong>{{.Name}}</strong> requests access to your relay-connected machines.</p><p>Read agents and replies{{if .Write}}; send messages and work requests{{end}}.</p><p><small>Client-provided name. Callback: {{.Redirect}}</small></p><form method="post" action="/oauth/authorize"><input type="hidden" name="request" value="{{.Request}}">{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}{{if .LoggedIn}}<p>You are signed in to the relay.</p>{{else}}<label>Relay secret<input type="password" name="relay_secret" autocomplete="current-password" placeholder="rw_…" required></label>{{end}}<p>Access lasts up to 30 days. You can revoke it from Connected apps.</p><button name="decision" value="allow">Allow access</button> <button name="decision" value="deny" formnovalidate>Cancel</button></form></body></html>`))

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
	id := randomID("", 32)
	var c oauthClient
	err := s.oauth.store.update(func(d *oauthData) error {
		var ok bool
		c, ok = d.Clients[q.Get("client_id")]
		if !ok || !contains(c.Redirects, q.Get("redirect_uri")) {
			return fmt.Errorf("Unknown client or redirect URI")
		}
		if len(d.Pending) >= 1000 {
			return fmt.Errorf("Too many pending authorizations")
		}
		d.Pending[secretHash(id)] = oauthPending{c.ID, q.Get("redirect_uri"), q.Get("code_challenge"), scope, q.Get("state"), secretHash(nonce), time.Now().Add(10 * time.Minute)}
		return nil
	})
	if err != nil {
		oauthError(w, 400, "invalid_request", err.Error())
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
	// An arbitrary well-shaped key is a namespace, not proof of an existing mesh.
	// First authorization requires the daemon holding that exact secret online.
	if decision == "allow" && (!ok || s.anyDaemon(key.UserID) == nil) {
		var pending oauthPending
		var client oauthClient
		err := s.oauth.store.update(func(d *oauthData) error {
			var exists bool
			pending, exists = d.Pending[secretHash(r.PostForm.Get("request"))]
			if !exists || !browserProof(r, pending.Browser) {
				return fmt.Errorf("Authorization expired or browser session changed")
			}
			client = d.Clients[pending.ClientID]
			return nil
		})
		if err != nil {
			oauthError(w, 400, "invalid_request", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_ = consentTemplate.Execute(w, map[string]any{"Name": client.Name, "Redirect": pending.Redirect, "Request": r.PostForm.Get("request"), "Write": contains(strings.Fields(pending.Scope), oauthWrite), "Error": "No connected daemon matches this secret. Check the secret and ensure your daemon is online, then try again."})
		return
	}
	var p oauthPending
	var code string
	err := s.oauth.store.update(func(d *oauthData) error {
		var exists bool
		p, exists = d.Pending[secretHash(r.PostForm.Get("request"))]
		if !exists || !browserProof(r, p.Browser) {
			return fmt.Errorf("Authorization expired or browser session changed")
		}
		delete(d.Pending, secretHash(r.PostForm.Get("request")))
		if decision == "allow" {
			id := randomID("rwg_", 24)
			d.Grants[id] = oauthGrant{id, key.UserID, p.ClientID, p.Scope, time.Now().Add(grantLifetime), false}
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
			if id, used := d.UsedRefresh[secretHash(f.Get("refresh_token"))]; used {
				g := d.Grants[id]
				if g.ClientID == f.Get("client_id") {
					g.Revoked = true
					d.Grants[id] = g
				}
				reject("Refresh token was already used; reconnect")
				return nil
			}
			t, ok := d.Refresh[secretHash(f.Get("refresh_token"))]
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
		t, ok := d.Refresh[hash]
		if !ok {
			t, ok = d.Access[hash]
		}
		if ok {
			g := d.Grants[t.UserID]
			if g.ClientID == r.PostForm.Get("client_id") {
				g.Revoked = true
				d.Grants[g.ID] = g
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
	err := s.oauth.store.update(func(d *oauthData) error {
		manager, _ := oauthEngine(d)
		ti, err := manager.LoadAccessToken(r.Context(), raw)
		if err != nil {
			return nil
		}
		g, ok := d.Grants[ti.GetUserID()]
		if ok && !g.Revoked {
			grant = g
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
		err := s.oauth.store.update(func(d *oauthData) error {
			for _, g := range d.Grants {
				if g.UserID == key.UserID && !g.Revoked {
					rows = append(rows, map[string]string{"ID": g.ID, "Name": d.Clients[g.ClientID].Name, "Scope": g.Scope, "Expires": g.Expires.Format(time.RFC3339)})
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
		g, ok := d.Grants[r.PostForm.Get("grant")]
		if ok && g.UserID == key.UserID {
			g.Revoked = true
			d.Grants[g.ID] = g
		}
		return nil
	})
	if err != nil {
		http.Error(w, "OAuth storage unavailable", 503)
		return
	}
	http.Redirect(w, r, "/oauth/connections", 303)
}
