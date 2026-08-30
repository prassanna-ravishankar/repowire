package hub

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

var paneIDPattern = regexp.MustCompile(`^%\d+$`)

type localPane struct {
	PaneID          string `json:"pane_id"`
	PID             int    `json:"pid"`
	Command         string `json:"command"`
	CWD             string `json:"cwd"`
	Session         string `json:"session"`
	Window          string `json:"window"`
	WindowID        string `json:"window_id"`
	DetectedBackend string `json:"detected_backend"`
	Confidence      string `json:"confidence"`
}

func listLocalPanes() []localPane {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_pid}\t#{pane_current_command}\t#{pane_current_path}\t#{session_name}\t#{window_name}\t#{window_id}").CombinedOutput()
	if err != nil {
		log.Printf("tmux list-panes failed: %v: %s", err, strings.TrimSpace(string(out)))
		return nil
	}
	var panes []localPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 7 {
			continue
		}
		pid, _ := strconv.Atoi(parts[1])
		backend := detectPaneBackend(parts[2])
		confidence := "unknown"
		if backend != "unknown" {
			confidence = "hint"
		}
		panes = append(panes, localPane{PaneID: parts[0], PID: pid, Command: parts[2], CWD: parts[3], Session: parts[4], Window: parts[5], WindowID: parts[6], DetectedBackend: backend, Confidence: confidence})
	}
	if len(panes) == 0 && len(out) > 0 {
		log.Printf("tmux list-panes returned unparseable output: %q", strings.TrimSpace(string(out)))
	}
	return panes
}

// TmuxPaneLister projects the daemon's tmux snapshot for lifecycle evidence.
// Keeping the shell call and parser here means orphan discovery and session
// closure cannot disagree about which panes exist.
type TmuxPaneLister struct{}

func (TmuxPaneLister) ListAllPanes() []PaneInfo {
	panes := listLocalPanes()
	out := make([]PaneInfo, 0, len(panes))
	for _, pane := range panes {
		out = append(out, PaneInfo{PaneID: pane.PaneID, Session: pane.Session})
	}
	return out
}

func detectPaneBackend(command string) string {
	name := strings.ToLower(filepath.Base(command))
	for _, candidate := range []struct{ needle, backend string }{{"claude", "claude-code"}, {"codex", "codex"}, {"opencode", "opencode"}, {"pi", "pi"}} {
		if name == candidate.needle || strings.Contains(name, candidate.needle) {
			return candidate.backend
		}
	}
	return "unknown"
}

func localPaneIdentity(boundary proto.CircleBoundary, pane *localPane) (circle, tmuxSession string) {
	if pane == nil {
		return "", ""
	}
	circle = proto.TmuxCircle(boundary, pane.Session, pane.WindowID)
	if pane.Session != "" && pane.Window != "" {
		tmuxSession = pane.Session + ":" + pane.Window
	}
	return circle, tmuxSession
}

func (h *Hub) registerPaneRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /panes/orphans", h.requireAuth(h.handleOrphanPanes))
	mux.HandleFunc("POST /panes/{pane_id}/link", h.requireAuth(h.handleLinkPane))
}

func (h *Hub) handleOrphanPanes(w http.ResponseWriter, r *http.Request) {
	registered := map[string]bool{}
	for _, item := range h.reg.GetAllPeers() {
		if item.PaneID != nil {
			registered[*item.PaneID] = true
		}
	}
	panes := []localPane{}
	for _, pane := range listLocalPanes() {
		if !registered[pane.PaneID] {
			panes = append(panes, pane)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"panes": panes})
}

type linkPaneRequest struct {
	Backend proto.AgentType `json:"backend"`
	Name    *string         `json:"name,omitempty"`
	Circle  *string         `json:"circle,omitempty"`
	CWD     *string         `json:"cwd,omitempty"`
}

