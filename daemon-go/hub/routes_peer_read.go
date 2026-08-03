package hub

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
)

// ============================================================================
// peer-read HTTP route group: GET /peers, /peers/{identifier},
// /peers/by-pane/{pane_id}, /circles/{name}/orchestrator.
//
// Read-only mirror of repowire/daemon/routes/peers.py (list_peers, get_peer,
// get_peer_by_pane, get_circle_orchestrator). The JSON wire shapes match the
// Python PeerInfo / OrchestratorStatusResponse exactly — dashboard, CLI, and
// MCP clients depend on the field names.
// ============================================================================

// inbound-status classification values. Mirror daemon/diagnostics.py constants.
const (
	inboundOffline          = "offline"
	inboundPaneUnsafe       = "pane_unsafe"
	inboundNoHook           = "no_hook"
	inboundLegacyUnverified = "legacy_unverified"
	inboundDegraded         = "inbound_degraded"
	inboundOnline           = "online"
)

// terminal hook-receipt stages used to derive inbound health. Mirror Python
// DeliveryTraceStore default stages.
const (
	stagePaneInjected        = "pane_injected"
	stageThreadInputAccepted = "thread_input_accepted"
	stageInjectionFailed     = "injection_failed"
)

// PeerInfo is the peer wire shape for HTTP responses. Field names + JSON tags
// match Python's PeerInfo (Name is the back-compat alias of DisplayName). The
// inbound-health block defaults to the "no probe" values so the shape is stable
// even when the read deps are absent.
type PeerInfo struct {
	PeerID      proto.PeerID      `json:"peer_id"`
	Name        proto.DisplayName `json:"name"` // back-compat: == display_name
	DisplayName proto.DisplayName `json:"display_name"`
	Path        *string           `json:"path"`
	Machine     *string           `json:"machine"`
	TmuxSession *string           `json:"tmux_session"`
	Backend     proto.AgentType   `json:"backend"`
	Model       *string           `json:"model"`
	Circle      string            `json:"circle"`
	Role        proto.PeerRole    `json:"role"`
	Status      string            `json:"status"`
	TurnState   *proto.TurnState  `json:"turn_state"`
	LastSeen    *string           `json:"last_seen"`
	Metadata    map[string]any    `json:"metadata"`
	Description string            `json:"description"`

	// inbound health
	WSConnected               bool     `json:"ws_connected"`
	HookSupportsReceipts      bool     `json:"hook_supports_receipts"`
	LastSuccessfulInjectionAt *string  `json:"last_successful_injection_at"`
	LastInjectionFailureAt    *string  `json:"last_injection_failure_at"`
	PendingAskCount           int      `json:"pending_ask_count"`
	OldestPendingAgeSeconds   *float64 `json:"oldest_pending_age_seconds"`
	PaneSafe                  *bool    `json:"pane_safe"`
	InboundStatus             string   `json:"inbound_status"`
}

// PeersResponse wraps the list-peers result.
type PeersResponse struct {
	Peers []PeerInfo `json:"peers"`
}

// OrchestratorStatusResponse is the /circles/{name}/orchestrator wire shape.
type OrchestratorStatusResponse struct {
	Circle            string  `json:"circle"`
	Present           bool    `json:"present"`
	PeerID            *string `json:"peer_id"`
	PeerName          *string `json:"peer_name"`
	LastSeen          *string `json:"last_seen"`
	StaleAfterSeconds int     `json:"stale_after_seconds"`
}

// registerPeerReadRoutes wires the read endpoints onto the mux, each gated by
// requireAuth. The {identifier} / {pane_id} / {name} path params use Go 1.22+
// pattern wildcards (r.PathValue).
func (h *Hub) registerPeerReadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /peers", h.requireAuth(h.listPeers))
	mux.HandleFunc("GET /peers/{rest...}", h.requireAuth(h.getPeerSubpath))
	mux.HandleFunc("GET /circles/{name}/orchestrator", h.requireAuth(h.getCircleOrchestrator))
}

