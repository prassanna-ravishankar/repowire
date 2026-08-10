package hub

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// ============================================================================
// POST /mcp — MCP-over-HTTP (streamable HTTP transport, JSON responses),
// exposing the complete mesh tool surface over the same services as REST.
//
// Built on the official github.com/modelcontextprotocol/go-sdk (the FastMCP
// equivalent for Go — the Python daemon serves its own HTTP MCP via FastMCP,
// this is the parity path rather than a hand-rolled JSON-RPC decoder). The
// handler runs Stateless+JSONResponse: every POST is a self-contained
// request/response (no session-id handshake, no SSE stream) — the simplest
// shape that satisfies the streamable-HTTP spec for a single-shot tool caller.
//
// Gated by config.MCPHTTPConfig (see WithMCP); nil h.mcp or Enabled=false
// means the route is never registered.
//
// Identity: the caller's peer arrives via the X-Repowire-Peer header stamped
// by the thin stdio shim. Absent that header, callers act as the daemon-
// owned "mcp-http" identity — mirrors repowire/mcp/server.py's
// _http_mcp_identity (name="mcp-http", circle="global", role="human"), the
// existing Python HTTP-MCP default. Tool handlers read the header straight off
// req.Extra.Header (populated by the SDK's streamable transport per-request;
// no context plumbing needed).
//
// ============================================================================

// mcpDefaultIdentity mirrors Python's _http_mcp_identity default name: the
// caller identity assumed when no X-Repowire-Peer header is present.
const mcpDefaultIdentity = "mcp-http"

// mcpDeps holds the /mcp route's config and the built SDK HTTP handler.
// Wired via WithMCP; nil -> registerMCPRoutes is a no-op.
type mcpDeps struct {
	cfg     config.MCPHTTPConfig
	handler http.Handler
}

// WithMCP builds the MCP server (tools bound to this hub + delivery) and wraps
// it in the SDK's streamable-HTTP handler. delivery may be nil in tests that
// only exercise whoami/list_peers; notify_peer/broadcast then return a tool
// error rather than panicking. Returns the hub for chaining; call before
// Routes.
func (h *Hub) WithMCP(cfg config.MCPHTTPConfig, delivery *service.PeerDelivery) *Hub {
	srv := mcp.NewServer(&mcp.Implementation{Name: "repowire", Version: "0.1"}, nil)
	registerMCPTools(srv, h, delivery, cfg)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})

	h.mcp = &mcpDeps{cfg: cfg, handler: handler}
	return h
}

// registerMCPRoutes wires POST /mcp when the endpoint is configured+enabled.
// No-op otherwise (mirrors the other optional With*/register* pairs).
func (h *Hub) registerMCPRoutes(mux *http.ServeMux) {
	if h.mcp == nil || !h.mcp.cfg.Enabled {
		return
	}
	mux.Handle("/mcp", http.HandlerFunc(h.handleMCP))
}

// handleMCP enforces the HTTP-level auth policy, then hands off to the SDK's
// streamable-HTTP handler for JSON-RPC decoding/dispatch.
func (h *Hub) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !h.mcpAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	h.validateMCPIdentity(r)
	h.mcp.handler.ServeHTTP(w, r)
}

func (h *Hub) validateMCPIdentity(r *http.Request) {
	claimed := r.Header.Get("X-Repowire-Peer")
	if claimed == "" {
		return
	}
	proof := r.Header.Get("X-Repowire-Identity-Proof")
	peer, _ := h.reg.GetPeerByName(claimed, nil)
	if h.store == nil || peer == nil || !h.store.ValidateMCPIdentityProof(r.Context(), proof, string(peer.PeerID)) {
		r.Header.Del("X-Repowire-Peer")
		r.Header.Del("X-Repowire-Identity-Proof")
		return
	}
	// Canonicalize display-name claims before the tool layer sees them.
	peerID := string(peer.PeerID)
	r.Header.Set("X-Repowire-Peer", peerID)
	_, _ = h.reg.TouchLastSeen(r.Context(), peerID, nil)
}

