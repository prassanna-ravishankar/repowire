// Package relay is the daemon-side connector to the hosted relay at repowire.io.
//
// It dials an OUTBOUND WebSocket to the relay and tunnels inbound traffic to the
// LOCAL daemon's HTTP surface (127.0.0.1), so a browser/phone hitting repowire.io
// reaches this daemon. It is a faithful port of repowire/daemon/relay_client.py
// and deliberately has ZERO coupling to the hub package: it only needs the local
// base URL and forwards to it over real HTTP (which preserves the daemon's
// localhost/auth semantics exactly — a tunneled request is a genuine local
// request, same as the Python client).
package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	pingInterval   = 20 * time.Second // WS-level keepalive (Python ping_interval)
	dialTimeout    = 10 * time.Second // Python open_timeout
	tunnelTimeout  = 30 * time.Second // Python HTTP_TUNNEL_TIMEOUT
	readLimit      = 16 << 20         // 16 MiB; matches Python max_size (attachments)
)

// strippedForwardHeaders are proxy headers removed before forwarding a tunneled
// request to the local daemon, so a remote caller cannot spoof the daemon's
// require_localhost / forwarded-for checks. Mirrors relay_client.py.
var strippedForwardHeaders = map[string]bool{
	"x-forwarded-for":   true,
	"x-forwarded-proto": true,
	"x-forwarded-host":  true,
	"x-real-ip":         true,
	"forwarded":         true,
}

// Client maintains the outbound relay connection and tunnels frames.
type Client struct {
	relayURL     string // ws(s):// base (no trailing slash)
	apiKey       string
	daemonID     string
	localBaseURL string // http://127.0.0.1:<port>
	authToken    string
	httpc        *http.Client

	mu            sync.Mutex
	running       bool
	connected     bool
	stopping      bool
	lastConnected *time.Time
	lastError     string
	lastErrorAt   *time.Time
	cancel        context.CancelFunc
	done          chan struct{}
}

type relaySession struct {
	conn *websocket.Conn

	writeMu   sync.Mutex
	streamsMu sync.Mutex
	streams   map[string]context.CancelFunc
}

func newRelaySession(conn *websocket.Conn) *relaySession {
	return &relaySession{conn: conn, streams: map[string]context.CancelFunc{}}
}

func (s *relaySession) write(ctx context.Context, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return wsjson.Write(ctx, s.conn, value)
}

func (s *relaySession) addStream(id string, cancel context.CancelFunc) {
	s.streamsMu.Lock()
	s.streams[id] = cancel
	s.streamsMu.Unlock()
}

func (s *relaySession) removeStream(id string) {
	s.streamsMu.Lock()
	delete(s.streams, id)
	s.streamsMu.Unlock()
}

func (s *relaySession) cancelStream(id string) {
	s.streamsMu.Lock()
	cancel := s.streams[id]
	s.streamsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *relaySession) cancelAll() {
	s.streamsMu.Lock()
	for _, cancel := range s.streams {
		cancel()
	}
	s.streams = map[string]context.CancelFunc{}
	s.streamsMu.Unlock()
}

// Status is the health/telemetry snapshot (mirrors relay_client.status()).
type Status struct {
	Connected     bool
	Running       bool
	URL           string
	DaemonID      string
	LastConnected *time.Time
	LastError     string
	LastErrorAt   *time.Time
}

// HealthMap projects Status onto the /health "relay" sub-object, mirroring the
// Python RelayHealth model (status ∈ connected|connecting|down).
func (s Status) HealthMap() map[string]any {
	state := "down"
	if s.Connected {
		state = "connected"
	} else if s.Running {
		state = "connecting"
	}
	m := map[string]any{
		"status":    state,
		"enabled":   true,
		"connected": s.Connected,
		"running":   s.Running,
		"url":       s.URL,
	}
	if s.LastConnected != nil {
		m["last_connected_at"] = s.LastConnected.Format(time.RFC3339Nano)
	}
	if s.LastError != "" {
		m["last_error"] = s.LastError
	}
	if s.LastErrorAt != nil {
		m["last_error_at"] = s.LastErrorAt.Format(time.RFC3339Nano)
	}
	return m
}

