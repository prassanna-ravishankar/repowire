package hub

// Tmux-lifecycle hooks (pane / session / window / client).
//
// Provider-agnostic lifecycle endpoints that react to tmux events by mutating
// peer state through the Registry. Mirrors the Python
// repowire/daemon/routes/lifecycle.py + lifecycle_handler.LifecycleHandler.
//
// All endpoints are localhost-only (require_localhost, NOT require_auth): the
// tmux hook scripts run on the same host as the daemon and post unauthenticated.
//
// Load-bearing behaviour preserved from the Python port:
//   - pane-died: forget spawn-ownership for the pane, offline the pane's peer
//     (NON-terminal — pane death is recoverable), sever its socket, clear pane
//     runtime state. 200 even when no peer occupies the pane.
//   - session-closed is EVIDENCE-GATED, not event-trusting (commit b9e5a66):
//     tmux's global session-closed hook resolves #{session_name} against the
//     SURVIVING attached session, so a transient session exit can name a circle
//     whose agents are all still alive. We probe live panes ONCE; an empty/
//     unreachable probe is INCONCLUSIVE (do nothing — refuse to treat "no
//     evidence" as "all gone"); a still-live named session is a spurious close
//     (do nothing); otherwise offline only peers whose own pane is absent.
//   - session-renamed: re-circle peers by pane id.
//   - window-renamed: NO-OP (renaming would strip the backend suffix and collide
//     names; session registration is the sole naming source).
//   - client-detached: log only.

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// nameFieldMax caps the name/pane fields on every lifecycle request, matching
// the Python pydantic Field(max_length=64). A field longer than this is a
// malformed/hostile payload; reject with 422 rather than process it.
const nameFieldMax = 64

// PaneInfo is one live tmux pane, the runtime-evidence unit the session-closed
// gate reasons over. Mirrors repowire.hooks._tmux.PaneInfo (the gate only reads
// PaneID and Session).
type PaneInfo struct {
	PaneID  string
	Session string
}

// PaneLister probes live tmux panes ONCE for the session-closed evidence gate.
// Injected so the gate is testable and so a tmux probe blip can't wipe a live
// circle. The production impl in main shells out to `tmux list-panes -a`; an
// empty slice means "no evidence / inconclusive", never "everything died".
type PaneLister interface {
	ListAllPanes() []PaneInfo
}

// LifecycleTransport is the narrow transport seam the lifecycle handler needs:
// severing a peer's live socket after offlining it. *WebSocketTransport.Close
// satisfies it. Kept narrow (vs the full Transport) so the handler stays
// testable with a trivial fake.
type LifecycleTransport interface {
	Close(id proto.PeerID) error
}

// LifecycleHandler reacts to tmux lifecycle events by mutating peer state. It
// has no knowledge of WHERE events come from — only the abstract events. Mirrors
// the Python LifecycleHandler over (registry, query-tracker-via-OnOffline,
// transport). Query cancellation cascades through reg.OnOffline, which the hub
// already wires to the QueryTracker (see NewHubWithTransport).
type LifecycleHandler struct {
	reg             *peer.Registry
	transport       LifecycleTransport
	panes           PaneLister
	boundary        proto.CircleBoundary
	updatePlacement func(paneID, tmuxSession, circle string)

	// forgetSpawnedPane removes a pane id from the spawn-ownership set on pane
	// death, so a tmux server restart can't reuse the id and match an externally
	// attached peer. clearPaneRuntimeState drops transient pane-scoped hook files
	// (ws-hook pid/meta) after a pane dies or is taken over.
	//
	// forgetSpawnedPane: main wires the SpawnService ownership Forget; nil → no-op.
	// clearPaneRuntimeState: nil defaults to the real ClearPaneRuntimeState in
	// NewLifecycleHandler (so pane death drops the stale meta.json that would
	// otherwise re-prove a reused pane) — main need not wire it.
	forgetSpawnedPane     func(paneID string)
	clearPaneRuntimeState func(paneID string)
}

