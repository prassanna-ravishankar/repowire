package hub

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// validNameRe / maxNameLen mirror daemon/routes/_shared.py exactly so circle and
// display_name validation matches the Python daemon byte-for-byte.
var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

const maxNameLen = 64

// flushWriteTimeout bounds each queued-delivery replay write on the connect
// path: a wedged client must not park the connect handshake forever.
const flushWriteTimeout = 10 * time.Second

// defaultQueueMax duplicates service/delivery.go's defaultQueueMax (the queued-
// delivery producer's per-peer cap): the connect-path flush lists at most this
// many rows. Not worth an exported seam for one shared magic number.
const defaultQueueMax = 50

func isValidIdentifier(v string) bool {
	return v != "" && len(v) <= maxNameLen && validNameRe.MatchString(v)
}

// HandleWS is the unified /ws endpoint for every agent runtime. It mirrors the
// Python websocket_endpoint: accept, require a connect frame, authenticate,
// validate, register through the registry FSM, then run the read loop until the
// socket drops — at which point the deferred teardown severs the transport and
// cancels any in-flight queries to this peer.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// Lazy repair piggy-backs on a real request; never a timer.
	h.reg.LazyRepairAsync(context.Background())

	ctx := r.Context()
	var sessionID proto.PeerID
	registered := false

	defer func() {
		// IDENTITY-CHECKED teardown: only remove the socket we own. A pane-backed
		// runtime can outlive its sidecar connection, so lazy repair owns the
		// runtime-evidence check before changing that peer's lifecycle status.
		if registered {
			if removed := h.transport.Disconnect(ctx, sessionID, conn); removed {
				p, _ := h.reg.GetPeer(sessionID)
				if transportOwnsLifecycle(p) {
					_, _ = h.reg.MarkOffline(ctx, sessionID, false)
				}
			}
		}
		_ = conn.CloseNow()
	}()

	// First frame must be connect.
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return
	}
	ftype, err := proto.ParseEnvelope(raw)
	if err != nil || ftype != proto.FrameConnect {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "First message must be connect"})
		_ = conn.Close(4000, "First message must be connect")
		return
	}

	var cf proto.ConnectFrame
	if err := json.Unmarshal(raw, &cf); err != nil {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Malformed connect frame"})
		_ = conn.Close(4000, "Malformed connect")
		return
	}

	// Authentication: constant-time compare when a token is configured.
	if h.authToken != "" {
		if cf.AuthToken == nil || !hmac.Equal([]byte(*cf.AuthToken), []byte(h.authToken)) {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Authentication failed"})
			_ = conn.Close(4001, "Authentication failed")
			log.Printf("ws: connection rejected: invalid or missing auth_token")
			return
		}
	}

	// Validate circle.
	circle := cf.Circle
	if circle == "" {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Circle is required; start in a named tmux session or use a spawn hint"})
		_ = conn.Close(4002, "Missing circle")
		return
	}
	if !isValidIdentifier(circle) {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid circle format"})
		_ = conn.Close(4002, "Invalid circle")
		return
	}

	// Validate circle_source (None|tmux|spawn_hint|fallback).
	if cf.CircleSource != nil {
		switch *cf.CircleSource {
		case "tmux", "tmux_window", "spawn_hint", "fallback":
		default:
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid circle_source"})
			_ = conn.Close(4002, "Invalid circle_source")
			return
		}
	}

	// Validate display_name.
	if !isValidIdentifier(string(cf.DisplayName)) {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid display_name format"})
		_ = conn.Close(4002, "Invalid display_name")
		return
	}

	// Validate backend. mcp-http is a daemon-owned identity, not a ws runtime.
	backend := cf.Backend
	if backend == "" {
		backend = proto.AgentClaudeCode
	}
	if !backend.Valid() {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid backend"})
		_ = conn.Close(4002, "Invalid backend")
		return
	}
	if backend == proto.AgentMCPHTTP {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "mcp-http is not a WebSocket backend"})
		_ = conn.Close(4002, "Invalid backend")
		return
	}

	// Validate role.
	requestedRole := cf.Role
	role := requestedRole
	if role == "" {
		role = proto.RoleAgent
	}
	if !role.Valid() {
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid role"})
		_ = conn.Close(4002, "Invalid role")
		return
	}

	path := cf.Path
	if path != nil && *path != "" {
		normalized, err := filepath.Abs(*path)
		if err != nil {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid path"})
			_ = conn.Close(4003, "Invalid path")
			return
		}
		normalized = filepath.Clean(normalized)
		if normalized == string(os.PathSeparator) {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Invalid path: root directory not allowed"})
			_ = conn.Close(4003, "Invalid path")
			log.Printf("ws: registration rejected: invalid path %s", *path)
			return
		}
		path = &normalized
	}
	if cf.PaneID != nil && *cf.PaneID != "" {
		verifiedCircle, verifiedRole, code, detail := h.verifiedPaneIdentity(*cf.PaneID, backend, derefString(path), circle, requestedRole)
		if code != http.StatusOK {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: detail})
			_ = conn.Close(4003, "Unverified pane identity")
			return
		}
		circle, role = verifiedCircle, verifiedRole
	} else if h.spawn != nil && !isDaemonMobilePeer(cf.DisplayName, path, role) {
		if cf.PeerID == nil {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Pane-less WebSocket registration requires an existing peer identity"})
			_ = conn.Close(4003, "Unverified pane-less identity")
			return
		}
		mapping, ok := h.reg.GetMapping(*cf.PeerID)
		mappingPath := ""
		if ok && mapping.Path != nil {
			mappingPath = *mapping.Path
		}
		if !ok || mapping.DisplayName != cf.DisplayName || mapping.Backend != backend || mapping.Circle != circle || mapping.Role != role || service.NormPath(mappingPath) != service.NormPath(derefString(path)) {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Pane-less WebSocket identity does not match its durable mapping"})
			_ = conn.Close(4003, "Pane-less identity mismatch")
			return
		}
	}

	machine, _ := os.Hostname()
	params := peer.AllocateParams{
		Circle:        circle,
		Backend:       backend,
		Model:         cf.Model,
		Path:          path,
		PaneID:        cf.PaneID,
		TmuxSession:   cf.TmuxSession,
		Machine:       machine,
		Role:          role,
		ClaimedPeerID: cf.PeerID,
		AgentPID:      cf.AgentPID,
	}
	if isDaemonMobilePeer(cf.DisplayName, path, role) {
		params.PreferredDisplayName = &cf.DisplayName
	}
	if len(cf.ModelDetails) > 0 || len(cf.Capabilities) > 0 || cf.HookVersion != nil {
		md := map[string]any{}
		if cf.HookVersion != nil {
			md["hook_version"] = *cf.HookVersion
		}
		if len(cf.Capabilities) > 0 {
			md["capabilities"] = cf.Capabilities
		}
		if len(cf.ModelDetails) > 0 {
			md["model_details"] = cf.ModelDetails
		}
		params.Metadata = md
	}

	peerID, assignedName, err := h.reg.AllocateAndRegister(ctx, params)
	if err != nil {
		if errors.Is(err, peer.ErrPeerRetired) {
			_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Code: "peer_retired", Error: err.Error()})
			_ = conn.Close(4004, "Peer retired")
			log.Printf("ws: connect rejected (retired peer_id)")
			return
		}
		_ = wsjson.Write(ctx, conn, proto.ErrorFrame{Type: proto.FrameError, Error: "Registration failed"})
		_ = conn.Close(4002, "Registration failed")
		return
	}
	sessionID = peerID
	registered = true

	// The registry is the source of truth for pane ownership: it may have rejected
	// the connect-frame pane (sticky orchestrator / unproven live-holder claim) and
	// registered the peer pane-less. Record the pane the registry ACTUALLY assigned,
	// not the raw frame, so delivery tracing / pane_injected stays truthful.
	connPane := cf.PaneID
	if p, ok := h.reg.GetPeer(peerID); ok {
		connPane = p.PaneID
	}

	h.transport.Connect(ctx, &service.ConnectionInfo{
		SessionID:   peerID,
		WS:          conn,
		PaneID:      connPane,
		DisplayName: assignedName,
		ConnectedAt: time.Now().UTC(),
	})

	if err := wsjson.Write(ctx, conn, proto.ConnectedFrame{
		Type:        proto.FrameConnected,
		SessionID:   peerID,
		DisplayName: assignedName,
	}); err != nil {
		return
	}
	log.Printf("ws: connected %s@%s (%s, %s)", assignedName, circle, peerID, backend)

	// Flush-on-connect: the moment a polling peer (re)connects, drain its durable
	// queued-delivery queue and replay each row directly onto this connection.
	// This is the consumer for service/delivery.go's queueNotify producer — a
	// no-live-transport notify is enqueued, and reconnecting drains it here. The
	// seed gate already guards live deliveries (service/delivery.go's
	// gateOnSeedSettled); a peer that just completed the connect handshake is
	// past pending_first_turn registration, so we replay without re-gating.
	// Best-effort: a write failure stops the replay (a row is deleted only
	// after its frame is written, so unsent rows survive for the next
	// reconnect).
	h.flushQueuedDeliveries(ctx, conn, peerID, assignedName)

	// Read loop: dispatch frames by type until the socket drops.
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		h.dispatch(ctx, sessionID, raw)
	}
}

