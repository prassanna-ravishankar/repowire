package hub

import (
	"net/http"
	"strings"

	"github.com/repowire/repowire/daemon-go/state"
)

// registerPushRoutes exposes the native-app device registry. The app reaches it
// through the relay tunnel; the daemon owns the tokens and the relay only sends.
func (h *Hub) registerPushRoutes(mux *http.ServeMux) {
	if h.store == nil {
		return
	}
	mux.HandleFunc("GET /push/devices", h.requireAuth(h.handleListPushDevices))
	mux.HandleFunc("POST /push/devices", h.requireAuth(h.handleRegisterPushDevice))
	mux.HandleFunc("DELETE /push/devices/{token}", h.requireAuth(h.handleDeletePushDevice))
}

func (h *Hub) handleListPushDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.store.ListPushDevices(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (h *Hub) handleRegisterPushDevice(w http.ResponseWriter, r *http.Request) {
	var req state.PushDevice
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Token = strings.ToLower(strings.TrimSpace(req.Token))
	if req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token is required")
		return
	}
	if req.Environment == "" {
		req.Environment = "production"
	}
	if req.Environment != "sandbox" && req.Environment != "production" {
		writeJSONError(w, http.StatusBadRequest, "environment must be 'sandbox' or 'production'")
		return
	}
	req.RegisteredAt = ""
	if err := h.store.UpsertPushDevice(r.Context(), req); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Hub) handleDeletePushDevice(w http.ResponseWriter, r *http.Request) {
	removed, err := h.store.DeletePushDevices(r.Context(), strings.ToLower(r.PathValue("token")))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}
