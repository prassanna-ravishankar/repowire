package hub

import (
	"net/http"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

func (h *Hub) registerSessionControlRoutes(mux *http.ServeMux) {
	if h.store == nil {
		return
	}
	mux.HandleFunc("POST /sessions/{session_id}/controls/resume", h.requireAuth(h.handleResumeSession))
	mux.HandleFunc("POST /sessions/{session_id}/controls/notify", h.requireAuth(h.handleNotifySession))
	mux.HandleFunc("POST /sessions/resume", h.requireAuth(h.handleResumeSessionAlias))
}

type sessionResumeRequest struct {
	FromPeer          string  `json:"from_peer"`
	DryRun            *bool   `json:"dry_run"`
	Profile           *string `json:"profile"`
	Message           *string `json:"message"`
	RepowireSessionID string  `json:"repowire_session_id"`
}

func (h *Hub) handleResumeSessionAlias(w http.ResponseWriter, r *http.Request) {
	var req sessionResumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepowireSessionID == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "repowire_session_id is required")
		return
	}
	h.resumeSession(w, r, req.RepowireSessionID, req)
}
func (h *Hub) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	var req sessionResumeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	h.resumeSession(w, r, r.PathValue("session_id"), req)
}
func (h *Hub) resumeSession(w http.ResponseWriter, r *http.Request, id string, req sessionResumeRequest) {
	binding, err := h.store.GetSessionBinding(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if binding == nil {
		writeJSONError(w, http.StatusNotFound, "session_not_found: "+id)
		return
	}
	if binding.CurrentExecutorPeerID != nil {
		if peer, ok := h.reg.GetPeer(proto.PeerID(*binding.CurrentExecutorPeerID)); ok && (peer.Status == proto.StatusOnline || peer.Status == proto.StatusBusy) {
			writeJSONError(w, http.StatusConflict, "active_executor: session already has a live executor")
			return
		}
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	runtimeID := ""
	if binding.RuntimeSessionID != nil {
		runtimeID = *binding.RuntimeSessionID
	}
	backend := proto.AgentType(binding.Backend)
	plan, ok := service.ResolveLocalResume(backend, binding.ProjectPath, runtimeID, &binding.RepowireSessionID, binding.ResumeCapability)
	if !ok {
		writeJSONError(w, http.StatusConflict, "resume_unavailable: runtime session is unsupported or stale")
		return
	}
	response := map[string]any{"ok": true, "repowire_session_id": id, "session_status": binding.Status, "status": "resume_available", "capability": "supported", "message": "backend-native resume is available", "backend": binding.Backend, "runtime_session_id": binding.RuntimeSessionID, "runtime_source_uri": binding.RuntimeSourceURI, "executor_peer_id": binding.CurrentExecutorPeerID, "resume_capability": binding.ResumeCapability, "action": "inspect"}
	if dryRun {
		writeJSON(w, http.StatusOK, response)
		return
	}
	if h.spawn == nil || h.spawn.svc == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "spawn_service_unavailable")
		return
	}
	base, err := h.spawn.svc.ResolveCommand(backend, req.Profile)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	command, err := service.BuildResumeCommand(base, backend, runtimeID)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	role := proto.RoleAgent
	var peerID *proto.PeerID
	if binding.PeerID != nil {
		id := proto.PeerID(*binding.PeerID)
		peerID = &id
	}
	circle, _ := binding.Metadata["circle"].(string)
	if circle == "" && peerID != nil {
		if boundPeer, ok := h.reg.GetPeer(*peerID); ok {
			circle = boundPeer.Circle
		}
	}
	if circle == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "circle_unavailable: session has no recorded circle")
		return
	}
	spawned, err := h.spawn.svc.Spawn(service.SpawnConfig{Path: binding.ProjectPath, Backend: backend, Command: command, Circle: circle, Message: req.Message, Role: role, PeerID: peerID})
	if err != nil {
		h.writeSpawnError(w, err)
		return
	}
	response["action"] = "spawned"
	response["spawned_display_name"] = spawned.DisplayName
	response["tmux_session"] = spawned.TmuxSession
	response["pane_id"] = spawned.PaneID
	response["plan"] = plan
	writeJSON(w, http.StatusOK, response)
}

type sessionNotifyRequest struct {
	FromPeer     string           `json:"from_peer"`
	Text         string           `json:"text"`
	BypassCircle *bool            `json:"bypass_circle"`
	Attachments  []map[string]any `json:"attachments"`
}

func (h *Hub) handleNotifySession(w http.ResponseWriter, r *http.Request) {
	var req sessionNotifyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	binding, err := h.store.GetSessionBinding(r.Context(), r.PathValue("session_id"))
	if err != nil || binding == nil {
		writeJSONError(w, http.StatusNotFound, "session_not_found")
		return
	}
	if binding.CurrentExecutorPeerID == nil || h.messaging == nil {
		writeJSONError(w, http.StatusConflict, "session_executor_unavailable")
		return
	}
	from := req.FromPeer
	if from == "" {
		from = "dashboard"
	}
	bypass := true
	if req.BypassCircle != nil {
		bypass = *req.BypassCircle
	}
	result, err := h.messaging.delivery.Notify(r.Context(), service.NotifyParams{FromPeer: from, ToPeer: *binding.CurrentExecutorPeerID, Text: req.Text, BypassCircle: bypass, Attachments: req.Attachments})
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "repowire_session_id": binding.RepowireSessionID, "session_status": binding.Status, "capability": "active_executor", "executor_peer_id": binding.CurrentExecutorPeerID, "delivery_state": result.DeliveryState, "delivered": result.Delivered(), "queued": result.Queued(), "reason": result.Reason, "hook_delivery": result.HookDelivery})
}