// WithPlacementUpdater keeps durable pane ownership in sync with tmux rename
// events. It is optional because lifecycle tests and non-spawn hubs have none.
func (h *LifecycleHandler) WithPlacementUpdater(fn func(paneID, tmuxSession, circle string)) *LifecycleHandler {
	h.updatePlacement = fn
	return h
}

// NewLifecycleHandler builds the handler. transport severs sockets; panes probes
// live tmux for the session-closed evidence gate. forgetSpawnedPane and
// clearPaneRuntimeState may be nil; nil defaults to service.ClearPaneRuntimeState.
func NewLifecycleHandler(
	reg *peer.Registry,
	transport LifecycleTransport,
	panes PaneLister,
	forgetSpawnedPane func(paneID string),
	clearRuntime func(paneID string),
	boundary proto.CircleBoundary,
) *LifecycleHandler {
	if forgetSpawnedPane == nil {
		forgetSpawnedPane = func(string) {}
	}
	if clearRuntime == nil {
		// Default to the real pane-runtime cleanup so pane death drops the stale
		// meta.json that would otherwise re-prove a reused pane. main need not wire it.
		clearRuntime = service.ClearPaneRuntimeState
	}
	if boundary == "" {
		boundary = proto.CircleBoundarySession
	}
	return &LifecycleHandler{
		reg:                   reg,
		transport:             transport,
		panes:                 panes,
		boundary:              boundary,
		forgetSpawnedPane:     forgetSpawnedPane,
		clearPaneRuntimeState: clearRuntime,
	}
}

// HandlePaneDied marks the peer in this pane OFFLINE (non-terminal — pane death
// is recoverable, a pane can be re-attached), disconnects its transport, and
// clears the pane's runtime state. It also forgets the pane from the spawn-
// ownership set so a reused id after a tmux server restart can't match an
// externally attached peer. No-peer is fine — still clears state and returns ok.
func (h *LifecycleHandler) HandlePaneDied(ctx context.Context, paneID string) {
	h.forgetSpawnedPane(paneID)

	p, ok := h.reg.GetPeerByPane(paneID)
	if !ok {
		h.clearPaneRuntimeState(paneID)
		return
	}

	// NON-terminal: pane death does not retire the identity (an orphan ws-hook
	// reconnecting is legitimate here). MarkOffline(terminal=false) drives the
	// transport-disconnect FSM event; OnOffline cascades query cancellation.
	if _, err := h.reg.MarkOffline(ctx, p.PeerID, false); err != nil {
		log.Printf("pane_died: mark_offline %s failed: %v", p.PeerID, err)
	}
	if h.transport != nil {
		_ = h.transport.Close(p.PeerID)
	}
	h.clearPaneRuntimeState(paneID)
	log.Printf("pane_died: %s (%s) marked offline", p.DisplayName, paneID)
}

