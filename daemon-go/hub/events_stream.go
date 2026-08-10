package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// events_stream.go owns GET /events/stream — the Server-Sent Events fan-out of
// the dashboard event window. It mirrors the Python daemon.routes.messages
// stream_events handler: event-driven, never a poll. The handler subscribes to
// the registry's event fan-out, flushes the currently-buffered window, then
// blocks on the subscribe channel (waking on every AddEvent) with a periodic
// comment-frame keepalive so both ends notice a dead socket.
//
// Wire shape: each event is the same map the GET /events route emits
// (routes_events.go / peer.Registry.GetEvents), serialised as a single SSE
// `data:` frame. Clients (the dashboard) depend on this exact framing.

// sseHeartbeat is the keepalive interval. Long enough that idle connections are
// not chatty, short enough that proxies/clients notice a dead socket. Matches
// the Python SSE_HEARTBEAT_SECS.
const sseHeartbeat = 15 * time.Second

// sseWriteTimeout bounds each write batch (initial replay, wake-driven batch,
// keepalive): a wedged client must not park the handler goroutine forever.
const sseWriteTimeout = 30 * time.Second

// EventsStreamRoutes registers GET /events/stream behind the shared bearer-token
// gate. Kept separate from EventRoutes so the SSE handler (which holds the
// connection open) is wired with one line and the base events group is
// untouched. Registered after EventRoutes in Hub.Routes.
func (h *Hub) EventsStreamRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /events/stream", h.requireAuth(h.handleEventsStream))
}

// handleEventsStream streams dashboard events over SSE.
//
// Subscribe-before-flush so an event added concurrently with the initial flush
// still wakes the loop. The loop selects on: the request context (client gone),
// the subscribe channel (drain EventsSince(cursor), advance cursor), and a 15s
// ticker (write a `: keepalive` comment frame). Requires an http.Flusher; 500 if
// the ResponseWriter cannot flush (fail loud rather than silently buffer forever).
func (h *Hub) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	// Subscribe before the initial flush so events added during the flush still
	// raise the wake channel for the next loop iteration.
	wake, unsubscribe := h.reg.SubscribeEvents()
	defer unsubscribe()

	// rc lets us bound each write batch below; SetWriteDeadline may return
	// ErrNotSupported for exotic ResponseWriter wrappers — best-effort hardening,
	// so that error is ignored rather than aborting the stream.
	rc := http.NewResponseController(w)

	var lastEventID string
	initial := h.reg.GetEvents()
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	for _, ev := range initial {
		if !writeSSEEvent(w, ev) {
			return
		}
	}
	if n := len(initial); n > 0 {
		lastEventID, _ = initial[n-1]["id"].(string)
	}
	flusher.Flush()
	// Drain any wake raised during the initial flush: we have already delivered
	// everything currently buffered, so collapse a stale signal.
	select {
	case <-wake:
	default:
	}

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			newEvents := h.reg.EventsSince(lastEventID)
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			for _, ev := range newEvents {
				if !writeSSEEvent(w, ev) {
					return
				}
			}
			if n := len(newEvents); n > 0 {
				lastEventID, _ = newEvents[n-1]["id"].(string)
			}
			flusher.Flush()
		case <-ticker.C:
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent serialises an event map as a single SSE data frame. Returns false
// if the JSON encode or the write fails (the connection is then abandoned by the
// caller). A frame that cannot be encoded is skipped rather than aborting the
// stream — a single malformed event must not kill a live dashboard feed.
func writeSSEEvent(w http.ResponseWriter, ev map[string]any) bool {
	payload, err := json.Marshal(ev)
	if err != nil {
		return true // skip the unencodable event, keep the stream alive
	}
	_, werr := fmt.Fprintf(w, "data: %s\n\n", payload)
	return werr == nil
}
