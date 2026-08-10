package hub

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

func (h *Hub) registerPeerMCPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /peers/{name}/mcp", h.requireAuth(h.handleListPeerMCP))
	mux.HandleFunc("POST /peers/{name}/mcp", h.requireAuth(h.handleAddPeerMCP))
	mux.HandleFunc("DELETE /peers/{name}/mcp/{server_name}", h.requireAuth(h.handleRemovePeerMCP))
}

func (h *Hub) resolveLocalMCPPeer(w http.ResponseWriter, r *http.Request) (*proto.Peer, map[string]any, bool) {
	var circle *string
	if value := r.URL.Query().Get("circle"); value != "" {
		circle = &value
	}
	peer, err := h.reg.GetPeerByName(r.PathValue("name"), circle)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return nil, nil, false
	}
	if peer == nil {
		writeJSONError(w, http.StatusNotFound, "Peer not found: "+r.PathValue("name"))
		return nil, nil, false
	}
	scope, err := service.MCPConfigScope(peer.Backend)
	if err != nil {
		writeJSONError(w, http.StatusNotImplemented, err.Error())
		return nil, nil, false
	}
	host, _ := os.Hostname()
	sameHost := peer.Machine == "" || peer.Machine == host
	scope["peer_id"], scope["peer_name"], scope["project_path"] = peer.PeerID, peer.DisplayName, peer.Path
	scope["peer_machine"], scope["self_machine"], scope["same_host"] = peer.Machine, host, sameHost
	if !sameHost {
		writeJSONError(w, http.StatusConflict, map[string]any{"error": "cross_host", "hint": "Per-peer MCP config is same-host only.", "peer_machine": peer.Machine, "self_machine": host, "config_scope": scope})
		return nil, nil, false
	}
	return peer, scope, true
}

func (h *Hub) handleListPeerMCP(w http.ResponseWriter, r *http.Request) {
	peer, scope, ok := h.resolveLocalMCPPeer(w, r)
	if !ok {
		return
	}
	entries, err := service.ListPeerMCP(r.Context(), peer)
	if err != nil {
		writePeerMCPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": entries, "config_scope": scope})
}

func (h *Hub) handleAddPeerMCP(w http.ResponseWriter, r *http.Request) {
	peer, scope, ok := h.resolveLocalMCPPeer(w, r)
	if !ok {
		return
	}
	var request service.MCPServerSpec
	if !decodeJSON(w, r, &request) {
		return
	}
	request.Scope = r.URL.Query().Get("scope")
	if request.Scope == "" {
		request.Scope = "user"
	}
	supported := false
	for _, value := range scope["supported_scopes"].([]string) {
		if value == request.Scope {
			supported = true
		}
	}
	if !supported {
		writeJSONError(w, http.StatusConflict, map[string]any{"error": "unsupported_scope", "requested_scope": request.Scope, "supported_scopes": scope["supported_scopes"], "config_scope": scope})
		return
	}
	if err := service.AddPeerMCP(r.Context(), peer, request); err != nil {
		writePeerMCPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config_scope": scope})
}

func (h *Hub) handleRemovePeerMCP(w http.ResponseWriter, r *http.Request) {
	peer, scope, ok := h.resolveLocalMCPPeer(w, r)
	if !ok {
		return
	}
	if err := service.RemovePeerMCP(r.Context(), peer, r.PathValue("server_name")); err != nil {
		writePeerMCPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config_scope": scope})
}

func writePeerMCPError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, service.ErrMCPUnsupported):
		status = http.StatusNotImplemented
	case errors.Is(err, service.ErrMCPDuplicate):
		status = http.StatusConflict
	case errors.Is(err, service.ErrMCPNotFound):
		status = http.StatusNotFound
	case strings.Contains(err.Error(), "timed out"):
		status = http.StatusGatewayTimeout
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "must contain") || strings.Contains(err.Error(), "invalid"):
		status = http.StatusBadRequest
	}
	writeJSONError(w, status, err.Error())
}
