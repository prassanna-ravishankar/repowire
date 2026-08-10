// Package relayserver hosts the public relay that local Repowire daemons dial.
package relayserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	defaultTunnelTimeout = 30 * time.Second
	streamStartTimeout   = 10 * time.Second
)

var tunnelRoots = []string{
	"/ack", "/acp", "/answer", "/ask", "/ask-many", "/asks", "/attachments",
	"/broadcast", "/circles", "/deliveries", "/events", "/health", "/hooks",
	"/jobs", "/kill-peer", "/notify", "/panes", "/peer", "/peers", "/query",
	"/questions", "/reviews", "/schedules", "/session", "/sessions",
	"/shares", "/spawn", "/traces", "/work", "/ws",
}

type daemonConn struct {
	userID      string
	daemonID    string
	connectedAt time.Time
	ws          *websocket.Conn
	writeMu     sync.Mutex
}

func (c *daemonConn) write(ctx context.Context, value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, c.ws, value)
}

type pendingRequest struct {
	conn *daemonConn
	ch   chan map[string]any
}

// Server is an in-memory relay. API keys and share links intentionally expire
// when the process restarts, matching the previous hosted relay contract.
type Server struct {
	mu          sync.Mutex
	connections map[string]*daemonConn
	users       map[string]map[string]*daemonConn
	pending     map[string]*pendingRequest
	tokens      *tokenStore
	webOut      string
	timeout     time.Duration
	mux         *http.ServeMux
}

func New(webOut string) *Server {
	s := &Server{
		connections: map[string]*daemonConn{},
		users:       map[string]map[string]*daemonConn{},
		pending:     map[string]*pendingRequest{},
		tokens:      newTokenStore(),
		webOut:      webOut,
		timeout:     defaultTunnelTimeout,
		mux:         http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /auth", s.auth)
	s.mux.HandleFunc("GET /dashboard", s.dashboard)
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /events/stream", s.eventsStream)
	s.mux.HandleFunc("POST /api/v1/register", s.registerToken)
	s.mux.HandleFunc("GET /api/v1/daemons", s.listDaemons)
	s.mux.HandleFunc("POST /api/v1/share", s.createShare)
	s.mux.HandleFunc("GET /api/v1/share", s.listShares)
	s.mux.HandleFunc("DELETE /api/v1/share/{share_id}", s.revokeShare)
	s.mux.HandleFunc("GET /s/{share_id}", s.shareViewer)
	s.mux.HandleFunc("GET /s/{share_id}/stream", s.shareStream)
	s.mux.HandleFunc("POST /s/{share_id}/ask", s.shareAsk)
	s.mux.HandleFunc("/ws/relay", s.relayWebSocket)
	s.mux.HandleFunc("/d/{token}/{path...}", s.legacyTunnel)
	if s.webOut != "" {
		next := filepath.Join(s.webOut, "_next")
		if stat, err := os.Stat(next); err == nil && stat.IsDir() {
			s.mux.Handle("/_next/", http.StripPrefix("/_next/", http.FileServer(http.Dir(next))))
		}
	}
	s.mux.HandleFunc("/", s.cookieTunnel)
}

func connKey(userID, daemonID string) string { return userID + "/" + daemonID }

func (s *Server) registerConn(c *daemonConn) *daemonConn {
	key := connKey(c.userID, c.daemonID)
	s.mu.Lock()
	old := s.connections[key]
	s.connections[key] = c
	if s.users[c.userID] == nil {
		s.users[c.userID] = map[string]*daemonConn{}
	}
	s.users[c.userID][key] = c
	s.mu.Unlock()
	return old
}

func (s *Server) unregisterConn(c *daemonConn) {
	s.mu.Lock()
	key := connKey(c.userID, c.daemonID)
	if s.connections[key] != c {
		s.mu.Unlock()
		return
	}
	delete(s.connections, key)
	delete(s.users[c.userID], key)
	if len(s.users[c.userID]) == 0 {
		delete(s.users, c.userID)
	}
	for id, pending := range s.pending {
		if pending.conn == c {
			delete(s.pending, id)
			select {
			case pending.ch <- map[string]any{"type": "daemon_disconnected"}:
			default:
			}
		}
	}
	s.mu.Unlock()
}

func (s *Server) daemon(userID, daemonID string) *daemonConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections[connKey(userID, daemonID)]
}

