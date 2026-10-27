package relayserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Repowire Managed: the relay is the control plane for hosted agents. Agents
// run on a Cloudflare Worker that trusts only the relay's control token; the
// relay authenticates the browser, names the owner, and proxies.

const managedPrefix = "/api/v1/managed"

// ManagedConfig is optional. Without URL and ControlToken the managed and
// admin APIs answer 503; without the Cloudflare pair, costs report no actual.
type ManagedConfig struct {
	URL              string
	ControlToken     string
	AdminUsers       []string
	CFAccountID      string
	CFAnalyticsToken string
}

func ManagedConfigFromEnv() ManagedConfig {
	var admins []string
	for _, id := range strings.Split(os.Getenv("REPOWIRE_ADMIN_USERS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			admins = append(admins, id)
		}
	}
	return ManagedConfig{
		URL:              strings.TrimSpace(os.Getenv("REPOWIRE_MANAGED_URL")),
		ControlToken:     strings.TrimSpace(os.Getenv("REPOWIRE_MANAGED_CONTROL_TOKEN")),
		AdminUsers:       admins,
		CFAccountID:      strings.TrimSpace(os.Getenv("REPOWIRE_CF_ACCOUNT_ID")),
		CFAnalyticsToken: strings.TrimSpace(os.Getenv("REPOWIRE_CF_ANALYTICS_TOKEN")),
	}
}

// managedWorker calls the Worker control API with the relay's credential.
type managedWorker struct {
	base   *url.URL
	token  string
	client *http.Client
}

// ConfigureManaged must run before serving. It is safe to call with an empty
// config: the routes stay registered and report themselves unconfigured.
func (s *Server) ConfigureManaged(cfg ManagedConfig) error {
	s.admins = cfg.AdminUsers
	s.costs.configure(cfg.CFAccountID, cfg.CFAnalyticsToken)
	if cfg.URL == "" && cfg.ControlToken == "" {
		return nil
	}
	base, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return fmt.Errorf("REPOWIRE_MANAGED_URL must be an http(s) URL")
	}
	if cfg.ControlToken == "" {
		return fmt.Errorf("REPOWIRE_MANAGED_CONTROL_TOKEN is required with REPOWIRE_MANAGED_URL")
	}
	s.worker = &managedWorker{base: base, token: cfg.ControlToken, client: &http.Client{Timeout: 30 * time.Second}}
	s.events.dial = s.worker.dialEvents
	return nil
}

func (m *managedWorker) url(path, query string) string {
	u := *m.base
	u.Path = m.base.Path + path
	u.RawPath = ""
	u.RawQuery = query
	return u.String()
}

func (m *managedWorker) header(owner string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + m.token}}
	if owner != "" {
		h.Set("X-Repowire-Owner", owner)
	}
	return h
}

// call sends one control request. owner is empty for /admin routes.
func (m *managedWorker) call(ctx context.Context, method, path, query, owner string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.url(path, query), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = m.header(owner)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return m.client.Do(req)
}