func transportOwnsLifecycle(p *proto.Peer) bool {
	return p == nil || p.PaneID == nil || *p.PaneID == ""
}

func isDaemonMobilePeer(name proto.DisplayName, path *string, role proto.PeerRole) bool {
	if role != proto.RoleService || path == nil {
		return false
	}
	value := string(name)
	return (value == "telegram" || value == "slack") && *path == "/"+value
}

// flushQueuedDeliveries replays the durable queued-delivery queue for the freshly
// connected peer onto this connection. notify rows become a notify frame, ask rows
// an ask frame — the same wire shapes router.SendNotification / router.SendAsk put
// on the wire (clients depend on them). The store is the nil-checked *state.Store
// threaded via WithReadDeps; nil → no-op. It LISTS first and deletes each row only
// after its frame is successfully written, so a write failure on a dropping socket
// leaves unsent rows for the next reconnect (no data loss). It never kills the
// connect path.
func (h *Hub) flushQueuedDeliveries(ctx context.Context, conn *websocket.Conn, id proto.PeerID, toName proto.DisplayName) {
	if h.store == nil {
		return
	}
	// List-then-delete-after-success, NOT drain-then-replay: deleting on read
	// loses an owed delivery if the replay write then fails on a dropping socket.
	// A row is removed only once its frame has been written. (Parity with the
	// Python list_for_peer -> send -> delete split.)
	pending, err := h.store.ListDeliveries(ctx, string(id), defaultQueueMax, time.Time{})
	if err != nil {
		log.Printf("ws: flush-on-connect list for %s failed: %v", id, err)
		return
	}
	for _, d := range pending {
		var frame map[string]any
		switch d.Kind {
		case state.DeliveryNotify:
			frame = map[string]any{
				"type":        string(proto.FrameNotify),
				"delivery_id": d.DeliveryID,
				"from_peer":   d.FromPeerName,
				"to_peer":     string(toName),
				"text":        d.Text,
			}
		case state.DeliveryAsk:
			frame = map[string]any{
				"type":        string(proto.FrameAsk),
				"delivery_id": d.DeliveryID,
				"from_peer":   d.FromPeerName,
				"to_peer":     string(toName),
				"text":        d.Text,
			}
			if d.CorrelationID != nil {
				frame["correlation_id"] = *d.CorrelationID
			}
		default:
			continue
		}
		if len(d.Attachments) > 0 {
			frame["attachments"] = d.Attachments
		}
		writeCtx, cancel := context.WithTimeout(ctx, flushWriteTimeout)
		err := wsjson.Write(writeCtx, conn, frame)
		cancel()
		if err != nil {
			log.Printf("ws: flush-on-connect replay to %s stopped after write failure (%s): %v", id, d.DeliveryID, err)
			return // row NOT deleted — a later reconnect retries it
		}
		// Delete only after the frame is on the wire.
		if _, err := h.store.DeleteDelivery(ctx, d.DeliveryID); err != nil {
			log.Printf("ws: flush-on-connect delete after replay to %s failed (%s): %v", id, d.DeliveryID, err)
		}
	}
}