// NewClient builds a relay client. daemonID identifies this daemon to the relay
// (relay dedupes one connection per (user, daemon_id)); localBaseURL is the local
// daemon HTTP endpoint tunneled requests are forwarded to.
func NewClient(relayURL, apiKey, daemonID, localBaseURL string) *Client {
	return &Client{
		relayURL:     strings.TrimRight(relayURL, "/"),
		apiKey:       apiKey,
		daemonID:     daemonID,
		localBaseURL: strings.TrimRight(localBaseURL, "/"),
		httpc:        &http.Client{Timeout: tunnelTimeout},
	}
}

// WithAuthToken authenticates tunneled requests to a protected local daemon.
func (c *Client) WithAuthToken(token string) *Client {
	c.authToken = token
	return c
}

// Start launches the reconnect loop. Idempotent; no-op when the api key is empty
// or a loop is already running. The parent ctx bounds the loop; Stop also cancels.
func (c *Client) Start(parent context.Context) {
	if c.apiKey == "" {
		return
	}
	c.mu.Lock()
	if c.running || c.stopping {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.running = true
	c.done = make(chan struct{})
	c.mu.Unlock()
	go c.runLoop(ctx)
}

// EnsureRunning relaunches the loop if it died — lazy self-heal, called from
// /health (mirrors relay_client.ensure_running). Returns true if it (re)started.
func (c *Client) EnsureRunning(parent context.Context) bool {
	c.mu.Lock()
	dead := c.apiKey != "" && !c.stopping && !c.running
	c.mu.Unlock()
	if !dead {
		return false
	}
	log.Printf("relay: loop was not running; relaunching (lazy self-heal)")
	c.Start(parent)
	return true
}

// Stop signals shutdown and blocks until the loop exits.
func (c *Client) Stop() {
	c.mu.Lock()
	c.stopping = true
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Status returns the current telemetry snapshot.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{
		Connected:     c.connected,
		Running:       c.running,
		URL:           c.relayURL,
		DaemonID:      c.daemonID,
		LastConnected: c.lastConnected,
		LastError:     c.lastError,
		LastErrorAt:   c.lastErrorAt,
	}
}

func (c *Client) recordErr(err error) {
	now := time.Now().UTC()
	c.mu.Lock()
	c.lastError = err.Error()
	c.lastErrorAt = &now
	c.mu.Unlock()
}

func (c *Client) buildURL() string {
	q := url.Values{}
	q.Set("api_key", c.apiKey)
	q.Set("daemon_id", c.daemonID)
	return c.relayURL + "/ws/relay?" + q.Encode()
}

func (c *Client) runLoop(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		c.running = false
		c.connected = false
		close(c.done)
		c.mu.Unlock()
	}()
	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.recordErr(err)
			log.Printf("relay: connection lost, reconnecting in %s: %v", backoff, err)
		} else {
			log.Printf("relay: closed cleanly, reconnecting in %s", backoff)
		}
		if connected {
			backoff = initialBackoff // reset once we had a live connection
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if !connected {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// connectAndServe dials the relay and serves frames until the socket closes or
// ctx is cancelled. Returns whether it managed to connect (for backoff reset).
func (c *Client) connectAndServe(ctx context.Context) (bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, c.buildURL(), nil)
	cancel()
	if err != nil {
		return false, err
	}
	conn.SetReadLimit(readLimit)
	defer conn.Close(websocket.StatusNormalClosure, "")
	session := newRelaySession(conn)
	defer session.cancelAll()

	now := time.Now().UTC()
	c.mu.Lock()
	c.connected = true
	c.lastConnected = &now
	c.mu.Unlock()
	log.Printf("relay: connected to %s", c.relayURL)
	defer func() {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
	}()

	// WS-level keepalive: the relay does not ping the daemon, so we ping it. A
	// failed ping closes the socket, which breaks the read loop and reconnects.
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				pc, cancel := context.WithTimeout(pingCtx, pingInterval)
				err := conn.Ping(pc)
				cancel()
				if err != nil {
					conn.Close(websocket.StatusPolicyViolation, "ping timeout")
					return
				}
			}
		}
	}()

	// Ordinary requests remain sequential. Streaming requests run in their own
	// goroutine so the read loop can receive cancellation frames; relaySession
	// serializes the resulting WebSocket writes.
	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return true, err
		}
		c.handleMessage(ctx, session, msg)
	}
}

