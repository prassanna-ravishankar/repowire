package hub

// routes_work.go — tracked-work + durable-job HTTP routes. Port of
// repowire/daemon/routes/work.py. Both /work and /jobs aliases are served with
// method-qualified ServeMux patterns. create_work merges the execution request,
// resolves assigned_peer_id
// via ResolvePeerStrict (404/409), routes cron → calendar else work_store, then
// service.JobRunner.Wake(). Terminal update_state/cancel release the executor via
// service.SessionControl. Wire shapes match the Python responses verbatim — clients read
// these keys.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// workRoutesRegistry is the narrow registry seam create_work needs to canonicalize
// an assigned-peer identifier into a peer_id. *peer.Registry satisfies it.
type workRoutesRegistry interface {
	ResolvePeerStrict(identifier string, circle *string) []*proto.Peer
}

// *peer.Registry satisfies the work routes' assigned-peer resolver seam.
var _ workRoutesRegistry = (*peer.Registry)(nil)

// workRoutes bundles the work-route deps, wired onto the Hub via WithWork.
type workRoutes struct {
	store   *state.Store
	runner  *service.JobRunner
	control *service.SessionControl
	reg     workRoutesRegistry
}

// WithWork attaches the work/jobs route group. runner drives dispatch + Wake;
// control releases executors on terminal transitions; reg canonicalizes assigned
// peers. nil store → the routes 503. Returns the hub for chaining; call before Routes.
func (h *Hub) WithWork(runner *service.JobRunner, store *state.Store, reg workRoutesRegistry) *Hub {
	h.work = &workRoutes{store: store, runner: runner, reg: reg}
	if runner != nil {
		h.work.control = runner.Control()
	}
	return h
}

// registerWorkRoutes attaches the work/jobs handlers. The bare "/work" + "/jobs"
// collection endpoints (POST create, GET list) and the "/work/"/"/jobs/" subtrees
// (per-id status/update/run/retry/cancel/result) are dispatched by suffix.
func (h *Hub) registerWorkRoutes(mux *http.ServeMux) {
	for _, base := range []string{"/work", "/jobs"} {
		mux.HandleFunc("POST "+base, h.requireAuth(h.handleCreateWork))
		mux.HandleFunc("GET "+base, h.requireAuth(h.handleListWork))
		mux.HandleFunc("GET "+base+"/{work_id}", h.requireAuth(h.handleWorkStatus))
		mux.HandleFunc("PATCH "+base+"/{work_id}", h.requireAuth(h.handleWorkUpdate))
		mux.HandleFunc("GET "+base+"/{work_id}/{$}", h.requireAuth(h.handleWorkStatus))
		mux.HandleFunc("PATCH "+base+"/{work_id}/{$}", h.requireAuth(h.handleWorkUpdate))
		mux.HandleFunc("GET "+base+"/{work_id}/status", h.requireAuth(h.handleWorkStatus))
		mux.HandleFunc("GET "+base+"/{work_id}/result", h.requireAuth(h.handleWorkResult))
		mux.HandleFunc("POST "+base+"/{work_id}/run", h.requireAuth(h.handleWorkRun))
		mux.HandleFunc("POST "+base+"/{work_id}/retry", h.requireAuth(h.handleWorkRetry))
		mux.HandleFunc("POST "+base+"/{work_id}/cancel", h.requireAuth(h.handleWorkCancel))
	}
}

// ----------------------------------------------------------------------------
// Request/response bodies (wire shapes mirror work.py pydantic models).
// ----------------------------------------------------------------------------

type workCreateRequest struct {
	Title             string         `json:"title"`
	Kind              string         `json:"kind"`
	CreatedByPeerID   *string        `json:"created_by_peer_id"`
	OwnerPeerID       *string        `json:"owner_peer_id"`
	AssignedPeerID    *string        `json:"assigned_peer_id"`
	RepowireSessionID *string        `json:"repowire_session_id"`
	CorrelationID     *string        `json:"correlation_id"`
	Circle            *string        `json:"circle"`
	SourceKind        *string        `json:"source_kind"`
	SourceID          *string        `json:"source_id"`
	Scope             *string        `json:"scope"`
	Visibility        string         `json:"visibility"`
	Request           map[string]any `json:"request"`
	DeadlineAt        *string        `json:"deadline_at"`
	ExpiresAt         *string        `json:"expires_at"`
	Provenance        map[string]any `json:"provenance"`
	Prompt            *string        `json:"prompt"`
	PromptFile        *string        `json:"prompt_file"`
	Path              *string        `json:"path"`
	Backend           *string        `json:"backend"`
	Profile           *string        `json:"profile"`
	DueAt             *string        `json:"due_at"`
	Cron              *string        `json:"cron"`
	ResultSurface     *string        `json:"result_surface"`
	ProcessScope      *string        `json:"process_scope"`
	Continuity        *string        `json:"continuity"`
}