func (s *Server) anyDaemon(userID string) *daemonConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range s.users[userID] {
		return conn
	}
	return nil
}

func (s *Server) allDaemons(userID string) []*daemonConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*daemonConn, 0, len(s.users[userID]))
	for _, conn := range s.users[userID] {
		out = append(out, conn)
	}
	return out
}

func (s *Server) addPending(id string, conn *daemonConn) chan map[string]any {
	ch := make(chan map[string]any, 16)
	s.mu.Lock()
	s.pending[id] = &pendingRequest{conn: conn, ch: ch}
	s.mu.Unlock()
	return ch
}

func (s *Server) removePending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func (s *Server) deliverPending(msg map[string]any) {
	id, _ := msg["request_id"].(string)
	s.mu.Lock()
	pending := s.pending[id]
	s.mu.Unlock()
	if pending != nil {
		select {
		case pending.ch <- msg:
		default:
			select {
			case <-pending.ch:
			default:
			}
			select {
			case pending.ch <- map[string]any{"type": "relay_overflow"}:
			default:
			}
		}
	}
}

func (s *Server) landing(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("rw_token"); err == nil {
		if _, ok := s.tokens.validate(cookie.Value); ok {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, landingHTML(r.URL.Query().Get("error")))
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/?error=missing_token", http.StatusSeeOther)
		return
	}
	token := strings.TrimSpace(r.Form.Get("token"))
	if token == "" {
		http.Redirect(w, r, "/?error=missing_token", http.StatusSeeOther)
		return
	}
	key, ok := s.tokens.validate(token)
	if !ok {
		http.Redirect(w, r, "/?error=invalid_key", http.StatusSeeOther)
		return
	}
	if s.anyDaemon(key.UserID) == nil {
		http.Redirect(w, r, "/?error=no_daemon", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "rw_token", Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.cookieKey(r); !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	path := filepath.Join(s.webOut, "dashboard.html")
	if stat, err := os.Stat(path); s.webOut == "" || err != nil || stat.IsDir() {
		http.Error(w, "Dashboard not built. Rebuild relay image.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, path)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	count := len(s.connections)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "connected_daemons": count})
}

func (s *Server) registerToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
	}
	if decodeJSON(r, &req) != nil || strings.TrimSpace(req.UserID) == "" {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	writeJSON(w, http.StatusOK, s.tokens.register(req.UserID))
}

func (s *Server) headerKey(r *http.Request) (APIKey, bool) {
	return s.tokens.validate(r.Header.Get("X-API-Key"))
}

func (s *Server) cookieKey(r *http.Request) (APIKey, bool) {
	cookie, err := r.Cookie("rw_token")
	if err != nil {
		return APIKey{}, false
	}
	return s.tokens.validate(cookie.Value)
}

func (s *Server) listDaemons(w http.ResponseWriter, r *http.Request) {
	key, ok := s.headerKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	daemons := s.allDaemons(key.UserID)
	sort.Slice(daemons, func(i, j int) bool { return daemons[i].daemonID < daemons[j].daemonID })
	out := make([]map[string]any, 0, len(daemons))
	for _, daemon := range daemons {
		out = append(out, map[string]any{"daemon_id": daemon.daemonID, "connected_at": daemon.connectedAt.Format(time.RFC3339Nano)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	key, ok := s.headerKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	var req struct {
		PeerName    string `json:"peer_name"`
		Permissions string `json:"permissions"`
		TTLSeconds  *int   `json:"ttl_secs"`
	}
	if decodeJSON(r, &req) != nil || strings.TrimSpace(req.PeerName) == "" {
		writeError(w, http.StatusBadRequest, "peer_name is required")
		return
	}
	if req.Permissions == "" {
		req.Permissions = "ro"
	}
	if req.Permissions != "ro" && req.Permissions != "rw" {
		writeError(w, http.StatusBadRequest, "permissions must be 'ro' or 'rw'")
		return
	}
	var ttl time.Duration
	if req.TTLSeconds != nil {
		if *req.TTLSeconds <= 0 {
			writeError(w, http.StatusBadRequest, "ttl_secs must be a positive integer")
			return
		}
		ttl = time.Duration(*req.TTLSeconds) * time.Second
	}
	if s.anyDaemon(key.UserID) == nil {
		writeError(w, http.StatusBadGateway, "No daemon connected")
		return
	}
	writeJSON(w, http.StatusOK, s.tokens.createShare(key.UserID, req.PeerName, req.Permissions, ttl))
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	key, ok := s.headerKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	writeJSON(w, http.StatusOK, s.tokens.listShares(key.UserID))
}

func (s *Server) revokeShare(w http.ResponseWriter, r *http.Request) {
	key, ok := s.headerKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	id := r.PathValue("share_id")
	share, ok := s.tokens.share(id)
	if !ok {
		writeError(w, http.StatusNotFound, "Share token not found")
		return
	}
	if share.UserID != key.UserID {
		writeError(w, http.StatusForbidden, "Not your share token")
		return
	}
	s.tokens.revokeShare(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "share_id": id})
}

func (s *Server) shareViewer(w http.ResponseWriter, r *http.Request) {
	share, ok := s.tokens.share(r.PathValue("share_id"))
	if !ok {
		http.Error(w, "Share link not found or expired.", http.StatusNotFound)
		return
	}
	if s.anyDaemon(share.UserID) == nil {
		http.Error(w, "Session owner is not connected.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, viewerHTML(share))
}

func (s *Server) shareAsk(w http.ResponseWriter, r *http.Request) {
	share, ok := s.tokens.share(r.PathValue("share_id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Share not found or expired")
		return
	}
	if share.Permissions != "rw" {
		writeError(w, http.StatusForbidden, "This share link is read-only")
		return
	}
	conn := s.anyDaemon(share.UserID)
	if conn == nil {
		writeError(w, http.StatusServiceUnavailable, "Session owner not connected")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if decodeJSON(r, &req) != nil || strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	body, _ := json.Marshal(map[string]any{"from_peer": "guest", "to_peer": share.PeerName, "text": strings.TrimSpace(req.Text)})
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.Header = http.Header{"Content-Type": []string{"application/json"}}
	s.tunnel(w, r2, conn, "/ask")
}

func isTunnelPath(path string) bool {
	for _, root := range tunnelRoots {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func (s *Server) legacyTunnel(w http.ResponseWriter, r *http.Request) {
	key, ok := s.tokens.validate(r.PathValue("token"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	conn := s.anyDaemon(key.UserID)
	if conn == nil {
		writeError(w, http.StatusBadGateway, "No daemon connected")
		return
	}
	s.tunnel(w, r, conn, "/"+r.PathValue("path"))
}

func (s *Server) cookieTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		s.landing(w, r)
		return
	}
	if s.serveStatic(w, r) {
		return
	}
	if !isTunnelPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	key, ok := s.cookieKey(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	conn := s.anyDaemon(key.UserID)
	if conn == nil {
		writeError(w, http.StatusBadGateway, "No daemon connected")
		return
	}
	s.tunnel(w, r, conn, r.URL.Path)
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) bool {
	if s.webOut == "" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(r.URL.Path))
	switch ext {
	case ".ico", ".webp", ".png", ".jpg", ".jpeg", ".svg", ".gif", ".woff", ".woff2":
	default:
		return false
	}
	rel := strings.TrimPrefix(filepath.Clean("/"+r.URL.Path), string(filepath.Separator))
	path := filepath.Join(s.webOut, rel)
	if stat, err := os.Stat(path); err != nil || stat.IsDir() {
		return false
	}
	http.ServeFile(w, r, path)
	return true
}

func (s *Server) tunnel(w http.ResponseWriter, r *http.Request, conn *daemonConn, path string) {
	id := randomID("req_", 16)
	ch := s.addPending(id, conn)
	defer s.removePending(id)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	headers := map[string]string{}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if lower == "host" || lower == "connection" || lower == "transfer-encoding" || lower == "cookie" {
			continue
		}
		headers[name] = strings.Join(values, ", ")
	}
	msg := map[string]any{"type": "http_request", "request_id": id, "method": r.Method, "path": path, "headers": headers, "query_string": r.URL.RawQuery}
	if len(body) > 0 {
		msg["body"] = base64.StdEncoding.EncodeToString(body)
	}
	if err := conn.write(r.Context(), msg); err != nil {
		writeError(w, http.StatusBadGateway, "Failed to reach daemon")
		return
	}
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case response := <-ch:
		if response["type"] == "daemon_disconnected" {
			writeError(w, http.StatusBadGateway, "Daemon disconnected")
			return
		}
		writeTunnelResponse(w, response)
	case <-timer.C:
		writeError(w, http.StatusGatewayTimeout, "Daemon did not respond in time")
	case <-r.Context().Done():
	}
}

func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	key, ok := s.cookieKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	conn := s.anyDaemon(key.UserID)
	if conn == nil {
		writeError(w, http.StatusBadGateway, "No daemon connected")
		return
	}
	s.streamTunnel(w, r, conn, "")
}

func (s *Server) shareStream(w http.ResponseWriter, r *http.Request) {
	share, ok := s.tokens.share(r.PathValue("share_id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Share not found or expired")
		return
	}
	conn := s.anyDaemon(share.UserID)
	if conn == nil {
		writeError(w, http.StatusServiceUnavailable, "Session owner not connected")
		return
	}
	s.streamTunnel(w, r, conn, share.PeerName)
}

func (s *Server) streamTunnel(w http.ResponseWriter, r *http.Request, conn *daemonConn, peerFilter string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	id := randomID("stream_", 16)
	ch := s.addPending(id, conn)
	defer s.removePending(id)
	ctx := r.Context()
	if err := conn.write(ctx, map[string]any{"type": "http_stream_request", "request_id": id, "method": "GET", "path": "/events/stream", "headers": map[string]string{}, "query_string": ""}); err != nil {
		writeError(w, http.StatusBadGateway, "Failed to reach daemon")
		return
	}
	defer func() {
		cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.write(cancelCtx, map[string]any{"type": "http_stream_cancel", "request_id": id})
	}()

	startTimer := time.NewTimer(streamStartTimeout)
	defer startTimer.Stop()
	select {
	case start := <-ch:
		if start["type"] != "http_stream_start" {
			writeError(w, http.StatusBadGateway, "Daemon stream failed")
			return
		}
		status := intValue(start["status"], http.StatusOK)
		copyHeaders(w.Header(), start["headers"])
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(status)
		flusher.Flush()
	case <-startTimer.C:
		writeError(w, http.StatusGatewayTimeout, "Daemon stream did not start")
		return
	case <-ctx.Done():
		return
	}

	filter := &ssePeerFilter{peer: peerFilter}
	for {
		select {
		case msg := <-ch:
			switch msg["type"] {
			case "http_stream_chunk":
				body, _ := base64.StdEncoding.DecodeString(stringValue(msg["body"]))
				if peerFilter != "" {
					body = filter.write(body)
					if _, valid := s.tokens.share(r.PathValue("share_id")); !valid {
						_, _ = io.WriteString(w, "data: {\"type\":\"share_expired\"}\n\n")
						flusher.Flush()
						return
					}
				}
				if len(body) > 0 {
					if _, err := w.Write(body); err != nil {
						return
					}
					flusher.Flush()
				}
			case "http_stream_end", "daemon_disconnected":
				return
			case "relay_overflow":
				_, _ = io.WriteString(w, "event: error\ndata: {\"detail\":\"relay stream overflow\"}\n\n")
				flusher.Flush()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

type ssePeerFilter struct {
	peer string
	buf  []byte
}

func (f *ssePeerFilter) write(chunk []byte) []byte {
	f.buf = append(f.buf, chunk...)
	var out bytes.Buffer
	for {
		idx := bytes.Index(f.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := append([]byte(nil), f.buf[:idx+2]...)
		f.buf = f.buf[idx+2:]
		trimmed := strings.TrimSpace(string(frame))
		if strings.HasPrefix(trimmed, ":") {
			out.Write(frame)
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) == nil && eventMentionsPeer(event, f.peer) {
			out.Write(frame)
		}
	}
	return out.Bytes()
}

func eventMentionsPeer(event map[string]any, peerName string) bool {
	for _, key := range []string{"peer", "peer_name", "from", "from_peer", "to", "to_peer", "display_name"} {
		if stringValue(event[key]) == peerName {
			return true
		}
	}
	return false
}

func (s *Server) relayWebSocket(w http.ResponseWriter, r *http.Request) {
	key, ok := s.tokens.validate(r.URL.Query().Get("api_key"))
	daemonID := r.URL.Query().Get("daemon_id")
	if !ok || daemonID == "" {
		http.Error(w, "invalid api_key or daemon_id", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(16 << 20)
	daemon := &daemonConn{userID: key.UserID, daemonID: daemonID, connectedAt: time.Now().UTC(), ws: conn}
	if old := s.registerConn(daemon); old != nil {
		_ = old.ws.Close(websocket.StatusCode(4000), "Replaced by new connection")
	}
	log.Printf("relay server: daemon connected %s/%s", key.UserID, daemonID)
	defer func() {
		s.unregisterConn(daemon)
		_ = conn.CloseNow()
		log.Printf("relay server: daemon disconnected %s/%s", key.UserID, daemonID)
	}()

	ctx := r.Context()
	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		switch stringValue(msg["type"]) {
		case "pong":
		case "http_response", "http_stream_start", "http_stream_chunk", "http_stream_end":
			s.deliverPending(msg)
		case "relay_broadcast":
			msg["source_daemon_id"] = daemonID
			for _, target := range s.allDaemons(key.UserID) {
				if target != daemon {
					_ = target.write(ctx, msg)
				}
			}
		case "relay_query", "relay_notify", "relay_response":
			target := s.daemon(key.UserID, stringValue(msg["target_daemon_id"]))
			if target != nil {
				msg["source_daemon_id"] = daemonID
				_ = target.write(ctx, msg)
			}
		}
	}
}

func writeTunnelResponse(w http.ResponseWriter, msg map[string]any) {
	copyHeaders(w.Header(), msg["headers"])
	body, _ := base64.StdEncoding.DecodeString(stringValue(msg["body"]))
	w.WriteHeader(intValue(msg["status"], http.StatusOK))
	_, _ = w.Write(body)
}

func copyHeaders(dst http.Header, raw any) {
	switch headers := raw.(type) {
	case map[string]any:
		for key, value := range headers {
			if !strings.EqualFold(key, "content-length") {
				dst.Set(key, stringValue(value))
			}
		}
	case map[string]string:
		for key, value := range headers {
			if !strings.EqualFold(key, "content-length") {
				dst.Set(key, value)
			}
		}
	}
}

func decodeJSON(r *http.Request, target any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(value)
	}
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		if parsed, err := strconv.Atoi(string(typed)); err == nil {
			return parsed
		}
	}
	return fallback
}

func FindWebOutputDir() string {
	var candidates []string
	if explicit := os.Getenv("REPOWIRE_WEB_OUT"); explicit != "" {
		candidates = append(candidates, explicit)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "web", "out"), filepath.Join(cwd, "..", "web", "out"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "web", "out"), filepath.Join(dir, "..", "web", "out"))
	}
	for _, candidate := range candidates {
		if stat, err := os.Stat(filepath.Join(candidate, "dashboard.html")); err == nil && !stat.IsDir() {
			return filepath.Clean(candidate)
		}
	}
	return ""
}

func ListenAndServe(ctx context.Context, addr, webOut string) error {
	server := &http.Server{Addr: addr, Handler: New(webOut).Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