func (c *Client) handleMessage(ctx context.Context, session *relaySession, msg map[string]any) {
	switch t, _ := msg["type"].(string); t {
	case "ping":
		_ = session.write(ctx, map[string]any{"type": "pong"})
	case "http_request":
		c.handleHTTPRequest(ctx, session, msg)
	case "http_stream_request":
		c.startHTTPStream(ctx, session, msg)
	case "http_stream_cancel":
		session.cancelStream(stringField(msg, "request_id"))
	case "relay_query", "relay_notify", "relay_broadcast":
		c.handleRelayMessage(ctx, session, msg)
	default:
		// Unknown/opaque — ignore (Python logs at debug).
	}
}

// handleHTTPRequest tunnels a relay http_request to the local daemon and returns
// an http_response frame (base64 bodies both ways).
func (c *Client) handleHTTPRequest(ctx context.Context, session *relaySession, msg map[string]any) {
	reqID, _ := msg["request_id"].(string)
	path := requestPath(msg)
	// HTTP MCP is deliberately local-only. A tunneled request originates from
	// the hosted relay even though the final local hop would appear loopback, so
	// reject it here before it can reach the daemon's localhost auth check.
	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		c.sendHTTPResponse(ctx, session, reqID, http.StatusNotFound, nil, []byte("not found"))
		return
	}
	var body []byte
	if b, ok := msg["body"].(string); ok && b != "" {
		body, _ = base64.StdEncoding.DecodeString(b)
	}
	req, err := c.localRequest(ctx, msg, bytes.NewReader(body))
	if err != nil {
		c.sendHTTPResponse(ctx, session, reqID, http.StatusBadGateway, nil, []byte(err.Error()))
		return
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		c.recordErr(err)
		c.sendHTTPResponse(ctx, session, reqID, http.StatusBadGateway, nil, []byte(err.Error()))
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	c.sendHTTPResponse(ctx, session, reqID, resp.StatusCode, responseHeaders(resp.Header), respBody)
}

func (c *Client) sendHTTPResponse(ctx context.Context, session *relaySession, reqID string, status int, headers map[string]any, body []byte) {
	if headers == nil {
		headers = map[string]any{}
	}
	_ = session.write(ctx, map[string]any{
		"type":       "http_response",
		"request_id": reqID,
		"status":     status,
		"headers":    headers,
		"body":       base64.StdEncoding.EncodeToString(body),
	})
}

func (c *Client) startHTTPStream(parent context.Context, session *relaySession, msg map[string]any) {
	reqID := stringField(msg, "request_id")
	streamCtx, cancel := context.WithCancel(parent)
	session.addStream(reqID, cancel)
	go func() {
		defer cancel()
		defer session.removeStream(reqID)
		c.handleHTTPStream(streamCtx, session, msg)
	}()
}

