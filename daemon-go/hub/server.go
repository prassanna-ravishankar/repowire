// Package hub owns the WebSocket server, message router, and HTTP route
// layer. It is the only network hub: every peer speaks the same wire protocol
// to it. Routing is keyed on proto.PeerID throughout, so a DisplayName can
// never be passed where a routing target is required.
//
// hub is the route/transport layer: it decodes requests, composes the
// application services in the service package (delivery, ask lifecycle,
// spawn, session control, scheduling), and writes responses. service must
// never import hub — see service's package doc for the layering rule.
package hub

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// Hub is the network hub: it owns the transport, query tracker, message router,
// and the registry it routes against. Everything routing-sensitive flows through
// proto.PeerID. The hub wires reg.OnOffline to the tracker so a terminal/
// transport offline cascades query cancellation.
type Hub struct {
	reg       *peer.Registry
	transport *service.WebSocketTransport
	router    *service.MessageRouter
	authToken string

	// Read-path deps for the HTTP route groups. Optional and nil-safe: the
	// peer-read handlers degrade gracefully (no inbound-health probe) when these
	// are unset, mirroring Python's getattr(state, ..., None) pattern. Wired via
	// WithReadDeps from main once the service.AskTracker / state.Store exist.
	asks  *service.AskTracker
	store *state.Store

	// messaging is the optional /notify + /broadcast route group, wired via
	// WithMessaging when the daemon has built a service.PeerDelivery. nil → those
	// endpoints are not registered (the spike daemon has no service.PeerDelivery yet).
	messaging *MessagingRoutes
	// deliveryTraces is shared by notify and ask lifecycle paths so HTTP and MCP
	// asks write the same durable breadcrumb sequence as notifications.
	deliveryTraces deliveryTracer

	// lifecycle is the optional tmux-lifecycle hook route group (pane/session/
	// window/client), wired via WithLifecycle. nil → the /hooks/lifecycle/*
	// endpoints are not registered. Built in main with the tmux PaneLister.
	lifecycle *LifecycleHandler

	// ask holds the ask-lifecycle route dependencies (service.AskTracker + service.PeerDelivery
	// + the narrow registry seam), wired via WithAskLifecycle. The
	// /ask·/ack·/answer·/query·/asks/* handlers 503 while unwired.
	ask *askLifecycleDeps

	// Optional /schedules dependencies, wired via WithSchedules.
	scheduleStore       scheduleStore
	scheduleWaker       scheduleWaker
	schedulesConfigured bool

	// session holds the session-wiring route dependencies (registry + query
	// tracker + queued-delivery store), wired via WithSessionRoutes. The
	// /session/update·/deliveries/pending handlers 503 while unwired.
	session *sessionDeps

	// reviews is the JSON-backed review-queue store behind the /reviews routes,
	// wired via WithReviews. nil → a default store at ~/.repowire/review_queue.json
	// is created lazily at Routes() time so the endpoints are always available.
	reviews *ReviewQueueStore

	// relay is the optional relay config behind the /shares proxy routes, wired
	// via WithShares. nil (or disabled) → POST/DELETE 503, GET returns []
	// (matches Python when relay is not configured).
	relay *RelayConfig

	// relayStatus, when set (WithRelayStatus), returns the relay-client health
	// sub-object for /health. It also drives lazy self-heal: main's closure calls
	// the relay client's EnsureRunning before returning the snapshot. A plain func
	// (not the relay type) keeps hub decoupled from the relay package.
	relayStatus func() map[string]any

	// work holds the tracked-work / durable-job route dependencies (work store,
	// JobRunner, SessionControl, assigned-peer resolver), wired via WithWork /
	// assigned-peer resolver. The /work·/jobs handlers 503 while unwired.
	work *workRoutes

	// spawn holds the spawn-kill-restart route dependencies (SpawnService + the
	// narrow spawnRegistry seam + service.AskTracker quiesce barrier), wired via WithSpawn.
	// nil → the /spawn·/kill-peer·/peers/{name}/{restart,switch-backend,rehook}
	// handlers 503. Built in main with the real TmuxController + PaneOwnership.
	spawn *spawnDeps

	// mcp holds the POST /mcp (MCP-over-HTTP JSON-RPC) route dependencies —
	// config.MCPHTTPConfig + the service.PeerDelivery notify_peer/broadcast
	// dispatch to — wired via WithMCP. nil, or cfg.Enabled=false, → /mcp is not
	// registered. See routes_mcp.go.
	mcp           *mcpDeps
	jobCompletion *service.JobCompletion
}

func (h *Hub) WithJobCompletion(completion *service.JobCompletion) *Hub {
	h.jobCompletion = completion
	return h
}

// WithReviews wires an explicit review-queue store onto the hub (e.g. a test
// store under a temp dir). When unset, Routes() lazily creates the default
// JSON-backed store. Returns the hub for chaining; call before Routes.
func (h *Hub) WithReviews(store *ReviewQueueStore) *Hub {
	h.reviews = store
	return h
}

// WithRelayStatus wires the relay-client health provider (see relayStatus).
// Returns the hub for chaining; call before Routes.
func (h *Hub) WithRelayStatus(fn func() map[string]any) *Hub {
	h.relayStatus = fn
	return h
}