// HandleSessionClosed offlines peers whose pane is genuinely gone after a session
// close — EVIDENCE-GATED (commit b9e5a66), not event-trusting. See the file
// header for why blindly trusting the event nukes a live circle.
func (h *LifecycleHandler) HandleSessionClosed(ctx context.Context, sessionName string) {
	var peers []*proto.Peer
	for _, p := range h.reg.GetAllPeers() {
		if p.TmuxSession == nil {
			if p.Circle == sessionName { // legacy registration without a tmux locator
				peers = append(peers, p)
			}
			continue
		}
		session, _, _ := strings.Cut(*p.TmuxSession, ":")
		if session == sessionName {
			peers = append(peers, p)
		}
	}
	if len(peers) == 0 {
		return
	}

	// Probe live tmux panes ONCE.
	var livePanes []PaneInfo
	if h.panes != nil {
		livePanes = h.panes.ListAllPanes()
	}
	if len(livePanes) == 0 {
		// Inconclusive (tmux unreachable / empty listing). Refuse to treat "no
		// evidence" as "all gone" — that's how a probe blip wipes a live circle.
		// pane-died / lazy repair catch real deaths.
		log.Printf("session_closed: tmux pane probe returned nothing for %s; "+
			"skipping mass-offline (no runtime evidence of closure)", sessionName)
		return
	}

	livePaneIDs := make(map[string]struct{}, len(livePanes))
	sessionStillLive := false
	for _, pane := range livePanes {
		livePaneIDs[pane.PaneID] = struct{}{}
		if pane.Session == sessionName {
			sessionStillLive = true
		}
	}
	if sessionStillLive {
		// Spurious close — likely a transient session exit resolving
		// #{session_name} to the surviving session.
		log.Printf("session_closed ignored: session %s still has live panes "+
			"(spurious close)", sessionName)
		return
	}

	// Session is genuinely gone from tmux. Offline only peers whose pane is
	// actually absent; spare any whose pane is somehow still live (pane-died owns
	// those).
	var doomed []*proto.Peer
	for _, p := range peers {
		if p.PaneID == nil || *p.PaneID == "" {
			doomed = append(doomed, p)
			continue
		}
		if _, live := livePaneIDs[*p.PaneID]; !live {
			doomed = append(doomed, p)
		}
	}
	if len(doomed) == 0 {
		log.Printf("session_closed: session %s gone but all %d peers still hold "+
			"live panes; nothing offlined", sessionName, len(peers))
		return
	}

	for _, p := range doomed {
		// NON-terminal: a closed tmux session can be re-created; the identity is
		// not retired here.
		if _, err := h.reg.MarkOffline(ctx, p.PeerID, false); err != nil {
			log.Printf("session_closed: mark_offline %s failed: %v", p.PeerID, err)
			continue
		}
		if h.transport != nil {
			_ = h.transport.Close(p.PeerID)
		}
		if p.PaneID != nil && *p.PaneID != "" {
			h.clearPaneRuntimeState(*p.PaneID)
		}
	}
	log.Printf("session_closed: marked %d peers offline in circle %s (%d spared)",
		len(doomed), sessionName, len(peers)-len(doomed))
}

// HandleSessionRenamed refreshes peer tmux locators; session-boundary circles
// follow the renamed session while window-boundary circles remain stable.
func (h *LifecycleHandler) HandleSessionRenamed(ctx context.Context, newName string, paneIDs []string) int {
	count := 0
	for _, paneID := range paneIDs {
		p, ok := h.reg.GetPeerByPane(paneID)
		if !ok {
			continue
		}
		window := ""
		if p.TmuxSession != nil {
			_, window, _ = strings.Cut(*p.TmuxSession, ":")
		}
		tmuxSession := newName
		if window != "" {
			tmuxSession += ":" + window
		}
		h.reg.UpdateTmuxSession(ctx, p.PeerID, tmuxSession)
		if h.boundary == proto.CircleBoundarySession && p.Circle != newName {
			h.reg.SetCircle(ctx, p.PeerID, newName)
		}
		circle := p.Circle
		if h.boundary == proto.CircleBoundarySession {
			circle = newName
		}
		if h.updatePlacement != nil {
			h.updatePlacement(paneID, tmuxSession, circle)
		}
		count++
	}
	if count > 0 {
		log.Printf("session_renamed: updated %d peers → %s", count, newName)
	}
	return count
}

// HandleWindowRenamed refreshes tmux locators without changing circle or display
// name; window-boundary circles use the stable window id, not its mutable name.
func (h *LifecycleHandler) HandleWindowRenamed(ctx context.Context, sessionName, newName string, paneIDs []string) {
	for _, paneID := range paneIDs {
		if p, ok := h.reg.GetPeerByPane(paneID); ok {
			tmuxSession := sessionName + ":" + newName
			h.reg.UpdateTmuxSession(ctx, p.PeerID, tmuxSession)
			if h.updatePlacement != nil {
				h.updatePlacement(paneID, tmuxSession, p.Circle)
			}
		}
	}
}

// HandleClientDetached logs a client detach. No state change.
func (h *LifecycleHandler) HandleClientDetached(sessionName string) {
	log.Printf("client_detached: session %s", sessionName)
}

// ---- HTTP wire models (must match the Python pydantic request shapes) ----

type paneDiedRequest struct {
	PaneID string `json:"pane_id"`
}

type sessionClosedRequest struct {
	SessionName string `json:"session_name"`
}

type sessionRenamedRequest struct {
	NewName string   `json:"new_name"`
	PaneIDs []string `json:"pane_ids"`
}