// mcpAuthorized enforces the /mcp auth policy. RequireAuth demands a bearer
// token matching h.authToken (fail closed if authToken is unset — a
// RequireAuth=true daemon with no configured token is a misconfiguration, not
// an open door). Otherwise the request is allowed only when
// AllowUnauthenticatedLocalhost is set AND the caller is loopback (isLocalhost,
// shared with the lifecycle-hook routes).
func (h *Hub) mcpAuthorized(r *http.Request) bool {
	cfg := h.mcp.cfg
	if cfg.Bind == "localhost-only" && !isLocalhost(r) {
		return false
	}
	if cfg.RequireAuth {
		if h.authToken == "" {
			return false
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if !strings.HasPrefix(got, prefix) {
			return false
		}
		token := strings.TrimPrefix(got, prefix)
		return subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) == 1
	}
	return cfg.AllowUnauthenticatedLocalhost && isLocalhost(r)
}

// callerIdentity reads X-Repowire-Peer off the per-request headers the SDK
// attaches to CallToolRequest.Extra, falling back to the mcp-http daemon
// identity when absent (Python parity — see the package doc above).
func callerIdentity(req *mcp.CallToolRequest) string {
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		if v := req.Extra.Header.Get("X-Repowire-Peer"); v != "" {
			return v
		}
	}
	return mcpDefaultIdentity
}

// textResult wraps a single text block as a successful CallToolResult.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// ---- tool argument shapes ---------------------------------------------------

type mcpListPeersArgs struct {
	ShowOffline bool   `json:"show_offline,omitempty" jsonschema:"Include offline peers"`
	IncludeSelf bool   `json:"include_self,omitempty" jsonschema:"Include the calling peer"`
	Circle      string `json:"circle,omitempty" jsonschema:"Circle name or * for mesh-wide"`
}

type mcpNotifyPeerArgs struct {
	PeerName    string           `json:"peer_name" jsonschema:"Target peer display_name or peer_id"`
	Message     string           `json:"message" jsonschema:"Message text to deliver"`
	Circle      string           `json:"circle,omitempty" jsonschema:"Optional circle scope for resolving peer_name"`
	Attachments []map[string]any `json:"attachments,omitempty"`
}

type mcpBroadcastArgs struct {
	Message string `json:"message" jsonschema:"Message text to broadcast"`
}

// registerMCPTools installs the complete MCP tool set onto srv. Read and
// messaging tools are always present; lifecycle/admin tools enforce the
// configured dangerous-tool gate in their handlers.
func registerMCPTools(srv *mcp.Server, h *Hub, delivery *service.PeerDelivery, cfg config.MCPHTTPConfig) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "whoami",
		Description: "Return the caller's own peer identity (peer_id, display_name, circle, backend, status).",
	}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return h.mcpWhoami(callerIdentity(req)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_peers",
		Description: "List peers in the mesh, optionally filtered by status, circle, or backend.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args mcpListPeersArgs) (*mcp.CallToolResult, any, error) {
		return h.mcpListPeers(ctx, args, callerIdentity(req)), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "notify_peer",
		Description: "Send a necessary fire-and-forget update to one peer. Do not notify peers reflexively; they may be occupied.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args mcpNotifyPeerArgs) (*mcp.CallToolResult, any, error) {
		res, err := h.mcpNotifyPeer(ctx, delivery, args, callerIdentity(req))
		return res, nil, err
	})

	registerMCPParityTools(srv, h, cfg)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "broadcast",
		Description: "Broadcast only an announcement that materially affects every eligible peer in the caller's circle.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args mcpBroadcastArgs) (*mcp.CallToolResult, any, error) {
		res, err := mcpBroadcast(ctx, delivery, args, callerIdentity(req))
		return res, nil, err
	})
}

// ---- tool implementations ---------------------------------------------------

// mcpWhoami resolves the caller's identity via the registry (the same lookup
// GET /peers/{identifier} uses). An unresolved identity (including the default
// mcp-http synthetic caller, which has no registry entry) is not an error —
// it reports the bare identity instead, mirroring Python's HTTP-MCP default
// caller shape (name/circle/role, no peer_id) rather than failing the call.
func (h *Hub) mcpWhoami(fromPeer string) *mcp.CallToolResult {
	if p, err := h.reg.GetPeerByName(fromPeer, nil); err == nil && p != nil {
		return textResult(mcpPeerTSV([]*proto.Peer{p}))
	}
	return textResult(fmt.Sprintf("display_name=%s circle=global backend=mcp-http status=unregistered", fromPeer))
}