// WithSchedules attaches the /schedules route group built over the schedules
// store and the scheduler wake. The scheduler's firing loop is started/stopped
// by main (it owns the goroutine lifecycle); this only wires the routes.
// Returns the hub for chaining; call before Routes.
func (h *Hub) WithSchedules(store scheduleStore, scheduler scheduleWaker) *Hub {
	h.scheduleStore = store
	h.scheduleWaker = scheduler
	h.schedulesConfigured = true
	return h
}

// WithLifecycle attaches the tmux-lifecycle hook route group built over the
// supplied handler. The handler is constructed in main (it needs the tmux
// PaneLister, a host concern). Returns the hub for chaining; call before Routes.
func (h *Hub) WithLifecycle(lh *LifecycleHandler) *Hub {
	h.lifecycle = lh
	return h
}

// WithMessaging attaches the messaging (notify/broadcast) route group built over
// the supplied service.PeerDelivery and optional delivery-trace store. The registry is
// the LazyRepair seam; auth reuses the hub's requireAuth wrapper. Returns the
// hub for chaining; call before Routes.
func (h *Hub) WithMessaging(delivery *service.PeerDelivery, traces deliveryTracer) *Hub {
	h.deliveryTraces = traces
	h.messaging = NewMessagingRoutes(delivery, h.reg, traces)
	return h
}

// WithReadDeps injects the service.AskTracker and state.Store the HTTP read routes use
// to derive per-peer inbound health (pending-ask counts, last injection
// success/failure). Returns the hub for chaining. nil-safe: handlers skip the
// corresponding health fields when a dep is absent.
func (h *Hub) WithReadDeps(asks *service.AskTracker, store *state.Store) *Hub {
	h.asks = asks
	h.store = store
	return h
}

// NewHub constructs the hub over an already-built registry, minting a fresh
// transport. The transport, tracker, and router are created here; OnOffline is
// wired so the registry can cascade query cancellation without learning about
// the tracker's shape. Use newHubWithTransport when the registry was built with
// the same transport as its liveness seam (the real wiring order in main).
func NewHub(reg *peer.Registry, authToken string) *Hub {
	return NewHubWithTransport(reg, service.NewWebSocketTransport(), authToken)
}

// NewHubWithTransport wraps a registry around a pre-built transport so callers
// can hand the SAME transport to peer.NewRegistry first (chicken-and-egg: the
// registry needs a peer.Transport at construction, and the hub needs that
// registry — building the transport up front breaks the cycle). This is the
// real wiring order in main.
func NewHubWithTransport(reg *peer.Registry, transport *service.WebSocketTransport, authToken string) *Hub {
	h := &Hub{
		reg:       reg,
		transport: transport,
		router:    service.NewMessageRouter(transport, reg),
		authToken: authToken,
	}
	return h
}

// Transport exposes the live transport so the registry can be constructed with
// it (peer.Transport) before the hub wraps it. Used by main wiring.
func (h *Hub) Transport() *service.WebSocketTransport { return h.transport }

// Router exposes the message router for HTTP routes built outside this package.
func (h *Hub) Router() *service.MessageRouter { return h.router }

// Routes registers the hub's HTTP handlers on the mux.
func (h *Hub) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/ws", h.HandleWS)
	mux.HandleFunc("/health", h.health)
	h.registerPeerReadRoutes(mux)
	h.registerPeerLifecycleRoutes(mux)
	h.registerPaneRoutes(mux)
	h.registerPeerMCPRoutes(mux)
	h.registerHistoryRoutes(mux)
	h.registerACPPermissionRoutes(mux)
	h.registerShutdownRoute(mux)
	h.registerOrchestratorRoutes(mux)
	h.EventRoutes(mux)
	h.EventsStreamRoutes(mux)
	h.registerAskLifecycleRoutes(mux)
	if h.session != nil {
		h.registerSessionRoutes(mux)
	}
	if h.messaging != nil {
		h.messaging.Register(mux, h.requireAuth)
	}
	if h.lifecycle != nil {
		h.LifecycleRoutes(mux, h.lifecycle)
	}
	if h.schedulesConfigured {
		h.registerScheduleRoutes(mux)
	}
	if h.work != nil {
		h.registerWorkRoutes(mux)
	}
	if h.spawn != nil {
		h.registerSpawnRoutes(mux)
	}
	h.registerMCPRoutes(mux)
	// reviews/shares/attachments are independent leaf endpoints, always
	// registered. Reviews lazily defaults its store; shares/attachments degrade
	// gracefully when their (relay) dependency is unset.
	if h.reviews == nil {
		h.reviews = NewReviewQueueStore(DefaultReviewQueuePath())
	}
	h.registerReviewRoutes(mux)
	h.registerShareRoutes(mux)
	h.registerAttachmentRoutes(mux)
	h.registerTraceRoutes(mux)
	h.registerSessionControlRoutes(mux)
}

// requireAuth, writeJSON, writeError, and writeJSONError are package-shared HTTP
// helpers defined in routes_ask_lifecycle.go; server.go does not redeclare them.

// health returns liveness plus the live peer count and schema version. Like /ws,
// it opportunistically kicks lazy_repair in a tracked goroutine — maintenance
// piggy-backs on real requests, never a timer.
func (h *Hub) health(w http.ResponseWriter, r *http.Request) {
	h.reg.LazyRepairAsync(context.Background())
	peers := len(h.transport.GetAllSessions())
	out := map[string]any{
		"status":         "ok",
		"peers":          peers,
		"schema_version": state.SchemaVersion,
	}
	if h.relayStatus != nil {
		out["relay"] = h.relayStatus() // also triggers relay lazy self-heal
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
