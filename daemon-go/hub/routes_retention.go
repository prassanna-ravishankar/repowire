package hub

import (
	"errors"
	"net/http"

	"github.com/repowire/repowire/daemon-go/service"
)

// routes_retention.go owns the operator surface of the retention policy
// (rfcs/retention.md):
//
//	GET  /retention        policy, last sweep, and a dry run of the next
//	POST /retention/sweep  run a sweep now ({"dry_run": bool, "vacuum": bool})

func (h *Hub) registerRetentionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /retention", h.requireAuth(h.handleRetention))
	mux.HandleFunc("POST /retention/sweep", h.requireAuth(h.handleRetentionSweep))
}

type retentionSweepRequest struct {
	DryRun bool `json:"dry_run"`
	Vacuum bool `json:"vacuum"`
}

func (h *Hub) handleRetention(w http.ResponseWriter, r *http.Request) {
	if h.retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention needs the state store")
		return
	}
	next, err := h.retention.Sweep(r.Context(), true, false)
	if err != nil && !errors.Is(err, service.ErrSweepRunning) {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": h.retention.Status(), "next_sweep": next.Counts})
}

func (h *Hub) handleRetentionSweep(w http.ResponseWriter, r *http.Request) {
	if h.retention == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "retention needs the state store")
		return
	}
	var req retentionSweepRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	report, err := h.retention.Sweep(r.Context(), req.DryRun, req.Vacuum)
	if errors.Is(err, service.ErrSweepRunning) {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	body := map[string]any{"dry_run": req.DryRun, "counts": report.Counts, "errors": report.Errors}
	if err != nil {
		body["error"] = err.Error()
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}
