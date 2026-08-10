package hub

import (
	"net/http"
)

func (h *Hub) registerTraceRoutes(mux *http.ServeMux) {
	if h.store != nil {
		mux.HandleFunc("GET /traces/{trace_id}", h.requireAuth(h.handleGetTrace))
	}
}

func (h *Hub) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.StagesFor(r.Context(), r.PathValue("trace_id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(rows) == 0 {
		writeJSONError(w, http.StatusNotFound, "No delivery trace for: "+r.PathValue("trace_id"))
		return
	}
	stages := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		stages = append(stages, map[string]any{"seq": row.Seq, "stage": row.Stage, "status": row.Status, "peer_id": nilIfEmpty(row.PeerID), "from_peer_id": nilIfEmpty(row.FromPeerID), "ts": row.TS, "detail": row.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"trace_id": r.PathValue("trace_id"), "kind": rows[0].Kind, "stages": stages})
}
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