// mcpListPeers mirrors h.listPeers' status/circle/backend filters (see
// routes_peer_read.go), rendered as a compact one-line-per-peer text block
// instead of the JSON PeersResponse.
func (h *Hub) mcpListPeers(ctx context.Context, args mcpListPeersArgs, caller string) *mcp.CallToolResult {
	h.reg.LazyRepair(ctx)
	peers := h.reg.GetAllPeers()
	me, _ := h.reg.GetPeerByName(caller, nil)
	if !args.ShowOffline {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return p.Status == proto.StatusOnline || p.Status == proto.StatusBusy
		})
	}
	effectiveCircle := args.Circle
	if effectiveCircle == "" && me != nil && me.Role != proto.RoleOrchestrator {
		effectiveCircle = me.Circle
	}
	if effectiveCircle != "" && effectiveCircle != "*" {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return p.Circle == effectiveCircle || p.Role.BypassesCircles()
		})
	}
	if !args.IncludeSelf && me != nil {
		peers = filterPeers(peers, func(p *proto.Peer) bool {
			return p.PeerID != me.PeerID
		})
	}
	return textResult(mcpPeerTSV(peers))
}

func mcpPeerTSV(peers []*proto.Peer) string {
	var b strings.Builder
	b.WriteString("peer_id\tname\tproject\tcircle\trole\tstatus\tpath\tmachine\tdescription\tbackend\tlast_seen\tturn_state\tmodel")
	for _, p := range peers {
		project, _ := p.Metadata["project"].(string)
		lastSeen, model := "", ""
		if p.LastSeen != nil {
			lastSeen = p.LastSeen.Format(time.RFC3339Nano)
		}
		if p.Model != nil {
			model = *p.Model
		}
		fmt.Fprintf(&b, "\n%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			p.PeerID, p.DisplayName, project, p.Circle, p.Role, p.Status, p.Path, p.Machine, p.Description, p.Backend, lastSeen, p.TurnState, model)
	}
	return b.String()
}

// mcpNotifyPeer dispatches to service.PeerDelivery.Notify. A non-nil error
// return is auto-packed into an isError CallToolResult by the AddTool wrapper
// (see toolForErr in the SDK) — no manual CallToolResult.SetError needed here.
func (h *Hub) mcpNotifyPeer(ctx context.Context, delivery *service.PeerDelivery, args mcpNotifyPeerArgs, fromPeer string) (*mcp.CallToolResult, error) {
	if args.PeerName == "" || args.Message == "" {
		return nil, fmt.Errorf("peer_name and message are required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("notify_peer is not wired (no delivery service configured)")
	}
	circle := h.mcpSendCircle(fromPeer, args.Circle)
	deliveryID := "notif-" + uuid.NewString()[:8]
	_, err := delivery.Notify(ctx, service.NotifyParams{
		FromPeer:    fromPeer,
		ToPeer:      args.PeerName,
		Text:        args.Message,
		Circle:      circle,
		Attachments: args.Attachments,
		DeliveryID:  deliveryID,
	})
	if err != nil {
		return nil, fmt.Errorf("notify failed: %w", err)
	}
	return textResult(deliveryID), nil
}

func (h *Hub) mcpSendCircle(caller, requested string) *string {
	if requested != "" {
		return &requested
	}
	if peer, _ := h.reg.GetPeerByName(caller, nil); peer != nil && peer.Circle != "" {
		return &peer.Circle
	}
	return nil
}

// mcpBroadcast dispatches to service.PeerDelivery.Broadcast.
func mcpBroadcast(ctx context.Context, delivery *service.PeerDelivery, args mcpBroadcastArgs, fromPeer string) (*mcp.CallToolResult, error) {
	if args.Message == "" {
		return nil, fmt.Errorf("message is required")
	}
	if delivery == nil {
		return nil, fmt.Errorf("broadcast is not wired (no delivery service configured)")
	}
	sent, failed := delivery.Broadcast(ctx, fromPeer, args.Message, nil, false)
	return textResult(fmt.Sprintf("sent=%d failed=%d", len(sent), len(failed))), nil
}