func (h *Hub) getPeerSubpath(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.PathValue("rest"), "/"), "/")
	if len(parts) == 2 && parts[0] == "by-pane" {
		r.SetPathValue("pane_id", parts[1])
		h.getPeerByPane(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "doctor" {
		r.SetPathValue("identifier", parts[0])
		h.getPeerDoctor(w, r)
		return
	}
	if len(parts) == 1 && parts[0] != "" {
		r.SetPathValue("identifier", parts[0])
		h.getPeer(w, r)
		return
	}
	writeJSONError(w, http.StatusNotFound, "Peer route not found")
}

func (h *Hub) getPeerDoctor(w http.ResponseWriter, r *http.Request) {
	h.reg.LazyRepair(r.Context())
	circleValue := r.URL.Query().Get("circle")
	var circle *string
	if circleValue != "" {
		circle = &circleValue
	}
	peer, err := h.reg.ResolvePeer(r.PathValue("identifier"), circle)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if peer == nil {
		writeJSONError(w, http.StatusNotFound, "Peer not found: "+r.PathValue("identifier"))
		return
	}
	host, _ := os.Hostname()
	local := peer.Machine == host || ((peer.Machine == "" || peer.Machine == "unknown") && peer.PaneID != nil)
	report := map[string]any{"peer_id": peer.PeerID, "display_name": peer.DisplayName, "status": peer.Status, "turn_state": peer.TurnState, "last_seen": peer.LastSeen, "machine": peer.Machine, "backend": peer.Backend, "circle": peer.Circle, "role": peer.Role, "pane_id": peer.PaneID, "is_local_machine": local, "ws_connected": h.transport.IsConnected(peer.PeerID), "contradictions": []map[string]string{}}
	if pane, ok := h.transport.ConnectionPaneID(peer.PeerID); ok {
		report["ws_pane_id"] = pane
	}
	contradictions := []map[string]string{}
	if local && (peer.Status == proto.StatusOnline || peer.Status == proto.StatusBusy) && !h.transport.IsConnected(peer.PeerID) {
		contradictions = append(contradictions, contradiction("ONLINE_BUT_NO_WS", "error", fmt.Sprintf("peer is %s but has no live WebSocket connection", peer.Status)))
	}
	if local && peer.PaneID != nil && *peer.PaneID != "" {
		exists := false
		if h.spawn != nil && h.spawn.svc != nil {
			exists = h.spawn.svc.Tmux().ProbePane(*peer.PaneID) != nil
		}
		report["tmux_pane_exists"] = exists
		if !exists {
			contradictions = append(contradictions, contradiction("PANE_MISSING", "error", "registry pane "+*peer.PaneID+" is not present in tmux"))
		}
		meta := clienthooks.ReadPaneRuntimeMetadata(*peer.PaneID)
		report["hook_meta_available"] = len(meta) > 0
		report["hook_meta_peer_id"] = meta["peer_id"]
		report["hook_meta_display_name"] = meta["display_name"]
		if id, _ := meta["peer_id"].(string); id != "" && id != string(peer.PeerID) {
			contradictions = append(contradictions, contradiction("HOOK_PEERID_MISMATCH", "error", "pane hook metadata peer_id "+id+" differs from registry peer_id "+string(peer.PeerID)))
		}
	}
	if local && peer.AgentPID != nil {
		alive := *peer.AgentPID > 0 && syscall.Kill(*peer.AgentPID, 0) == nil
		report["agent_pid"] = peer.AgentPID
		report["agent_pid_alive"] = alive
		if !alive {
			contradictions = append(contradictions, contradiction("AGENT_PID_DEAD", "error", fmt.Sprintf("agent pid %d is not a live process", *peer.AgentPID)))
		}
	}
	if wsPane, ok := report["ws_pane_id"].(string); ok && peer.PaneID != nil && wsPane != *peer.PaneID {
		contradictions = append(contradictions, contradiction("WS_PANE_MISMATCH", "warning", "websocket pane differs from registry pane"))
	}
	if h.asks != nil {
		pending, _ := h.asks.PendingForPeer(r.Context(), peer.PeerID, 50, "inbound")
		report["pending_inbound_count"] = len(pending)
		if len(pending) > 0 {
			oldest := pending[len(pending)-1]
			age := time.Since(oldest.CreatedAt).Seconds()
			report["oldest_pending_age_seconds"] = age
			report["oldest_pending_cid"] = oldest.CorrelationID
			if age > 1800 {
				contradictions = append(contradictions, contradiction("STALE_PENDING_ASK", "warning", fmt.Sprintf("oldest pending inbound ask %s has been open ~%dm", oldest.CorrelationID, int(age/60))))
			}
		}
	}
	report["contradictions"] = contradictions
	writeJSON(w, http.StatusOK, report)
}
func contradiction(code, severity, detail string) map[string]string {
	return map[string]string{"code": code, "severity": severity, "detail": detail}
}

// listPeers handles GET /peers with optional status/path/backend/circle filters.
// Kicks lazy_repair first (maintenance piggy-backs on the request), then builds
// PeerInfo with derived inbound-health using ONE batched LatestStagesForPeers
// query rather than 2N per-peer DB hits.
func (h *Hub) listPeers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.reg.LazyRepair(ctx)
	peers := h.reg.GetAllPeers()

	q := r.URL.Query()
	if status := q.Get("status"); status == "online" {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return p.Status == proto.StatusOnline || p.Status == proto.StatusBusy
		})
	} else if status == "offline" {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return p.Status == proto.StatusOffline
		})
	}
	if path := q.Get("path"); path != "" {
		peers = filterByPath(peers, path)
	}
	if backend := q.Get("backend"); backend != "" {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return string(p.Backend) == backend
		})
	}
	if circle := q.Get("circle"); circle != "" && circle != "*" {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return p.Circle == circle || p.Role.BypassesCircles()
		})
	}

	injectionTimes := h.injectionTimesFor(ctx, peers)
	infos := make([]PeerInfo, 0, len(peers))
	for _, p := range peers {
		infos = append(infos, h.peerToInfoWithHealth(ctx, p, injectionTimes))
	}
	writeJSON(w, http.StatusOK, PeersResponse{Peers: infos})
}