// dispatch routes one inbound frame to the right handler. Unknown / malformed
// frames are logged, not fatal — a single bad message must not kill the loop.
func (h *Hub) dispatch(ctx context.Context, id proto.PeerID, raw []byte) {
	ftype, err := proto.ParseEnvelope(raw)
	if err != nil {
		log.Printf("ws: malformed frame from %s: %v", id, err)
		return
	}
	switch ftype {
	case proto.FrameStatus:
		var f proto.StatusFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			log.Printf("ws: bad status frame from %s: %v", id, err)
			return
		}
		_ = h.reg.UpdateStatus(ctx, id, normalizeStatus(f.Status))
		if f.TurnState != nil && validTurnState(*f.TurnState) {
			h.reg.UpdateTurnState(ctx, id, *f.TurnState)
		}

	case proto.FrameUpdateDisplayName:
		var f proto.UpdateDisplayNameFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			log.Printf("ws: bad update_display_name frame from %s: %v", id, err)
			return
		}
		if isValidIdentifier(string(f.DisplayName)) {
			if _, err := h.reg.UpdateDisplayName(ctx, id, f.DisplayName); err != nil {
				log.Printf("ws: update_display_name from %s failed: %v", id, err)
			}
		} else {
			log.Printf("ws: update_display_name from %s invalid name %q", id, f.DisplayName)
		}

	case proto.FramePong:
		h.transport.ResolvePong(id, decodeMap(raw))

	case proto.FrameDeliveryAck:
		data := decodeMap(raw)
		deliveryID, _ := data["delivery_id"].(string)
		if deliveryID == "" {
			log.Printf("ws: delivery_ack from %s missing delivery_id, dropping", id)
			return
		}
		h.transport.ResolveDeliveryAck(id, deliveryID, data)

	case proto.FrameError:
		var f proto.ErrorFrame
		_ = json.Unmarshal(raw, &f)
		log.Printf("ws: client %s reported error: %s", id, f.Error)

	default:
		log.Printf("ws: unknown message type from %s: %s", id, ftype)
	}
}

// normalizeStatus mirrors the Python status_map: idle -> online; anything
// unrecognized -> online. busy/offline pass through.
func normalizeStatus(s proto.PeerStatus) proto.PeerStatus {
	switch s {
	case proto.StatusBusy:
		return proto.StatusBusy
	case proto.StatusOffline:
		return proto.StatusOffline
	case proto.StatusOnline, "idle":
		return proto.StatusOnline
	}
	return proto.StatusOnline
}

// validTurnState mirrors the Python turn_state allowlist (the empty "unknown"
// state is never accepted off the wire).
func validTurnState(ts proto.TurnState) bool {
	switch ts {
	case proto.TurnIdle, proto.TurnWorking, proto.TurnAwaitingInput, proto.TurnPendingFirstTurn:
		return true
	}
	return false
}

// decodeMap best-effort decodes a frame to a generic map for pong/delivery_ack,
// which the transport hands back to waiters verbatim.
func decodeMap(raw []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}