func (c *Client) handleHTTPStream(ctx context.Context, session *relaySession, msg map[string]any) {
	reqID := stringField(msg, "request_id")
	path := requestPath(msg)
	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		_ = session.write(ctx, map[string]any{"type": "http_stream_start", "request_id": reqID, "status": http.StatusNotFound, "headers": map[string]any{}})
		_ = session.write(ctx, map[string]any{"type": "http_stream_end", "request_id": reqID})
		return
	}
	req, err := c.localRequest(ctx, msg, nil)
	if err != nil {
		c.sendStreamError(ctx, session, reqID, err)
		return
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.recordErr(err)
			c.sendStreamError(ctx, session, reqID, err)
		}
		return
	}
	defer resp.Body.Close()
	if err := session.write(ctx, map[string]any{"type": "http_stream_start", "request_id": reqID, "status": resp.StatusCode, "headers": responseHeaders(resp.Header)}); err != nil {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if err := session.write(ctx, map[string]any{"type": "http_stream_chunk", "request_id": reqID, "body": base64.StdEncoding.EncodeToString(buf[:n])}); err != nil {
				return
			}
		}
		if readErr != nil {
			if readErr != io.EOF && ctx.Err() == nil {
				c.recordErr(readErr)
			}
			break
		}
	}
	_ = session.write(ctx, map[string]any{"type": "http_stream_end", "request_id": reqID})
}

func requestPath(msg map[string]any) string {
	if path := stringField(msg, "path"); path != "" {
		return path
	}
	return "/"
}

func (c *Client) localRequest(ctx context.Context, msg map[string]any, body io.Reader) (*http.Request, error) {
	method := stringField(msg, "method")
	if method == "" {
		method = http.MethodGet
	}
	u := c.localBaseURL + requestPath(msg)
	if query := stringField(msg, "query_string"); query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, msg["headers"])
	c.authorize(req)
	return req, nil
}

func responseHeaders(header http.Header) map[string]any {
	out := make(map[string]any, len(header))
	for key := range header {
		out[key] = header.Get(key)
	}
	return out
}

func (c *Client) sendStreamError(ctx context.Context, session *relaySession, reqID string, err error) {
	_ = session.write(ctx, map[string]any{"type": "http_stream_start", "request_id": reqID, "status": http.StatusBadGateway, "headers": map[string]any{}})
	_ = session.write(ctx, map[string]any{"type": "http_stream_chunk", "request_id": reqID, "body": base64.StdEncoding.EncodeToString([]byte(err.Error()))})
	_ = session.write(ctx, map[string]any{"type": "http_stream_end", "request_id": reqID})
}

func (c *Client) authorize(req *http.Request) {
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
}

func copyRequestHeaders(dst http.Header, raw any) {
	headers, _ := raw.(map[string]any)
	for key, value := range headers {
		if !strippedForwardHeaders[strings.ToLower(key)] {
			if text, ok := value.(string); ok {
				dst.Set(key, text)
			}
		}
	}
}

func stringField(msg map[string]any, key string) string {
	value, _ := msg[key].(string)
	return value
}

// handleRelayMessage forwards a cross-daemon relay_query/notify/broadcast to the
// matching local endpoint and returns a relay_response frame.
func (c *Client) handleRelayMessage(ctx context.Context, session *relaySession, msg map[string]any) {
	t, _ := msg["type"].(string)
	corr, _ := msg["correlation_id"].(string)
	src, _ := msg["source_daemon_id"].(string)
	endpoint := map[string]string{
		"relay_query":     "/query",
		"relay_notify":    "/notify",
		"relay_broadcast": "/broadcast",
	}[t]

	payload, _ := msg["payload"].(map[string]any)
	pb, _ := json.Marshal(payload)

	status := http.StatusBadGateway
	var respBody any = map[string]any{}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.localBaseURL+endpoint, bytes.NewReader(pb))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		c.authorize(req)
		resp, derr := c.httpc.Do(req)
		if derr != nil {
			c.recordErr(derr)
		} else {
			defer resp.Body.Close()
			status = resp.StatusCode
			raw, _ := io.ReadAll(resp.Body)
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				var j any
				if json.Unmarshal(raw, &j) == nil {
					respBody = j
				} else {
					respBody = map[string]any{"text": string(raw)}
				}
			} else {
				respBody = map[string]any{"text": string(raw)}
			}
		}
	}
	_ = session.write(ctx, map[string]any{
		"type":             "relay_response",
		"correlation_id":   corr,
		"source_daemon_id": src,
		"target_daemon_id": src,
		"status":           status,
		"body":             respBody,
	})
}
