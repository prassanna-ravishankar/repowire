package hub

// Scheduled check-in HTTP route group. Port of repowire/daemon/routes/schedules.py.
//
//	POST   /schedules         create a one-shot or recurring scheduled check-in
//	GET    /schedules         list schedules (optionally filtered by from_peer)
//	DELETE /schedules/{id}    cancel a pending schedule
//
// Wire shapes match the Python daemon field-for-field (CLI/MCP/bot clients
// depend on them). Create requires exactly one of fire_at|cron (400 otherwise):
// fire_at is parsed as ISO-8601 (naive → UTC); the cron path computes the next
// fire externally (service.NextFireAfter) then hands the resolved time to the store. Bad
// cron / unknown kind → 400. After any mutation the scheduler is woken (the
// notify_changed analogue) so its deadline-driven loop recomputes — never a poll.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// scheduleStore is the data layer the routes touch. *state.Store satisfies it.
type scheduleStore interface {
	CreateSchedule(ctx context.Context, fromPeer, toPeer, text string, fireAt time.Time, kind string, circle, cron *string) (*state.Schedule, error)
	ListSchedules(ctx context.Context, fromPeer *string) ([]*state.Schedule, error)
	DeleteSchedule(ctx context.Context, scheduleID string) (*state.Schedule, error)
}

// scheduleWaker is the wake seam (the scheduler's Wake). *Scheduler satisfies it.
type scheduleWaker interface{ Wake() }

// Register attaches the endpoints to the mux, each wrapped by the hub's auth
// middleware. POST/GET share the "/schedules" pattern (dispatched by method);
// DELETE uses the trailing-id pattern.
func (h *Hub) registerScheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /schedules", h.requireAuth(h.handleScheduleCreate))
	mux.HandleFunc("GET /schedules", h.requireAuth(h.handleScheduleList))
	mux.HandleFunc("DELETE /schedules/{schedule_id}", h.requireAuth(h.handleScheduleByID))
}

// ----------------------------------------------------------------------------
// Wire types — match daemon/routes/schedules.py field-for-field.
// ----------------------------------------------------------------------------

type scheduleCreateRequest struct {
	FromPeer string  `json:"from_peer"`
	ToPeer   string  `json:"to_peer"`
	Text     string  `json:"text"`
	FireAt   *string `json:"fire_at"`
	Cron     *string `json:"cron"`
	Kind     string  `json:"kind"`
	Circle   *string `json:"circle"`
}

type scheduleResponse = state.Schedule

type scheduleListResponse struct {
	Schedules []*state.Schedule `json:"schedules"`
}

// ----------------------------------------------------------------------------
// Handlers
// ----------------------------------------------------------------------------

func (h *Hub) handleScheduleCreate(w http.ResponseWriter, r *http.Request) {
	var req scheduleCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	resp, err := h.createSchedule(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Hub) createSchedule(ctx context.Context, req scheduleCreateRequest) (*state.Schedule, error) {
	if h == nil || h.scheduleStore == nil || h.scheduleWaker == nil {
		return nil, routeErr(http.StatusServiceUnavailable, "schedules not configured")
	}

	// Exactly one of fire_at|cron. (nil == nil) and (set && set) both rejected.
	if (req.FireAt == nil) == (req.Cron == nil) {
		return nil, routeErr(http.StatusBadRequest, "provide exactly one of fire_at or cron")
	}

	kind := req.Kind
	if kind == "" {
		kind = "notify"
	}

	var (
		fireAt time.Time
		cron   *string
	)
	if req.Cron != nil {
		// Validate + compute the first fire externally, mirroring create_cron →
		// next_fire_after. Store the normalized cron so reschedule reparses cleanly.
		norm, err := service.ValidateCron(*req.Cron)
		if err != nil {
			return nil, routeErr(http.StatusBadRequest, err.Error())
		}
		next, err := service.NextFireAfter(norm, time.Now().UTC())
		if err != nil {
			return nil, routeErr(http.StatusBadRequest, err.Error())
		}
		fireAt = next
		cron = &norm
	} else {
		parsed, err := parseFireAt(*req.FireAt)
		if err != nil {
			return nil, routeErr(http.StatusBadRequest, err.Error())
		}
		fireAt = parsed
	}

	sched, err := h.scheduleStore.CreateSchedule(
		ctx, req.FromPeer, req.ToPeer, req.Text, fireAt, kind, req.Circle, cron,
	)
	if err != nil {
		// Invalid kind (and any other store-side validation) → 400, matching the
		// Python ValueError → HTTPException(400) mapping.
		return nil, routeErr(http.StatusBadRequest, err.Error())
	}

	h.scheduleWaker.Wake()
	return sched, nil
}

func (h *Hub) handleScheduleList(w http.ResponseWriter, r *http.Request) {
	var fromPeer *string
	if v := r.URL.Query().Get("from_peer"); v != "" {
		fromPeer = &v
	}
	out, err := h.listSchedules(r.Context(), fromPeer)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Hub) listSchedules(ctx context.Context, fromPeer *string) (scheduleListResponse, error) {
	if h == nil || h.scheduleStore == nil {
		return scheduleListResponse{}, routeErr(http.StatusServiceUnavailable, "schedules not configured")
	}
	scheds, err := h.scheduleStore.ListSchedules(ctx, fromPeer)
	if err != nil {
		return scheduleListResponse{}, routeErr(http.StatusInternalServerError, err.Error())
	}
	return scheduleListResponse{Schedules: scheds}, nil
}

// handleScheduleByID handles DELETE /schedules/{id}.
func (h *Hub) handleScheduleByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("schedule_id")
	if id == "" {
		writeError(w, http.StatusNotFound, "No schedule: ")
		return
	}
	err := h.deleteSchedule(r.Context(), id)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Hub) deleteSchedule(ctx context.Context, id string) error {
	if h == nil || h.scheduleStore == nil || h.scheduleWaker == nil {
		return routeErr(http.StatusServiceUnavailable, "schedules not configured")
	}
	removed, err := h.scheduleStore.DeleteSchedule(ctx, id)
	if err != nil {
		return routeErr(http.StatusInternalServerError, err.Error())
	}
	if removed == nil {
		return routeErr(http.StatusNotFound, "No schedule: "+id)
	}
	h.scheduleWaker.Wake()
	return nil
}

// parseFireAt parses an ISO-8601 fire_at. Mirrors _parse_fire_at: a naive
// datetime (no offset) is treated as UTC; an explicit offset is honored and
// converted to UTC. Returns the parsed UTC time, or an error → 400.
func parseFireAt(raw string) (time.Time, error) {
	// Layouts with an explicit offset/zone first (honored), then naive (→ UTC).
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("fire_at must be ISO-8601")
}