// getJSON decodes a 200 response; anything else becomes an error carrying the
// Worker's status and {error} text.
func (m *managedWorker) getJSON(ctx context.Context, path, query, owner string, out any) error {
	resp, err := m.call(ctx, http.MethodGet, path, query, owner, nil)
	if err != nil {
		return fmt.Errorf("managed service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&failure)
		return fmt.Errorf("managed service %s returned %d: %s", path, resp.StatusCode, failure.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *Server) managedRoutes() {
	user := func(h managedHandler) http.HandlerFunc { return s.managedRoute(false, false, h) }
	owner := func(h managedHandler) http.HandlerFunc { return s.managedRoute(false, true, h) }
	admin := func(h managedHandler) http.HandlerFunc { return s.managedRoute(true, true, h) }

	s.mux.HandleFunc("GET "+managedPrefix+"/me", user(s.managedMe))
	s.mux.HandleFunc("GET "+managedPrefix+"/clients", user(s.managedClients))
	s.mux.HandleFunc("DELETE "+managedPrefix+"/apps/{grant_id}", user(s.managedRevokeApp))
	s.mux.HandleFunc("GET "+managedPrefix+"/events", owner(s.managedEventsStream))
	for _, route := range []string{
		"GET /agents", "POST /agents", "GET /agents/{id}", "PATCH /agents/{id}", "DELETE /agents/{id}",
		"GET /agents/{id}/messages", "POST /agents/{id}/messages", "GET /agents/{id}/runs", "GET /agents/{id}/asks",
		"POST /agents/{id}/abort", "GET /workspaces",
	} {
		method, path, _ := strings.Cut(route, " ")
		s.mux.HandleFunc(method+" "+managedPrefix+path, owner(s.managedProxy))
	}
	for _, name := range []string{"owners", "sandboxes", "usage"} {
		s.mux.HandleFunc("GET /api/v1/admin/"+name, admin(s.adminProxy))
	}
	s.mux.HandleFunc("GET /api/v1/admin/summary", admin(s.adminSummary))
	s.mux.HandleFunc("GET /api/v1/admin/costs", admin(s.adminCosts))
	s.mux.HandleFunc("GET /managed", s.managedPage("managed.html", false))
	s.mux.HandleFunc("GET /admin", s.managedPage("admin.html", true))
}

type managedHandler func(w http.ResponseWriter, r *http.Request, user string)

// managedRoute checks, in order: caller identity (401), admin membership (403),
// cross-origin cookie writes (403), Worker configuration (503).
func (s *Server) managedRoute(admin, needsWorker bool, h managedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		key, viaCookie, ok := s.callerKey(r)
		if !ok {
			managedError(w, http.StatusUnauthorized, "sign in with your relay key")
			return
		}
		if admin && !s.isAdmin(key.UserID) {
			managedError(w, http.StatusForbidden, "admin only")
			return
		}
		if viaCookie && r.Method != http.MethodGet && !sameHostOrigin(r) {
			managedError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if needsWorker && s.worker == nil {
			managedError(w, http.StatusServiceUnavailable, "managed agents are not configured on this relay (set REPOWIRE_MANAGED_URL and REPOWIRE_MANAGED_CONTROL_TOKEN)")
			return
		}
		h(w, r, key.UserID)
	}
}

// callerKey accepts the dashboard cookie or an Authorization: Bearer relay key.
func (s *Server) callerKey(r *http.Request) (APIKey, bool, bool) {
	if raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		key, valid := s.tokens.validate(strings.TrimSpace(raw))
		return key, false, valid
	}
	key, ok := s.cookieKey(r)
	return key, true, ok
}

func (s *Server) isAdmin(userID string) bool { return slices.Contains(s.admins, userID) }

// SameSite=Lax already withholds the cookie from cross-site writes; this also
// refuses same-site but cross-origin pages.
func sameHostOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func managedError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) managedMe(w http.ResponseWriter, _ *http.Request, user string) {
	writeJSON(w, http.StatusOK, map[string]any{"user_id": user, "admin": s.isAdmin(user), "managed": s.worker != nil})
}

// managedProxy forwards an owner route to the Worker path of the same shape
// and passes its status and body through untouched.
func (s *Server) managedProxy(w http.ResponseWriter, r *http.Request, user string) {
	s.forward(w, r, strings.TrimPrefix(r.URL.EscapedPath(), managedPrefix), user)
}

func (s *Server) adminProxy(w http.ResponseWriter, r *http.Request, _ string) {
	s.forward(w, r, strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1"), "")
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, path, owner string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		managedError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	resp, err := s.worker.call(r.Context(), r.Method, path, r.URL.RawQuery, owner, body)
	if err != nil {
		managedError(w, http.StatusBadGateway, "managed service unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type relayDaemon struct {
	DaemonID    string `json:"daemon_id"`
	ConnectedAt string `json:"connected_at"`
	Connected   bool   `json:"connected"`
}

func (c *daemonConn) view(connected bool) relayDaemon {
	return relayDaemon{DaemonID: c.daemonID, ConnectedAt: c.connectedAt.Format(time.RFC3339Nano), Connected: connected}
}

func (s *Server) relayDaemons(userID string) []relayDaemon {
	conns := s.allDaemons(userID)
	sort.Slice(conns, func(i, j int) bool { return conns[i].daemonID < conns[j].daemonID })
	out := make([]relayDaemon, 0, len(conns))
	for _, conn := range conns {
		out = append(out, conn.view(true))
	}
	return out
}

type managedApp struct {
	GrantID    string `json:"grant_id"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name,omitempty"`
	Scope      string `json:"scope"`
	Expires    string `json:"expires"`
	Active     bool   `json:"active"`
}

func (s *Server) userApps(userID string) ([]managedApp, error) {
	apps := []managedApp{}
	if s.oauth == nil {
		return apps, nil
	}
	err := s.oauth.store.view(func(d *oauthData) error {
		for _, g := range d.Grants.list(userID) {
			if g.UserID != userID || g.Revoked {
				continue
			}
			client, _ := d.Clients.get(g.ClientID)
			apps = append(apps, managedApp{GrantID: g.ID, ClientID: g.ClientID, ClientName: client.Name, Scope: g.Scope, Expires: g.Expires.UTC().Format(time.RFC3339), Active: g.Active})
		}
		return nil
	})
	sort.Slice(apps, func(i, j int) bool { return apps[i].Expires > apps[j].Expires })
	return apps, err
}

// managedClients combines what connects on the user's behalf: daemons on this
// relay, daemons on the Worker gateway, and OAuth apps. A Worker failure
// degrades the gateway section with a visible gateway_error, not the response.
func (s *Server) managedClients(w http.ResponseWriter, r *http.Request, user string) {
	out := map[string]any{"daemons": s.relayDaemons(user), "gateway": []any{}, "outbox": 0}
	apps, err := s.userApps(user)
	if err != nil {
		managedError(w, http.StatusServiceUnavailable, "OAuth storage unavailable")
		return
	}
	out["apps"] = apps
	if s.worker == nil {
		out["gateway_error"] = "managed agents are not configured on this relay"
	} else {
		var gateway struct {
			Items  []any `json:"items"`
			Outbox int   `json:"outbox"`
		}
		if err := s.worker.getJSON(r.Context(), "/clients", "", user, &gateway); err != nil {
			out["gateway_error"] = err.Error()
		} else {
			if gateway.Items != nil {
				out["gateway"] = gateway.Items
			}
			out["outbox"] = gateway.Outbox
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) managedRevokeApp(w http.ResponseWriter, r *http.Request, user string) {
	if s.oauth == nil {
		managedError(w, http.StatusNotFound, "app grant not found")
		return
	}
	id := r.PathValue("grant_id")
	found := false
	err := s.oauth.store.update(func(d *oauthData) error {
		g, ok := d.Grants.get(id)
		if !ok || g.UserID != user {
			return nil
		}
		found = true
		g.Revoked = true
		d.Grants.put(g.ID, g)
		return nil
	})
	switch {
	case err != nil:
		managedError(w, http.StatusServiceUnavailable, "OAuth storage unavailable")
	case !found:
		managedError(w, http.StatusNotFound, "app grant not found")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "grant_id": id})
	}
}

// managedPage serves a static export page to a signed-in user. Signed-out
// visitors go to the landing page, like /dashboard.
func (s *Server) managedPage(file string, admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := s.cookieKey(r)
		if !ok {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if admin && !s.isAdmin(key.UserID) {
			http.Error(w, "Admin only.", http.StatusForbidden)
			return
		}
		s.serveExport(w, r, file)
	}
}

func (s *Server) serveExport(w http.ResponseWriter, r *http.Request, file string) {
	path := filepath.Join(s.webOut, file)
	if stat, err := os.Stat(path); s.webOut == "" || err != nil || stat.IsDir() {
		http.Error(w, "Page not built. Rebuild relay image.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, path)
}