// getPeer handles GET /peers/{identifier} (optional ?circle=). 404 when unknown,
// 409 when the display_name is ambiguous (registry returns an error).
func (h *Hub) getPeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identifier := r.PathValue("identifier")
	var circle *string
	if c := r.URL.Query().Get("circle"); c != "" {
		circle = &c
	}
	p, err := h.reg.ResolvePeer(identifier, circle)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if p == nil {
		writeJSONError(w, http.StatusNotFound, "Peer not found: "+identifier)
		return
	}
	injectionTimes := h.injectionTimesFor(ctx, []*proto.Peer{p})
	writeJSON(w, http.StatusOK, h.peerToInfoWithHealth(ctx, p, injectionTimes))
}

// getPeerByPane handles GET /peers/by-pane/{pane_id}. 404 when no peer owns it.
func (h *Hub) getPeerByPane(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	paneID := r.PathValue("pane_id")
	p, ok := h.reg.GetPeerByPane(paneID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "No peer for pane: "+paneID)
		return
	}
	injectionTimes := h.injectionTimesFor(ctx, []*proto.Peer{p})
	writeJSON(w, http.StatusOK, h.peerToInfoWithHealth(ctx, p, injectionTimes))
}

// getCircleOrchestrator handles GET /circles/{name}/orchestrator. present=true
// iff a live orchestrator (role=orchestrator, online/busy, fresh last_seen)
// exists in the circle.
func (h *Hub) getCircleOrchestrator(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tolerance := int(h.reg.HeartbeatTolerance().Seconds())
	resp := OrchestratorStatusResponse{
		Circle:            name,
		Present:           false,
		StaleAfterSeconds: tolerance,
	}
	if orch, ok := h.reg.GetOrchestrator(name); ok {
		pid := string(orch.PeerID)
		pname := string(orch.DisplayName)
		resp.Present = true
		resp.PeerID = &pid
		resp.PeerName = &pname
		resp.LastSeen = isoOrNil(orch.LastSeen)
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- helpers ---------------------------------------------------------------

// injectionTimesFor batches the latest pane_injected/injection_failed timestamps
// for all peers in ONE query (avoids 2N DB hits). Returns nil when the store is
// absent; callers treat a nil map as "no observed injections".
func (h *Hub) injectionTimesFor(ctx context.Context, peers []*proto.Peer) map[[2]string]string {
	if h.store == nil || len(peers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, string(p.PeerID))
	}
	times, err := h.store.LatestStagesForPeers(ctx, ids)
	if err != nil {
		// Fail loud in the log, but don't 500 a read: degrade to no-health.
		return nil
	}
	return times
}

// peerToInfoWithHealth projects a Peer to PeerInfo and fills the inbound-health
// block: ws_connected (transport), hook_supports_receipts (advertised cap OR an
// observed injection trace row), last success/failure injection times (from the
// batched ledger snapshot), pending-ask count + oldest age (AskTracker), and the
// classified inbound_status. pane_safe is left nil (unprobed) in read views.
func (h *Hub) peerToInfoWithHealth(ctx context.Context, p *proto.Peer, injectionTimes map[[2]string]string) PeerInfo {
	info := peerToInfo(p)

	wsConnected := h.transport != nil && h.transport.IsConnected(p.PeerID)

	advertises := proto.HasCapability(p.Metadata, proto.CapDeliveryReceipts)
	lastSuccess := newestStage(injectionTimes, p.PeerID, stagePaneInjected, stageThreadInputAccepted)
	lastFailure := lookupStage(injectionTimes, p.PeerID, stageInjectionFailed)
	observedReceipt := lastSuccess != nil || lastFailure != nil
	hookSupportsReceipts := advertises || observedReceipt

	var pendingCount int
	var oldestAge *float64
	if h.asks != nil {
		pending, err := h.asks.PendingForPeer(ctx, p.PeerID, 50, "inbound")
		if err == nil {
			pendingCount = len(pending)
			if pendingCount > 0 {
				// PendingForPeer returns newest first; oldest is the last entry.
				oldest := pending[len(pending)-1]
				age := time.Since(oldest.CreatedAt).Seconds()
				oldestAge = &age
			}
		}
	}

	info.WSConnected = wsConnected
	info.HookSupportsReceipts = hookSupportsReceipts
	info.LastSuccessfulInjectionAt = lastSuccess
	info.LastInjectionFailureAt = lastFailure
	info.PendingAskCount = pendingCount
	info.OldestPendingAgeSeconds = oldestAge
	info.PaneSafe = nil // unprobed in read views
	info.InboundStatus = computeInboundStatus(inboundStatusInputs{
		isOffline:            p.Status == proto.StatusOffline,
		wsConnected:          wsConnected,
		hookSupportsReceipts: hookSupportsReceipts,
		paneSafe:             nil,
		lastSuccessAt:        lastSuccess,
		lastFailureAt:        lastFailure,
	})
	return info
}

// peerToInfo is the no-health projection (mirrors Python _peer_to_info).
func peerToInfo(p *proto.Peer) PeerInfo {
	var path *string
	if p.Path != "" {
		path = &p.Path
	}
	var machine *string
	if p.Machine != "" {
		machine = &p.Machine
	}
	var turn *proto.TurnState
	if p.TurnState != proto.TurnUnknown {
		ts := p.TurnState
		turn = &ts
	}
	meta := p.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return PeerInfo{
		PeerID:        p.PeerID,
		Name:          p.DisplayName,
		DisplayName:   p.DisplayName,
		Path:          path,
		Machine:       machine,
		TmuxSession:   p.TmuxSession,
		Backend:       p.Backend,
		Model:         p.Model,
		Circle:        p.Circle,
		Role:          p.Role,
		Status:        string(p.Status),
		TurnState:     turn,
		LastSeen:      isoOrNil(p.LastSeen),
		Metadata:      meta,
		Description:   p.Description,
		InboundStatus: inboundOffline,
	}
}

type inboundStatusInputs struct {
	isOffline            bool
	wsConnected          bool
	hookSupportsReceipts bool
	paneSafe             *bool
	lastSuccessAt        *string
	lastFailureAt        *string
}

// computeInboundStatus classifies inbound-delivery reachability. Precedence
// (first match wins): offline -> pane_unsafe -> no_hook -> legacy_unverified ->
// inbound_degraded -> online. Mirrors daemon/diagnostics.py compute_inbound_status.
func computeInboundStatus(in inboundStatusInputs) string {
	if in.isOffline {
		return inboundOffline
	}
	if in.wsConnected && in.paneSafe != nil && !*in.paneSafe {
		return inboundPaneUnsafe
	}
	if !in.wsConnected {
		return inboundNoHook
	}
	if !in.hookSupportsReceipts {
		return inboundLegacyUnverified
	}
	if in.lastFailureAt != nil &&
		(in.lastSuccessAt == nil || *in.lastFailureAt > *in.lastSuccessAt) {
		return inboundDegraded
	}
	return inboundOnline
}

func newestStage(times map[[2]string]string, id proto.PeerID, stages ...string) *string {
	// RFC3339Nano UTC timestamps sort lexically in time order.
	var newest *string
	for _, stage := range stages {
		if value := lookupStage(times, id, stage); value != nil && (newest == nil || *value > *newest) {
			newest = value
		}
	}
	return newest
}

// lookupStage returns the ts for (peer_id, stage) from the batched snapshot, or
// nil when absent.
func lookupStage(times map[[2]string]string, id proto.PeerID, stage string) *string {
	if times == nil {
		return nil
	}
	if ts, ok := times[[2]string{string(id), stage}]; ok {
		return &ts
	}
	return nil
}

// filterPeers keeps peers matching pred.
func filterPeers(peers []*proto.Peer, pred func(*proto.Peer) bool) []*proto.Peer {
	out := peers[:0:0]
	for _, p := range peers {
		if pred(p) {
			out = append(out, p)
		}
	}
	return out
}

// filterByPath matches peers whose path equals the filter exactly, plus a
// realpath-normalized fallback for the remainder (symlink-resolved). Mirrors
// list_peers' string_matches + resolved_map fallback.
func filterByPath(peers []*proto.Peer, path string) []*proto.Peer {
	target := realPath(path)
	out := make([]*proto.Peer, 0, len(peers))
	for _, p := range peers {
		if p.Path == path {
			out = append(out, p)
			continue
		}
		if p.Path != "" && realPath(p.Path) == target {
			out = append(out, p)
		}
	}
	return out
}

// realPath resolves symlinks to an absolute path, falling back to the input on
// error (mirrors os.path.realpath's best-effort behavior).
func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// isoOrNil renders a time as RFC3339-ish ISO-8601 (matching Python isoformat),
// or nil.
func isoOrNil(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05.999999-07:00")
	return &s
}

// writeJSON / writeError are shared response helpers defined once for the hub
// package (see server.go and routes_ask_lifecycle.go). Not redeclared here.