type windowRenamedRequest struct {
	SessionName string   `json:"session_name"`
	NewName     string   `json:"new_name"`
	PaneIDs     []string `json:"pane_ids"`
}

type clientDetachedRequest struct {
	SessionName string `json:"session_name"`
}

// LifecycleRoutes registers the tmux-lifecycle hook endpoints on the mux. All are
// localhost-only.
func (h *Hub) LifecycleRoutes(mux *http.ServeMux, lh *LifecycleHandler) {
	mux.HandleFunc("POST /hooks/lifecycle/pane-died", localhostOnly(lh.servePaneDied))
	mux.HandleFunc("POST /hooks/lifecycle/session-closed", localhostOnly(lh.serveSessionClosed))
	mux.HandleFunc("POST /hooks/lifecycle/session-renamed", localhostOnly(lh.serveSessionRenamed))
	mux.HandleFunc("POST /hooks/lifecycle/window-renamed", localhostOnly(lh.serveWindowRenamed))
	mux.HandleFunc("POST /hooks/lifecycle/client-detached", localhostOnly(lh.serveClientDetached))
}

func (lh *LifecycleHandler) servePaneDied(w http.ResponseWriter, r *http.Request) {
	var req paneDiedRequest
	if !decode(w, r, &req) || !requireName(w, req.PaneID) {
		return
	}
	lh.HandlePaneDied(r.Context(), req.PaneID)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (lh *LifecycleHandler) serveSessionClosed(w http.ResponseWriter, r *http.Request) {
	var req sessionClosedRequest
	if !decode(w, r, &req) || !requireName(w, req.SessionName) {
		return
	}
	lh.HandleSessionClosed(r.Context(), req.SessionName)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (lh *LifecycleHandler) serveSessionRenamed(w http.ResponseWriter, r *http.Request) {
	var req sessionRenamedRequest
	if !decode(w, r, &req) {
		return
	}
	if !isValidIdentifier(req.NewName) || !validNames(req.PaneIDs) {
		writeError(w, http.StatusUnprocessableEntity, "invalid field")
		return
	}
	lh.HandleSessionRenamed(r.Context(), req.NewName, req.PaneIDs)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (lh *LifecycleHandler) serveWindowRenamed(w http.ResponseWriter, r *http.Request) {
	var req windowRenamedRequest
	if !decode(w, r, &req) {
		return
	}
	if !validName(req.SessionName) || !validName(req.NewName) || !validNames(req.PaneIDs) {
		writeError(w, http.StatusUnprocessableEntity, "invalid field")
		return
	}
	lh.HandleWindowRenamed(r.Context(), req.SessionName, req.NewName, req.PaneIDs)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

func (lh *LifecycleHandler) serveClientDetached(w http.ResponseWriter, r *http.Request) {
	var req clientDetachedRequest
	if !decode(w, r, &req) || !requireName(w, req.SessionName) {
		return
	}
	lh.HandleClientDetached(req.SessionName)
	writeJSON(w, http.StatusOK, okResponse{OK: true})
}

// ---- HTTP helpers ----

// localhostOnly wraps a handler so only requests originating from the loopback
// interface reach it (mirrors Python require_localhost). 403 otherwise.
func localhostOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalhost(r) {
			writeError(w, http.StatusForbidden, "Restricted to localhost")
			return
		}
		next(w, r)
	}
}

func isLocalhost(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	switch host {
	case "127.0.0.1", "::1", "localhost", "":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

// requireName validates the single primary name field (read AFTER decode has
// populated it) against the length cap, writing a 422 when it is absent or
// over-length. Must be called after decode — passing the field before decode
// runs would always see the zero value.
func requireName(w http.ResponseWriter, name string) bool {
	if !validName(name) {
		writeError(w, http.StatusUnprocessableEntity, "invalid field")
		return false
	}
	return true
}

// validName enforces Field(min_length=1, max_length=64).
func validName(s string) bool {
	return len(s) >= 1 && len(s) <= nameFieldMax
}

func validNames(ss []string) bool {
	for _, s := range ss {
		if len(s) > nameFieldMax {
			return false
		}
	}
	return true
}