type workCancelRequest struct {
	RequestedByPeerID *string `json:"requested_by_peer_id"`
	Reason            string  `json:"reason"`
}

type workUpdateRequest struct {
	State         string         `json:"state"`
	AttemptID     *string        `json:"attempt_id"`
	StateReason   *string        `json:"state_reason"`
	Phase         *string        `json:"phase"`
	Progress      map[string]any `json:"progress"`
	ProgressNote  *string        `json:"progress_note"`
	ResultSummary *string        `json:"result_summary"`
	ResultData    map[string]any `json:"result_data"`
	Error         map[string]any `json:"error"`
	Artifacts     []any          `json:"artifacts"`
	Provenance    map[string]any `json:"provenance"`
}

type workListRequest struct {
	State             *string
	OwnerPeerID       *string
	CreatedByPeerID   *string
	RepowireSessionID *string
	Circle            *string
	View              string
}

func (h *Hub) workRouteReady() error {
	if h.work == nil || h.work.store == nil {
		return routeErr(http.StatusServiceUnavailable, "work store not configured")
	}
	return nil
}

// ----------------------------------------------------------------------------
// Collection: POST /work|/jobs (create), GET /work|/jobs (list)
// ----------------------------------------------------------------------------

func (h *Hub) handleCreateWork(w http.ResponseWriter, r *http.Request) {
	var req workCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.jobCreate(r.Context(), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jobCreate creates one tracked or recurring job and returns the HTTP response
// body without depending on an HTTP request. MCP uses this same path.
func (h *Hub) jobCreate(ctx context.Context, req workCreateRequest) (map[string]any, error) {
	if err := h.workRouteReady(); err != nil {
		return nil, err
	}
	if req.DueAt != nil && req.Cron != nil {
		return nil, routeErr(http.StatusBadRequest, "provide due_at or cron, not both")
	}
	assigned, code, detail := h.canonicalAssignedPeer(req.AssignedPeerID, req.Circle)
	if code != 0 {
		return nil, routeErr(code, detail)
	}
	merged, code, detail := mergeExecutionRequest(&req, assigned)
	if code != 0 {
		return nil, routeErr(code, detail)
	}

	if req.Cron != nil {
		norm, err := service.ValidateCron(*req.Cron)
		if err != nil {
			return nil, routeErr(http.StatusBadRequest, err.Error())
		}
		next, err := service.NextFireAfter(norm, time.Now().UTC())
		if err != nil {
			return nil, routeErr(http.StatusBadRequest, err.Error())
		}
		entry, err := h.work.store.CreateCalendarEntry(ctx, &state.CalendarEntry{
			Title:           req.Title,
			Kind:            orDefault(req.Kind, "general"),
			Cron:            norm,
			NextDueAt:       next.Format("2006-01-02T15:04:05.000000-07:00"),
			OwnerPeerID:     req.OwnerPeerID,
			AssignedPeerID:  assigned,
			Circle:          req.Circle,
			CreatedByPeerID: req.CreatedByPeerID,
			SourceKind:      req.SourceKind,
			SourceID:        req.SourceID,
			Scope:           req.Scope,
			Visibility:      orDefault(req.Visibility, "circle"),
			Request:         merged,
			Provenance:      req.Provenance,
		})
		if err != nil {
			return nil, routeErr(http.StatusInternalServerError, err.Error())
		}
		h.wakeRunner()
		return map[string]any{
			"calendar_id":  entry.CalendarID,
			"recurring_id": entry.CalendarID,
			"calendar":     entry.Status(),
		}, nil
	}

	work, err := h.work.store.CreateWork(ctx, state.WorkCreate{
		Title:             req.Title,
		Kind:              req.Kind,
		CreatedByPeerID:   req.CreatedByPeerID,
		OwnerPeerID:       req.OwnerPeerID,
		AssignedPeerID:    assigned,
		RepowireSessionID: req.RepowireSessionID,
		CorrelationID:     req.CorrelationID,
		Circle:            req.Circle,
		SourceKind:        req.SourceKind,
		SourceID:          req.SourceID,
		Scope:             req.Scope,
		Visibility:        req.Visibility,
		Request:           merged,
		DeadlineAt:        req.DeadlineAt,
		ExpiresAt:         req.ExpiresAt,
		Provenance:        req.Provenance,
	})
	if err != nil {
		return nil, routeErr(http.StatusInternalServerError, err.Error())
	}
	h.wakeRunner()
	return map[string]any{
		"job_id":  work.WorkID,
		"work_id": work.WorkID,
		"status":  work.Status(),
	}, nil
}

func (h *Hub) handleListWork(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	result, err := h.jobList(r.Context(), workListRequest{
		State:             optQuery(q, "state"),
		OwnerPeerID:       optQuery(q, "owner_peer_id"),
		CreatedByPeerID:   optQuery(q, "created_by_peer_id"),
		RepowireSessionID: optQuery(q, "repowire_session_id"),
		Circle:            optQuery(q, "circle"),
		View:              q.Get("view"),
	})
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jobList returns the HTTP list response without parsing query parameters.
func (h *Hub) jobList(ctx context.Context, req workListRequest) (map[string]any, error) {
	if err := h.workRouteReady(); err != nil {
		return nil, err
	}
	if req.View != "" && req.View != "full" && req.View != "summary" {
		return nil, routeErr(http.StatusBadRequest, "view must be one of: full, summary")
	}
	items, storeErr := h.work.store.ListWork(ctx, state.WorkFilter{
		State: req.State, OwnerPeerID: req.OwnerPeerID, CreatedByPeerID: req.CreatedByPeerID,
		RepowireSessionID: req.RepowireSessionID, Circle: req.Circle,
	})
	if storeErr != nil {
		return nil, routeErr(http.StatusBadRequest, storeErr.Error())
	}

	var recurring []*state.CalendarEntry
	stateFilter := derefString(req.State)
	if stateFilter == "" || stateFilter == "active" || stateFilter == "paused" || stateFilter == "cancelled" {
		recurring, storeErr = h.work.store.ListCalendarEntries(ctx, state.CalendarFilter{
			State: req.State, OwnerPeerID: req.OwnerPeerID, CreatedByPeerID: req.CreatedByPeerID, Circle: req.Circle,
		})
		if storeErr != nil {
			return nil, routeErr(http.StatusInternalServerError, storeErr.Error())
		}
	}

	summarize := req.View == "summary"
	workOut := make([]map[string]any, 0, len(items))
	for _, item := range items {
		s := item.Status()
		if summarize {
			s = summaryStatus(s)
		}
		workOut = append(workOut, s)
	}
	recurringOut := make([]map[string]any, 0, len(recurring))
	for _, item := range recurring {
		s := item.Status()
		if summarize {
			s = summaryStatus(s)
		}
		recurringOut = append(recurringOut, s)
	}
	return map[string]any{"work": workOut, "recurring": recurringOut}, nil
}

// ----------------------------------------------------------------------------
// Item: /work/{id}[/status|/run|/retry|/cancel|/result], /jobs aliases
// ----------------------------------------------------------------------------

func (h *Hub) handleWorkStatus(w http.ResponseWriter, r *http.Request) {
	result, err := h.jobStatus(r.Context(), r.PathValue("work_id"))
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jobStatus returns one job's HTTP status response without an HTTP request.
func (h *Hub) jobStatus(ctx context.Context, workID string) (map[string]any, error) {
	if err := h.workRouteReady(); err != nil {
		return nil, err
	}
	if strings.HasPrefix(workID, "cal-") {
		entry, err := h.work.store.GetCalendarEntry(ctx, workID)
		if err != nil {
			return nil, routeErr(http.StatusInternalServerError, err.Error())
		}
		if entry == nil {
			return nil, routeErr(http.StatusNotFound, "No recurring job: "+workID)
		}
		return map[string]any{"status": entry.Status()}, nil
	}
	work, err := h.work.store.GetWork(ctx, workID)
	if err != nil {
		return nil, routeErr(http.StatusInternalServerError, err.Error())
	}
	if work == nil {
		return nil, routeErr(http.StatusNotFound, "No work: "+workID)
	}
	return map[string]any{"status": work.Status()}, nil
}

func (h *Hub) handleWorkResult(w http.ResponseWriter, r *http.Request) {
	result, err := h.jobResult(r.Context(), r.PathValue("work_id"))
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jobResult returns one job's HTTP result response without an HTTP request.
func (h *Hub) jobResult(ctx context.Context, workID string) (map[string]any, error) {
	if err := h.workRouteReady(); err != nil {
		return nil, err
	}
	if strings.HasPrefix(workID, "cal-") {
		entry, err := h.work.store.GetCalendarEntry(ctx, workID)
		if err != nil {
			return nil, routeErr(http.StatusInternalServerError, err.Error())
		}
		if entry == nil {
			return nil, routeErr(http.StatusNotFound, "No recurring job: "+workID)
		}
		return map[string]any{
			"result": map[string]any{"result_state": "recurring_template", "calendar": entry.Status()},
		}, nil
	}
	work, err := h.work.store.GetWork(ctx, workID)
	if err != nil {
		return nil, routeErr(http.StatusInternalServerError, err.Error())
	}
	if work == nil {
		return nil, routeErr(http.StatusNotFound, "No work: "+workID)
	}
	return map[string]any{"result": work.Result()}, nil
}

func (h *Hub) handleWorkUpdate(w http.ResponseWriter, r *http.Request) {
	var req workUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.jobUpdate(r.Context(), r.PathValue("work_id"), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jobUpdate applies one lifecycle update and returns the HTTP response body.
func (h *Hub) jobUpdate(ctx context.Context, workID string, req workUpdateRequest) (map[string]any, error) {
	if err := h.workRouteReady(); err != nil {
		return nil, err
	}
	work, err := h.work.store.UpdateWorkState(ctx, workID, state.WorkUpdate{
		State:         req.State,
		StateReason:   req.StateReason,
		Phase:         req.Phase,
		Progress:      req.Progress,
		ProgressNote:  req.ProgressNote,
		ResultSummary: req.ResultSummary,
		ResultData:    req.ResultData,
		Error:         req.Error,
		Artifacts:     req.Artifacts,
		Provenance:    req.Provenance,
		AttemptID:     req.AttemptID,
	})
	if err != nil {
		switch {
		case err == state.ErrStaleAttempt:
			return nil, routeErr(http.StatusConflict, "stale attempt_id")
		case err == state.ErrAttemptIDRequired:
			return nil, routeErr(http.StatusBadRequest, err.Error())
		default:
			return nil, routeErr(http.StatusBadRequest, err.Error())
		}
	}
	if work == nil {
		return nil, routeErr(http.StatusNotFound, "No work: "+workID)
	}
	if work.Terminal() {
		work = h.releaseIfTerminal(ctx, work, work.State, req.AttemptID)
	}
	return map[string]any{"status": work.Status()}, nil
}

func (h *Hub) handleWorkRun(w http.ResponseWriter, r *http.Request) {
	if err := h.workRouteReady(); err != nil {
		writeRouteError(w, err)
		return
	}
	workID := r.PathValue("work_id")
	if strings.HasPrefix(workID, "cal-") {
		writeError(w, http.StatusConflict, "Recurring job templates cannot be run")
		return
	}
	ctx := r.Context()
	existing, err := h.work.store.GetWork(ctx, workID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "No work: "+workID)
		return
	}
	if !runnableState(existing.State) {
		writeError(w, http.StatusConflict, "Job cannot be run from state "+existing.State)
		return
	}
	h.runAndRespond(w, ctx, workID, false)
}

func (h *Hub) handleWorkRetry(w http.ResponseWriter, r *http.Request) {
	if err := h.workRouteReady(); err != nil {
		writeRouteError(w, err)
		return
	}
	workID := r.PathValue("work_id")
	if strings.HasPrefix(workID, "cal-") {
		writeError(w, http.StatusConflict, "Recurring job templates cannot be retried")
		return
	}
	ctx := r.Context()
	existing, err := h.work.store.GetWork(ctx, workID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "No work: "+workID)
		return
	}
	if !retryableState(existing.State) {
		writeError(w, http.StatusConflict, "Job cannot be retried from state "+existing.State)
		return
	}
	h.runAndRespond(w, ctx, workID, true)
}

func (h *Hub) handleWorkCancel(w http.ResponseWriter, r *http.Request) {
	var req workCancelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := h.jobCancel(r.Context(), r.PathValue("work_id"), req)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// jobCancel cancels one job and returns the HTTP response body.
func (h *Hub) jobCancel(ctx context.Context, workID string, req workCancelRequest) (map[string]any, error) {
	if err := h.workRouteReady(); err != nil {
		return nil, err
	}
	if strings.HasPrefix(workID, "cal-") {
		entry, err := h.work.store.CancelCalendarEntry(ctx, workID, req.Reason)
		if err != nil {
			return nil, routeErr(http.StatusInternalServerError, err.Error())
		}
		if entry == nil {
			return nil, routeErr(http.StatusNotFound, "No recurring job: "+workID)
		}
		h.wakeRunner()
		return map[string]any{"status": entry.Status()}, nil
	}
	existing, err := h.work.store.GetWork(ctx, workID)
	if err != nil {
		return nil, routeErr(http.StatusInternalServerError, err.Error())
	}
	work, err := h.work.store.CancelWork(ctx, workID, req.RequestedByPeerID, req.Reason)
	if err != nil {
		return nil, routeErr(http.StatusInternalServerError, err.Error())
	}
	if work == nil {
		return nil, routeErr(http.StatusNotFound, "No work: "+workID)
	}
	// In-flight (non-terminal, past-queued) cancel: best-effort terminal release.
	// ponytail: the ACP protocol-cancel branch (work.py _attempt_protocol_cancel)
	// is dead until the ACP transport lands — no daemon-owned live session handle
	// exists for WS/hook peers — so we go straight to the terminal release path
	// that the release_handle (per-fire spawn) drives. Wire protocol-cancel in
	// alongside the ACP transport.
	if existing != nil && !existing.Terminal() && existing.State != "queued" {
		current := h.currentAttemptID(work)
		if current != "" || hasReleaseHandle(work) {
			if hasReleaseHandle(work) && h.work.control != nil {
				release, _ := h.work.control.ReleaseExecutorForWork(ctx, work, "cancel_requested")
				work = h.mergeReleaseResult(ctx, work, release, current)
			}
			var attemptPtr *string
			if current != "" {
				attemptPtr = &current
			}
			updated, uerr := h.work.store.UpdateWorkState(ctx, workID, state.WorkUpdate{
				State:       "cancelled",
				StateReason: &req.Reason,
				Phase:       strPtr("cancelled"),
				Progress:    work.Progress,
				Provenance:  work.Provenance,
				AttemptID:   attemptPtr,
			})
			if uerr == nil && updated != nil {
				work = updated
			}
		}
	}
	return map[string]any{"status": work.Status()}, nil
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

func (h *Hub) runAndRespond(w http.ResponseWriter, ctx context.Context, workID string, retry bool) {
	if h.work.runner == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "job runner not configured")
		return
	}
	work, err := h.work.runner.RunJob(ctx, workID, true, retry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if work == nil {
		work, _ = h.work.store.GetWork(ctx, workID)
	}
	if work == nil {
		writeError(w, http.StatusNotFound, "No work: "+workID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": work.Status()})
}

// canonicalAssignedPeer resolves a display-name/peer-id assigned target to a
// peer_id. repow- ids and nil pass through. Returns (peerID, httpCode, detail):
// code 0 means success. Mirrors _canonical_assigned_peer (404/409).
func (h *Hub) canonicalAssignedPeer(identifier, circle *string) (*string, int, any) {
	if identifier == nil || *identifier == "" {
		return nil, 0, nil
	}
	if strings.HasPrefix(*identifier, "repow-") {
		return identifier, 0, nil
	}
	if h.work.reg == nil {
		// No resolver wired: pass through verbatim (best-effort).
		return identifier, 0, nil
	}
	resolved := h.work.reg.ResolvePeerStrict(*identifier, circle)
	if len(resolved) == 0 {
		return nil, http.StatusNotFound, map[string]any{"error": "assigned_peer_not_found", "peer": *identifier}
	}
	if len(resolved) > 1 {
		candidates := make([]map[string]any, 0, len(resolved))
		for _, p := range resolved {
			candidates = append(candidates, map[string]any{
				"peer_id": string(p.PeerID), "display_name": string(p.DisplayName), "circle": p.Circle,
			})
		}
		return nil, http.StatusConflict, map[string]any{"error": "ambiguous_assigned_peer", "peer": *identifier, "candidates": candidates}
	}
	pid := string(resolved[0].PeerID)
	return &pid, 0, nil
}

func (h *Hub) releaseIfTerminal(ctx context.Context, work *state.TrackedWork, terminalReason string, attemptID *string) *state.TrackedWork {
	if h.work.control == nil || !hasReleaseHandle(work) {
		return work
	}
	release, err := h.work.control.ReleaseExecutorForWork(ctx, work, terminalReason)
	if err != nil {
		return work
	}
	return h.mergeReleaseResult(ctx, work, release, derefString(attemptID))
}

// mergeReleaseResult stamps the release result onto provenance.release and
// persists it, leaving the work state unchanged. Mirrors merge_release_result +
// the update_state(state=work.state) write in work.py.
func (h *Hub) mergeReleaseResult(ctx context.Context, work *state.TrackedWork, release map[string]any, attemptID string) *state.TrackedWork {
	if release == nil {
		return work
	}
	prov := cloneAny(work.Provenance)
	prov["release"] = release
	var attemptPtr *string
	if attemptID != "" {
		attemptPtr = &attemptID
	}
	updated, err := h.work.store.UpdateWorkState(ctx, work.WorkID, state.WorkUpdate{
		State:       work.State,
		StateReason: work.StateReason,
		Phase:       work.Phase,
		Progress:    work.Progress,
		Provenance:  prov,
		AttemptID:   attemptPtr,
	})
	if err != nil || updated == nil {
		return work
	}
	return updated
}

func (h *Hub) currentAttemptID(work *state.TrackedWork) string {
	runner := mapAtAny(work.Provenance, "runner")
	v, _ := runner["current_attempt_id"].(string)
	return v
}

func (h *Hub) wakeRunner() {
	if h.work != nil && h.work.runner != nil {
		h.work.runner.Wake()
	}
}

// hasReleaseHandle reports whether the current attempt carries a per-fire release
// handle. Mirrors job_release.has_release_handle.
func hasReleaseHandle(work *state.TrackedWork) bool {
	runner := mapAtAny(work.Provenance, "runner")
	cur, _ := runner["current_attempt_id"].(string)
	for _, raw := range anySlice(runner["attempts"]) {
		attempt, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := attempt["attempt_id"].(string); id != cur {
			continue
		}
		acquisition := mapAtAny(attempt, "acquisition")
		if rh, ok := acquisition["release_handle"].(map[string]any); ok && rh != nil {
			return true
		}
	}
	return false
}

func runnableState(s string) bool {
	switch s {
	case "queued", "failed", "unavailable":
		return true
	}
	return false
}

func retryableState(s string) bool {
	switch s {
	case "failed", "unavailable", "delivered":
		return true
	}
	return false
}

func summaryStatus(status map[string]any) map[string]any {
	keys := []string{
		"job_id", "work_id", "calendar_id", "recurring_id", "title", "kind",
		"state", "state_reason", "phase", "owner_peer_id", "assigned_peer_id",
		"repowire_session_id", "correlation_id", "circle", "created_by_peer_id",
		"source_kind", "source_id", "scope", "visibility", "created_at", "updated_at",
		"deadline_at", "expires_at", "due_at", "cron", "next_due_at",
		"last_occurrence_work_id", "last_materialized_at", "result_summary",
		"cancel_requested", "cancel_requested_at", "cancel_requested_by_peer_id",
		"cancellation_reason",
	}
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := status[k]; ok {
			out[k] = v
		}
	}
	if execution, ok := status["execution"].(map[string]any); ok {
		exec := map[string]any{}
		for _, k := range []string{"target", "delivery", "process_scope", "continuity"} {
			if v, ok := execution[k]; ok {
				exec[k] = v
			}
		}
		out["execution"] = exec
	}
	return out
}

// mergeExecutionRequest threads prompt/target/schedule/delivery/process_scope/
// continuity into request.execution, applying the per_fire/resume defaults.
// Returns (merged, httpCode, detail); code 0 means success. Mirrors
// _merge_execution_request.
func mergeExecutionRequest(req *workCreateRequest, assigned *string) (map[string]any, int, any) {
	body := cloneAny(req.Request)
	execution := cloneAny(mapAtAny(body, "execution"))
	prompt := cloneAny(mapAtAny(execution, "prompt"))
	switch {
	case req.PromptFile != nil && *req.PromptFile != "":
		content, err := readPromptFile(*req.PromptFile)
		if err != nil {
			return nil, http.StatusBadRequest, err.Error()
		}
		prompt["body"] = content
		prompt["source"] = "file"
		prompt["source_path"] = *req.PromptFile
	case req.Prompt != nil:
		prompt["body"] = *req.Prompt
		prompt["source"] = "inline"
	case len(prompt) == 0:
		prompt["body"] = req.Title
		prompt["source"] = "title"
	}
	target := cloneAny(mapAtAny(execution, "target"))
	if req.Path != nil {
		target["path"] = *req.Path
	}
	if req.Backend != nil {
		target["backend"] = *req.Backend
	}
	if req.Profile != nil {
		target["profile"] = *req.Profile
	}
	if assigned != nil {
		target["assigned_peer_id"] = *assigned
	}
	schedule := cloneAny(mapAtAny(execution, "schedule"))
	if req.DueAt != nil {
		schedule["due_at"] = *req.DueAt
	}
	delivery := cloneAny(mapAtAny(execution, "delivery"))
	if _, ok := delivery["kind"]; !ok {
		delivery["kind"] = "ask"
	}
	if req.ResultSurface != nil {
		delivery["result_surface"] = *req.ResultSurface
	}

	processScope := ""
	if req.ProcessScope != nil {
		processScope = *req.ProcessScope
	} else if v, ok := execution["process_scope"].(string); ok {
		processScope = v
	}
	if processScope == "" && assigned == nil && target["path"] != nil && target["backend"] != nil {
		processScope = "per_fire"
	}
	if processScope != "" {
		if processScope == "per-fire" {
			processScope = "per_fire"
		}
		if processScope != "per_fire" && processScope != "persistent" {
			return nil, http.StatusBadRequest, "process_scope must be one of: per_fire, persistent"
		}
		execution["process_scope"] = processScope
	}

	continuity := ""
	if req.Continuity != nil {
		continuity = *req.Continuity
	} else if v, ok := execution["continuity"].(string); ok {
		continuity = v
	}
	if continuity == "" && processScope == "per_fire" {
		if req.Cron != nil {
			continuity = "resume"
		} else {
			continuity = "fresh"
		}
	}
	if continuity != "" {
		if continuity != "resume" && continuity != "fresh" {
			return nil, http.StatusBadRequest, "continuity must be one of: resume, fresh"
		}
		execution["continuity"] = continuity
	}

	execution["prompt"] = prompt
	execution["target"] = target
	execution["schedule"] = schedule
	execution["delivery"] = delivery
	body["execution"] = execution
	return body, 0, nil
}

func readPromptFile(path string) (string, error) {
	expanded := path
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		expanded = home
		if path != "~" {
			expanded = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	content, err := os.ReadFile(expanded)
	if err != nil {
		return "", fmt.Errorf("read prompt_file %q: %w", path, err)
	}
	return string(content), nil
}

func optQuery(q url.Values, key string) *string {
	v := q.Get(key)
	if v == "" {
		return nil
	}
	return &v
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// mapAtAny, anySlice, and cloneAny are duplicated from
// service/session_control.go (which owns the canonical definitions) because
// this route file is on the hub side of the hub/service split — not worth an
// exported seam for four generic map helpers.

func mapAtAny(m map[string]any, key string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func anySlice(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

func cloneAny(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
