package hub

import (
	"errors"
	"net/http"

	"github.com/repowire/repowire/daemon-go/service"
)

func (h *Hub) registerACPPermissionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /acp/permissions/{request_id}/decision", h.requireAuth(h.handleACPPermissionDecision))
}

type acpPermissionDecisionRequest struct {
	Outcome  string  `json:"outcome"`
	OptionID *string `json:"option_id,omitempty"`
	Message  *string `json:"message,omitempty"`
}

func (h *Hub) handleACPPermissionDecision(w http.ResponseWriter, r *http.Request) {
	if h.asks == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ACP permission broker not initialized")
		return
	}
	requestID := r.PathValue("request_id")
	ask, ok := h.asks.Get(requestID)
	if !ok || ask.Closed || ask.Question == nil || ask.Question["scope"] != "tool_permission" {
		writeJSONError(w, http.StatusNotFound, "ACP permission request not found: "+requestID)
		return
	}
	var request acpPermissionDecisionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Outcome != "allowed" && request.Outcome != "denied" && request.Outcome != "cancelled" {
		writeJSONError(w, http.StatusUnprocessableEntity, "outcome must be allowed, denied, or cancelled")
		return
	}
	var answer service.Answer
	if request.Outcome == "allowed" {
		valid := map[string]bool{}
		if options, ok := ask.Question["options"].([]any); ok {
			for _, raw := range options {
				if option, ok := raw.(map[string]any); ok {
					if id, ok := option["id"].(string); ok && id != "" {
						valid[id] = true
						if request.OptionID == nil {
							value := id
							request.OptionID = &value
						}
					}
				}
			}
		}
		if request.OptionID == nil {
			writeJSONError(w, http.StatusBadRequest, "Allowed ACP permission decision requires an option_id")
			return
		}
		if !valid[*request.OptionID] {
			writeJSONError(w, http.StatusBadRequest, "Unknown ACP permission option_id: "+*request.OptionID)
			return
		}
		answer = service.Answer{Outcome: "answered", OptionID: request.OptionID, Message: request.Message}
	} else {
		answer = service.Answer{Outcome: request.Outcome, Message: request.Message}
	}
	if _, err := h.asks.Answer(r.Context(), requestID, answer); err != nil {
		if errors.Is(err, service.ErrAskNotFound) || errors.Is(err, service.ErrAlreadyAnswered) {
			writeJSONError(w, http.StatusNotFound, "ACP permission request not found: "+requestID)
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "request_id": requestID, "outcome": request.Outcome, "option_id": request.OptionID})
}