func (h *Hub) handleLinkPane(w http.ResponseWriter, r *http.Request) {
	paneID := r.PathValue("pane_id")
	if !paneIDPattern.MatchString(paneID) {
		writeJSONError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Invalid tmux pane id: %q (expected like %%42)", paneID))
		return
	}
	var request linkPaneRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Backend == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "backend is required")
		return
	}
	if request.Name != nil && *request.Name != "" && !isValidIdentifier(*request.Name) {
		writeJSONError(w, http.StatusUnprocessableEntity, "name contains invalid characters")
		return
	}
	if existing, ok := h.reg.GetPeerByPane(paneID); ok {
		writeJSONError(w, http.StatusConflict, map[string]any{"error": "already_linked", "hint": fmt.Sprintf("Pane %s is already bound to peer %s.", paneID, existing.DisplayName), "peer_id": existing.PeerID})
		return
	}
	var pane *localPane
	for _, item := range listLocalPanes() {
		if item.PaneID == paneID {
			copy := item
			pane = &copy
			break
		}
	}
	cwd := ""
	if request.CWD != nil {
		cwd = *request.CWD
	} else if pane != nil {
		cwd = pane.CWD
	}
	if cwd == "" {
		writeJSONError(w, http.StatusNotFound, map[string]any{"error": "pane_not_found", "hint": fmt.Sprintf("No live tmux pane %s to resolve a working directory from.", paneID)})
		return
	}
	boundary := proto.CircleBoundarySession
	if h.spawn != nil {
		boundary = h.spawn.boundary
	}
	circle, tmuxSession := localPaneIdentity(boundary, pane)
	if request.Circle != nil && *request.Circle != "" {
		if circle != "" && *request.Circle != circle {
			writeJSONError(w, http.StatusConflict, "explicit circle contradicts live tmux boundary evidence")
			return
		}
		circle = *request.Circle
	}
	if circle == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "circle is required when the pane has no tmux session")
		return
	}
	machine, _ := os.Hostname()
	pid := 0
	if pane != nil {
		pid = pane.PID
	}
	id, displayName, err := h.reg.AllocateAndRegister(r.Context(), peer.AllocateParams{Circle: circle, Backend: request.Backend, Path: &cwd, PaneID: &paneID, TmuxSession: strPtr(tmuxSession), Machine: machine, Role: proto.RoleAgent, AgentPID: intPtrNonzero(pid)})
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if request.Name != nil && *request.Name != "" {
		updated, renameErr := h.reg.UpdateDisplayName(r.Context(), id, proto.DisplayName(*request.Name))
		if renameErr != nil || !updated {
			_, _ = h.reg.UnregisterPeer(r.Context(), string(id), nil)
			writeJSONError(w, http.StatusConflict, "requested name is already in use")
			return
		}
		displayName = proto.DisplayName(*request.Name)
	}
	spawned, spawnErr := clienthooks.ReconcileWSHook(paneID, string(id), string(displayName), string(request.Backend), cwd, pid)
	connected := false
	if spawnErr == nil && spawned {
		connected = awaitTransport(r.Context(), h.transport, id, 8*time.Second)
	}
	if connected {
		writeJSON(w, http.StatusOK, map[string]any{"linked": true, "pane_id": paneID, "peer_id": id, "display_name": displayName, "transport_connected": true, "reason": "linked"})
		return
	}
	clienthooks.ClearPaneRuntimeState(paneID)
	rolledBack, _ := h.reg.UnregisterPeer(r.Context(), string(id), nil)
	reason := "transport_unestablished"
	if spawnErr != nil || !spawned {
		reason = "ws_hook_spawn_failed"
	} else if !rolledBack {
		reason = "transport_unestablished_rollback_failed"
	}
	response := map[string]any{"linked": false, "pane_id": paneID, "transport_connected": false, "reason": reason, "repair_hint": fmt.Sprintf("The ws-hook did not connect. Confirm the pane is a live local agent and retry: repowire link --pane %s --backend %s", paneID, request.Backend)}
	if !rolledBack {
		response["peer_id"], response["display_name"] = id, displayName
	}
	writeJSON(w, http.StatusOK, response)
}

func awaitTransport(ctx context.Context, transport *service.WebSocketTransport, id proto.PeerID, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if transport.IsConnected(id) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return transport.IsConnected(id)
		case <-tick.C:
		}
	}
}

func intPtrNonzero(value int) *int {
	if value == 0 {
		return nil
	}
	return &value
}
