package hub

// routes_shares.go owns the "shares" HTTP route group: a thin proxy to the relay
// share-token HTTP API. Port of repowire/daemon/routes/shares.py.
//
//	POST   /shares       create a share token via the relay → {share_id, url, ...}
//	GET    /shares       list active share tokens for this daemon's relay user
//	DELETE /shares/{id}  revoke a share token via the relay
//
// Each handler proxies <relay_http_base>/api/v1/share with an x-api-key header
// and rewrites the response url to <base>/s/<share_id>. When the relay is not
// configured the endpoints degrade exactly as Python does: 503 for POST/DELETE,
// empty list for GET.
//
// RelayConfig is intentionally the narrow resolved view the route needs; main
// fills it from the Go config loader.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// RelayConfig is the minimal relay configuration the shares proxy needs. A nil
// *RelayConfig (or Enabled=false / empty APIKey) means "relay not configured".
type RelayConfig struct {
	Enabled bool
	URL     string // ws(s):// relay url; rewritten to http(s) for the HTTP API
	APIKey  string
}

// shareRequest mirrors the Python ShareRequest body.
type shareRequest struct {
	PeerName    string `json:"peer_name"`
	Permissions string `json:"permissions"`
	TTLSecs     *int   `json:"ttl_secs"`
}

// WithShares wires the relay config onto the hub, enabling the /shares routes.
// A nil cfg leaves the relay-not-configured behaviour (503 / empty list).
func (h *Hub) WithShares(cfg *RelayConfig) *Hub {
	h.relay = cfg
	return h
}

// relayHTTPAndKey returns (httpBase, apiKey, true) when the relay is configured,
// else ("", "", false). Mirrors Python _relay_http_and_key: wss→https, ws→http,
// trailing slash trimmed.
func (h *Hub) relayHTTPAndKey() (string, string, bool) {
	cfg := h.relay
	if cfg == nil || !cfg.Enabled || cfg.APIKey == "" {
		return "", "", false
	}
	base := cfg.URL
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.Replace(base, "ws://", "http://", 1)
	base = strings.TrimRight(base, "/")
	return base, cfg.APIKey, true
}

func (h *Hub) registerShareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /shares", h.requireAuth(h.createShare))
	mux.HandleFunc("GET /shares", h.requireAuth(h.listShares))
	mux.HandleFunc("DELETE /shares/{share_id}", h.requireAuth(h.handleRevokeShare))
}

// relayClient is the shared HTTP client for relay proxy calls (10s timeout, same
// as the Python httpx.AsyncClient(timeout=10.0)).
var relayClient = &http.Client{Timeout: 10 * time.Second}

func (h *Hub) createShare(w http.ResponseWriter, r *http.Request) {
	var req shareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "malformed request: "+err.Error())
		return
	}
	data, err := h.createShareDirect(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (h *Hub) createShareDirect(ctx context.Context, req shareRequest) (map[string]any, error) {
	base, apiKey, ok := h.relayHTTPAndKey()
	if !ok {
		return nil, routeErr(http.StatusServiceUnavailable, "Relay not configured. Run `repowire setup --relay` first.")
	}
	if req.Permissions == "" {
		req.Permissions = "ro"
	}
	body, _ := json.Marshal(map[string]any{
		"peer_name":   req.PeerName,
		"permissions": req.Permissions,
		"ttl_secs":    req.TTLSecs,
	})
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/share", bytes.NewReader(body))
	if err != nil {
		return nil, routeErr(http.StatusBadGateway, "relay request build failed: "+err.Error())
	}
	upstream.Header.Set("x-api-key", apiKey)
	upstream.Header.Set("Content-Type", "application/json")
	resp, err := relayClient.Do(upstream)
	if err != nil {
		return nil, routeErr(http.StatusBadGateway, "relay unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, routeErr(resp.StatusCode, string(raw))
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, routeErr(http.StatusBadGateway, "malformed relay response")
	}
	if shareID, sok := data["share_id"].(string); sok {
		data["url"] = base + "/s/" + shareID
	}
	return data, nil
}

func (h *Hub) listShares(w http.ResponseWriter, r *http.Request) {
	base, apiKey, ok := h.relayHTTPAndKey()
	if !ok {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	upstream, err := http.NewRequest(http.MethodGet, base+"/api/v1/share", nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "relay request build failed: "+err.Error())
		return
	}
	upstream.Header.Set("x-api-key", apiKey)
	resp, err := relayClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "relay unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		writeError(w, resp.StatusCode, string(raw))
		return
	}
	var tokens []map[string]any
	if err := json.Unmarshal(raw, &tokens); err != nil {
		writeError(w, http.StatusBadGateway, "malformed relay response")
		return
	}
	for _, t := range tokens {
		if shareID, sok := t["share_id"].(string); sok {
			t["url"] = base + "/s/" + shareID
		}
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *Hub) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("share_id")
	if shareID == "" {
		writeError(w, http.StatusNotFound, "share id required")
		return
	}
	data, err := h.revokeShareDirect(r.Context(), shareID)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (h *Hub) revokeShareDirect(ctx context.Context, shareID string) (map[string]any, error) {
	base, apiKey, ok := h.relayHTTPAndKey()
	if !ok {
		return nil, routeErr(http.StatusServiceUnavailable, "Relay not configured")
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/v1/share/"+shareID, nil)
	if err != nil {
		return nil, routeErr(http.StatusBadGateway, "relay request build failed: "+err.Error())
	}
	upstream.Header.Set("x-api-key", apiKey)
	resp, err := relayClient.Do(upstream)
	if err != nil {
		return nil, routeErr(http.StatusBadGateway, "relay unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return nil, routeErr(resp.StatusCode, string(raw))
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		// Relay returned a non-JSON 200/404; pass through an empty object rather
		// than erroring (revoke is idempotent).
		return map[string]any{}, nil
	}
	return data, nil
}
