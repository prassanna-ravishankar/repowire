package hub

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// Wire view surfaces (rfcs/wire-observability.md): wire_query and wire_show
// for a proven orchestrator over MCP, and GET /wire/threads[/{id}] for the
// operator. All read service.Wire; none of them carries message text. The
// actions (rfcs/wire-actions.md) are POST routes for the operator and the
// reassign, nudge and cancel_ask tools for an orchestrator; both reach
// wireAction (ask_actions.go), which keeps the rules.

// WithWire attaches the wire view.
func (h *Hub) WithWire(w *service.Wire) *Hub {
	h.wire = w
	return h
}

func (h *Hub) registerWireRoutes(mux *http.ServeMux) {
	if h.wire == nil {
		return
	}
	mux.HandleFunc("GET /wire/threads", h.requireAuth(h.handleWireThreads))
	mux.HandleFunc("GET /wire/threads/{thread_id}", h.requireAuth(h.handleWireThread))
	// Actions (rfcs/wire-actions.md) mutate other peers' asks: operator proof
	// only, never requireAuth's tokenless or local-dashboard pass.
	mux.HandleFunc("POST /wire/threads/{thread_id}/reassign", h.requireOperator(h.handleWireAction("reassign")))
	mux.HandleFunc("POST /wire/threads/{thread_id}/nudge", h.requireOperator(h.handleWireAction("nudge")))
	mux.HandleFunc("POST /wire/threads/{thread_id}/cancel", h.requireOperator(h.handleWireAction("cancel")))
	// A peer sets its own visibility over MCP; the operator may set any
	// peer's. An orchestrator cannot set another peer's.
	mux.HandleFunc("POST /peers/{name}/wire-visibility", h.requireOperator(h.handleWireVisibility))
}

func (h *Hub) handleWireVisibility(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := h.setWireVisibility(r.Context(), r.PathValue("name"), req.Mode)
	if err != nil {
		writeRouteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer": string(p.DisplayName), "peer_id": string(p.PeerID), "wire_visibility": req.Mode})
}

// setWireVisibility records a peer's wire visibility.
func (h *Hub) setWireVisibility(ctx context.Context, identifier, mode string) (*proto.Peer, error) {
	if h.wire == nil || h.ask == nil {
		return nil, routeErr(http.StatusServiceUnavailable, "the wire view is not available on this daemon")
	}
	p, err := h.ask.reg.GetPeerByName(identifier, nil)
	if err != nil {
		return nil, routeErr(http.StatusConflict, err.Error())
	}
	if p == nil {
		return nil, routeErr(http.StatusNotFound, "Unknown peer: "+identifier)
	}
	if err := h.wire.SetVisibility(ctx, p.PeerID, mode); err != nil {
		return nil, routeErr(http.StatusUnprocessableEntity, err.Error())
	}
	return p, nil
}

// requireOperator admits only a request carrying the daemon token. Unlike
// requireAuth it has no tokenless or same-origin pass, which any local
// process can satisfy; a daemon without a token refuses. On a machine where
// agents run as the same user, the token file is the limit of this proof
// (#581).
func (h *Hub) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authToken == "" {
			writeError(w, http.StatusForbidden, "Operator actions need daemon.auth_token set")
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(h.authToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "Operator actions need the daemon token")
			return
		}
		next(w, r)
	}
}

// WireActionRequest is the body of the wire action routes.
type WireActionRequest struct {
	To     string `json:"to,omitempty"`     // reassign
	Reason string `json:"reason,omitempty"` // reassign, cancel
	Note   string `json:"note,omitempty"`   // nudge
}

func (h *Hub) handleWireAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req WireActionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		result, err := h.wireAction(r.Context(), operator, action, r.PathValue("thread_id"), req)
		if err != nil {
			writeRouteError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func (h *Hub) handleWireThreads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := wireFilter(q.Get("circle"), q.Get("peer"), q.Get("kind"), q.Get("state"), q.Get("stuck_only") == "true", q.Get("since"), q.Get("limit"), q.Get("cursor"))
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	h.reg.LazyRepair(r.Context())
	page, err := h.wire.Threads(r.Context(), f)
	if err != nil {
		writeJSONError(w, wireStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Hub) handleWireThread(w http.ResponseWriter, r *http.Request) {
	h.reg.LazyRepair(r.Context())
	t, ok, err := h.wire.Thread(r.Context(), r.PathValue("thread_id"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "No thread: "+r.PathValue("thread_id"))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func wireStatus(err error) int {
	if errors.Is(err, service.ErrWireCursor) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// wireFilter builds a filter from string inputs, shared by HTTP and MCP.
// since is a Go duration or a number of days ("7d").
func wireFilter(circle, peer, kind, state string, stuckOnly bool, since, limit, cursor string) (service.WireFilter, error) {
	f := service.WireFilter{Circle: circle, Peer: peer, Kind: kind, State: state, StuckOnly: stuckOnly, Cursor: cursor}
	if kind != "" && kind != "ask" && kind != "notify" {
		return f, fmt.Errorf("kind must be ask or notify")
	}
	if since != "" {
		d, err := parseWireSince(since)
		if err != nil {
			return f, err
		}
		f.Since = d
	}
	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n <= 0 {
			return f, fmt.Errorf("limit must be a positive number")
		}
		f.Limit = n
	}
	return f, nil
}

func parseWireSince(since string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(since, "d"); ok {
		if n, err := strconv.ParseFloat(days, 64); err == nil && n > 0 {
			return time.Duration(n * float64(24*time.Hour)), nil
		}
	} else if d, err := time.ParseDuration(since); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("since must be a duration such as 90m, 24h or 7d")
}

// ---- MCP --------------------------------------------------------------------

type mcpWireQueryArgs struct {
	Circle    string `json:"circle,omitempty" jsonschema:"only threads with a party in this circle"`
	Peer      string `json:"peer,omitempty" jsonschema:"only threads with this party (display name or peer_id)"`
	Kind      string `json:"kind,omitempty" jsonschema:"ask or notify"`
	State     string `json:"state,omitempty" jsonschema:"only this state, for example delivered or reply_stashed"`
	StuckOnly bool   `json:"stuck_only,omitempty" jsonschema:"only threads with a stuck signal"`
	Since     string `json:"since,omitempty" jsonschema:"activity window, for example 90m, 24h (default) or 7d"`
	Limit     int    `json:"limit,omitempty" jsonschema:"at most this many threads (default 50, max 100)"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"resume after the previous page"`
}

type mcpWireVisibilityArgs struct {
	Mode string `json:"mode" jsonschema:"visible (default) or private. An orchestrator sees only the envelope of a private peer's threads (who, state, age), never their bodies, and cannot act on them"`
}

type mcpWireReassignArgs struct {
	ThreadID string `json:"thread_id" jsonschema:"a thread id from wire_query"`
	To       string `json:"to" jsonschema:"the new holder, a peer in your circle (display name or peer_id)"`
	Reason   string `json:"reason" jsonschema:"why, at most 200 characters; the asker, the former holder and the new holder see it"`
}

type mcpWireNudgeArgs struct {
	ThreadID string `json:"thread_id" jsonschema:"a thread id from wire_query"`
	Note     string `json:"note,omitempty" jsonschema:"optional note for the holder, at most 200 characters"`
}

type mcpWireCancelArgs struct {
	ThreadID string `json:"thread_id" jsonschema:"a thread id from wire_query"`
	Reason   string `json:"reason" jsonschema:"why, at most 200 characters; the asker gets it as the ask's outcome"`
}

type mcpWireShowArgs struct {
	ThreadID string `json:"thread_id" jsonschema:"a thread id from wire_query"`
	Cursor   int    `json:"cursor,omitempty" jsonschema:"resume the timeline after this stage seq"`
}

// registerWireTools always registers the tools, so tool registration does
// not depend on wiring order; without a wire view they answer an error.
func registerWireTools(srv *mcp.Server, h *Hub) {
	addMCPTool(srv, "wire_query", "Orchestrator only: list ask and notify threads across every circle (who asked whom, who holds it now, its state, and stuck signals). Metadata only, never message text.", func(ctx context.Context, caller string, a mcpWireQueryArgs) (string, error) {
		if _, err := h.requireWireCaller(caller, "wire_query"); err != nil {
			return "", err
		}
		limit := ""
		if a.Limit > 0 {
			limit = strconv.Itoa(a.Limit)
		}
		f, err := wireFilter(a.Circle, a.Peer, a.Kind, a.State, a.StuckOnly, a.Since, limit, a.Cursor)
		if err != nil {
			return "", err
		}
		h.reg.LazyRepair(ctx)
		page, err := h.wire.Threads(ctx, f)
		if err != nil {
			return "", err
		}
		return page.Text(time.Now()), nil
	})
	addMCPTool(srv, "wire_show", "Orchestrator only: one thread's envelope and stage timeline. Metadata only, never message text.", func(ctx context.Context, caller string, a mcpWireShowArgs) (string, error) {
		orch, err := h.requireWireCaller(caller, "wire_show")
		if err != nil {
			return "", err
		}
		h.reg.LazyRepair(ctx)
		t, ok, err := h.wire.Thread(ctx, a.ThreadID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no thread %s", service.WireCell(a.ThreadID))
		}
		text := t.Text(a.Cursor)
		if h.wire.OrchestratorMay(ctx, t, orch.Circle, nil) == nil {
			text += t.ActionsText(orch.Circle)
		}
		return text, nil
	})
	registerWireActionTool(srv, h, "reassign", "reassign", "Orchestrator only: move an open ask to another peer in your circle. Refused on a protected thread or one with a party outside your circle. The new holder gets the ask; the asker and the former holder are told.", func(a mcpWireReassignArgs) (string, WireActionRequest) {
		return a.ThreadID, WireActionRequest{To: a.To, Reason: a.Reason}
	})
	registerWireActionTool(srv, h, "nudge", "nudge", "Orchestrator only: remind an open ask's holder, at most once per 10 minutes per thread. Refused on a protected thread or one with a party outside your circle.", func(a mcpWireNudgeArgs) (string, WireActionRequest) {
		return a.ThreadID, WireActionRequest{Note: a.Note}
	})
	registerWireActionTool(srv, h, "cancel_ask", "cancel", "Orchestrator only: close an open ask. The asker gets the cancellation as its outcome and the holder is told to stop. Refused on a protected thread or one with a party outside your circle.", func(a mcpWireCancelArgs) (string, WireActionRequest) {
		return a.ThreadID, WireActionRequest{Reason: a.Reason}
	})
	addMCPTool(srv, "set_wire_visibility", "Choose whether orchestrators may act on your ask threads: visible (default) or private. Private threads still show their envelope in the wire view.", func(ctx context.Context, caller string, a mcpWireVisibilityArgs) (string, error) {
		self, err := h.mcpCallerPeer(caller, "set_wire_visibility")
		if err != nil {
			return "", err
		}
		if _, err := h.setWireVisibility(ctx, string(self.PeerID), a.Mode); err != nil {
			return "", err
		}
		return "wire visibility: " + a.Mode, nil
	})
}

// registerWireActionTool adapts one wire action to an MCP tool: the caller
// must be a proven orchestrator, and wireAction applies the thread rules.
func registerWireActionTool[T any](srv *mcp.Server, h *Hub, tool, action, description string, req func(T) (string, WireActionRequest)) {
	addMCPTool(srv, tool, description, func(ctx context.Context, caller string, a T) (string, error) {
		orch, err := h.requireWireCaller(caller, tool)
		if err != nil {
			return "", err
		}
		threadID, body := req(a)
		result, err := h.wireAction(ctx, wireActor{peer: orch}, action, threadID, body)
		if err != nil {
			return "", err
		}
		return jsonResult(result), nil
	})
}

// requireWireCaller admits a proven peer holding the orchestrator role, on
// every call, so a demoted orchestrator loses the view at once. Anonymous
// HTTP MCP is refused: the view reads other peers' threads.
func (h *Hub) requireWireCaller(caller, tool string) (*proto.Peer, error) {
	if h.wire == nil {
		return nil, fmt.Errorf("%s: the wire view is not available on this daemon", tool)
	}
	p, err := h.mcpCallerPeer(caller, tool)
	if err != nil {
		return nil, err
	}
	if p.Role != proto.RoleOrchestrator {
		return nil, fmt.Errorf("%s requires the orchestrator role", tool)
	}
	return p, nil
}
