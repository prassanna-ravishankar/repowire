package service

// transport.go owns WebSocketTransport, the concrete per-connection socket
// registry that the router and delivery services send frames through. It
// implements peer.Transport (the registry's liveness/sever seam) so the same
// transport instance is both the registry's runtime-evidence source and the
// socket the hub package's /ws handler serves on.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// ErrNotConnected is returned by Send when the target peer has no live socket.
var ErrNotConnected = errors.New("hub: peer not connected")

// ErrTransportUnavailable means a socket existed but could not accept a write.
// Delivery treats it as unreachable, never as an authorization failure.
var ErrTransportUnavailable = errors.New("hub: transport unavailable")

// deliveryAckTimeout mirrors the Python transport's 0.75s best-effort window.
const deliveryAckTimeout = 750 * time.Millisecond

// pingTimeout mirrors the Python transport's default pong wait.
const pingTimeout = 5 * time.Second

// writeTimeout bounds each WebSocket write. The delivery ack timeout only starts
// after a write returns, so a wedged half-open socket needs its own deadline.
const writeTimeout = 10 * time.Second

// ConnectionInfo is the live socket record for one connected peer. SessionID IS
// the peer_id (the daemon-assigned identity), so it is typed proto.PeerID.
type ConnectionInfo struct {
	SessionID   proto.PeerID
	WS          *websocket.Conn
	PaneID      *string
	DisplayName proto.DisplayName
	ConnectedAt time.Time
	// HookVersion is what the connecting ws-hook declared in its connect frame
	// (0 when it declared none).
	HookVersion int

	// AwaitReady marks a socket that is not ready for frames until its
	// owner calls Ready (the hub writes the connected frame first). A frame
	// re-sent to it after a replacement waits for that. Without it the socket
	// is ready on Connect.
	AwaitReady bool

	// ended closes when this socket stops being the peer's connection
	// (replaced, disconnected, or closed), so a frame waiting on it for a
	// delivery_ack learns that it never will. ready closes once the socket
	// may take frames. Both are set by Connect.
	ended     chan struct{}
	endOnce   sync.Once
	ready     chan struct{}
	readyOnce sync.Once
}

func (c *ConnectionInfo) markReady() {
	c.readyOnce.Do(func() { close(c.ready) })
}

func (c *ConnectionInfo) end() {
	if c.ended != nil {
		c.endOnce.Do(func() { close(c.ended) })
	}
}

// ackKey indexes an in-flight delivery ack by (peer, delivery id). Both halves
// matter: the same delivery_id can be reused across peers.
type ackKey struct {
	id         proto.PeerID
	deliveryID string
}

// WebSocketTransport owns every live socket and the in-flight pong/delivery-ack
// channels. All maps are guarded by mu. It implements peer.Transport so the
// registry can probe liveness and sever a retired peer's socket.
type WebSocketTransport struct {
	mu    sync.RWMutex
	conns map[proto.PeerID]*ConnectionInfo
	pongs map[proto.PeerID]chan map[string]any
	acks  map[ackKey]chan map[string]any
	acp   *ACPManager
}

// EnableACP turns on metadata.acp routing and owns the subprocess manager.
func (t *WebSocketTransport) EnableACP(enabled bool) {
	t.mu.Lock()
	if enabled && t.acp == nil {
		t.acp = NewACPManager()
	}
	var closing *ACPManager
	if !enabled && t.acp != nil {
		closing = t.acp
		t.acp = nil
	}
	t.mu.Unlock()
	if closing != nil {
		closing.Close()
	}
}

var _ peer.Transport = (*WebSocketTransport)(nil)

// NewWebSocketTransport returns an empty transport ready to accept connections.
func NewWebSocketTransport() *WebSocketTransport {
	return &WebSocketTransport{
		conns: make(map[proto.PeerID]*ConnectionInfo),
		pongs: make(map[proto.PeerID]chan map[string]any),
		acks:  make(map[ackKey]chan map[string]any),
	}
}

// Connect stores a fresh connection. If a connection already exists for the id
// it is closed first — a reconnect must not silently leak the old socket.
func (t *WebSocketTransport) Connect(ctx context.Context, info *ConnectionInfo) {
	if info.ended == nil {
		info.ended = make(chan struct{})
	}
	if info.ready == nil {
		info.ready = make(chan struct{})
		if !info.AwaitReady {
			info.markReady()
		}
	}
	t.mu.Lock()
	old := t.conns[info.SessionID]
	t.conns[info.SessionID] = info
	t.mu.Unlock()
	if old != nil && old != info {
		old.end()
	}

	if old != nil && old.WS != nil {
		_ = old.WS.Close(websocket.StatusNormalClosure, "superseded by reconnect")
	}
}

// Ready marks ws, id's current socket, ready for frames (the connect
// handshake is done). A no-op for any other socket.
func (t *WebSocketTransport) Ready(id proto.PeerID, ws *websocket.Conn) {
	t.mu.RLock()
	info, ok := t.conns[id]
	t.mu.RUnlock()
	if ok && info.WS == ws {
		info.markReady()
	}
}

// Disconnect removes the connection ONLY if the stored socket is the same one
// the caller holds. This is the guard for the old-handler-removing-the-new-
// connection race: a stale ws goroutine tearing down must not evict the fresh
// reconnect that already took its place. Returns true when it actually removed.
func (t *WebSocketTransport) Disconnect(ctx context.Context, id proto.PeerID, ws *websocket.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.conns[id]
	if !ok {
		return false
	}
	if info.WS != ws {
		// A newer connection replaced this one; the stale handler must not evict it.
		return false
	}
	delete(t.conns, id)
	info.end()
	return true
}

// Send writes a frame to the peer's socket as JSON.
func (t *WebSocketTransport) Send(ctx context.Context, id proto.PeerID, v any) error {
	_, err := t.send(ctx, id, v)
	return err
}

// send writes v to id's current socket and returns that connection.
func (t *WebSocketTransport) send(ctx context.Context, id proto.PeerID, v any) (*ConnectionInfo, error) {
	t.mu.RLock()
	info, ok := t.conns[id]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotConnected, id)
	}
	return info, write(ctx, info, v)
}

func write(ctx context.Context, info *ConnectionInfo, v any) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := wsjson.Write(writeCtx, info.WS, v); err != nil {
		return fmt.Errorf("%w: %v", ErrTransportUnavailable, err)
	}
	return nil
}

func (t *WebSocketTransport) current(id proto.PeerID) *ConnectionInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.conns[id]
}

// SendAndWaitDeliveryAck sends a frame carrying a delivery_id and waits up to
// timeout for the matching delivery_ack. Best-effort: a timeout on a socket
// that stays open returns (nil, nil) rather than an error, since older hooks
// never ack. A socket that ends first never acks either (#472): when a newer
// connection replaced it, the frame is sent once more there, under the same
// delivery_id and the same overall timeout; when none did, the wait fails with
// ErrNotConnected, so the caller queues or fails the delivery instead of
// reporting it delivered. A frame without a string delivery_id is sent as a
// plain Send (no ack channel registered).
func (t *WebSocketTransport) SendAndWaitDeliveryAck(ctx context.Context, id proto.PeerID, v any, timeout time.Duration) (map[string]any, error) {
	deliveryID, ok := extractDeliveryID(v)
	if !ok || deliveryID == "" {
		return nil, t.Send(ctx, id, v)
	}
	if timeout <= 0 {
		timeout = deliveryAckTimeout
	}

	key := ackKey{id: id, deliveryID: deliveryID}
	ch := make(chan map[string]any, 1)
	t.mu.Lock()
	t.acks[key] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.acks, key)
		t.mu.Unlock()
	}()

	info, err := t.send(ctx, id, v)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	resent := false
	for {
		select {
		case data := <-ch:
			return data, nil
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-info.ended:
			select {
			case data := <-ch: // the ack beat the close
				return data, nil
			default:
			}
			if next := t.current(id); next != nil && next != info && !resent {
				// The replacement takes frames only once its handshake is
				// done; a frame ahead of its connected frame would break it.
				select {
				case <-next.ready:
				case <-next.ended:
					return nil, fmt.Errorf("%w: %s: socket closed before delivery_ack", ErrNotConnected, id)
				case <-timer.C:
					return nil, fmt.Errorf("%w: %s: socket closed before delivery_ack and its replacement was not ready", ErrNotConnected, id)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				resent, info = true, next
				if err := write(ctx, info, v); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("%w: %s: socket closed before delivery_ack", ErrNotConnected, id)
		}
	}
}

// ConnectionHookVersion reports the hook version the peer's live socket
// connected with.
func (t *WebSocketTransport) ConnectionHookVersion(id proto.PeerID) (int, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	info, ok := t.conns[id]
	if !ok {
		return 0, false
	}
	return info.HookVersion, true
}

// PendingDeliveryAcks counts frames sent to id that still await their
// delivery_ack.
func (t *WebSocketTransport) PendingDeliveryAcks(id proto.PeerID) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := 0
	for key := range t.acks {
		if key.id == id {
			n++
		}
	}
	return n
}

// ResolveDeliveryAck hands a delivery_ack frame to whoever is waiting on it.
func (t *WebSocketTransport) ResolveDeliveryAck(id proto.PeerID, deliveryID string, data map[string]any) {
	if deliveryID == "" {
		return
	}
	t.mu.Lock()
	ch, ok := t.acks[ackKey{id: id, deliveryID: deliveryID}]
	if ok {
		delete(t.acks, ackKey{id: id, deliveryID: deliveryID})
	}
	t.mu.Unlock()
	if ok {
		ch <- data
	}
}

// Ping sends a ping frame and waits up to timeout for the matching pong.
func (t *WebSocketTransport) Ping(ctx context.Context, id proto.PeerID, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = pingTimeout
	}
	ch := make(chan map[string]any, 1)
	t.mu.Lock()
	if _, exists := t.conns[id]; !exists {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNotConnected, id)
	}
	t.pongs[id] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pongs, id)
		t.mu.Unlock()
	}()

	if err := t.Send(ctx, id, map[string]any{"type": string(proto.FramePing)}); err != nil {
		return nil, err
	}
	select {
	case data := <-ch:
		return data, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("hub: ping timeout for %s", id)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ResolvePong hands a pong frame to whoever is waiting on Ping.
func (t *WebSocketTransport) ResolvePong(id proto.PeerID, data map[string]any) {
	t.mu.Lock()
	ch, ok := t.pongs[id]
	if ok {
		delete(t.pongs, id)
	}
	t.mu.Unlock()
	if ok {
		ch <- data
	}
}

// ConnectionPaneID returns the pane_id recorded on the live connection, for
// truthful delivery-trace logging. ("", false) when not connected (or no pane).
// Mirrors the Python transport.get_connection_pane_id used in the delivery
// trace log line.
func (t *WebSocketTransport) ConnectionPaneID(id proto.PeerID) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	info, ok := t.conns[id]
	if !ok || info.PaneID == nil {
		return "", false
	}
	return *info.PaneID, true
}

// ACPRoute routes only when the experiment is enabled and metadata.acp has a
// usable command. Invalid blocks fall through to WebSocket delivery.
func (t *WebSocketTransport) ACPRoute(target *proto.Peer) (*ACPRouteDecision, bool) {
	t.mu.RLock()
	manager := t.acp
	t.mu.RUnlock()
	if manager == nil || target == nil || target.Metadata == nil {
		return nil, false
	}
	raw, ok := target.Metadata["acp"].(map[string]any)
	if !ok {
		return nil, false
	}
	command, _ := raw["command"].(string)
	if command == "" {
		return nil, false
	}
	spec := ACPPeerSpec{PeerID: target.PeerID, Command: command, CWD: target.Path, Env: map[string]string{}}
	if cwd, ok := raw["cwd"].(string); ok && cwd != "" {
		spec.CWD = cwd
	}
	if values, ok := raw["args"].([]any); ok {
		for _, value := range values {
			if arg, ok := value.(string); ok {
				spec.Args = append(spec.Args, arg)
			}
		}
	} else if values, ok := raw["args"].([]string); ok {
		spec.Args = append(spec.Args, values...)
	}
	if values, ok := raw["env"].(map[string]any); ok {
		for key, value := range values {
			if text, ok := value.(string); ok {
				spec.Env[key] = text
			}
		}
	} else if values, ok := raw["env"].(map[string]string); ok {
		for key, value := range values {
			spec.Env[key] = value
		}
	}
	return &ACPRouteDecision{PeerID: target.PeerID, Spec: spec, manager: manager}, true
}

// IsConnected reports whether the peer has a live socket. Used by the registry's
// ghost-eviction pass.
func (t *WebSocketTransport) IsConnected(id proto.PeerID) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.conns[id]
	return ok
}

// GetAllSessions returns the peer_ids of every connected peer.
func (t *WebSocketTransport) GetAllSessions() []proto.PeerID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]proto.PeerID, 0, len(t.conns))
	for id := range t.conns {
		out = append(out, id)
	}
	return out
}

// Close severs a peer's socket (peer.Transport). It removes the record under
// lock and closes the underlying connection. Idempotent. Registry removal and
// terminal retirement both use this seam, so the close reason stays generic.
func (t *WebSocketTransport) Close(id proto.PeerID) error {
	t.mu.Lock()
	info, ok := t.conns[id]
	manager := t.acp
	if ok {
		delete(t.conns, id)
	}
	t.mu.Unlock()
	if ok {
		info.end()
	}
	if ok && info.WS != nil {
		err := info.WS.Close(websocket.StatusGoingAway, "peer removed by daemon")
		if manager != nil {
			manager.Drop(id)
		}
		return err
	}
	if manager != nil {
		manager.Drop(id)
	}
	return nil
}

// CloseACP tears down every brokered subprocess during daemon shutdown.
// DropACP ends one peer's ACP child, if it has one (the ACP plug's stop).
func (t *WebSocketTransport) DropACP(id proto.PeerID) {
	t.mu.RLock()
	manager := t.acp
	t.mu.RUnlock()
	if manager != nil {
		manager.Drop(id)
	}
}

func (t *WebSocketTransport) CloseACP() {
	t.mu.RLock()
	manager := t.acp
	t.mu.RUnlock()
	if manager != nil {
		manager.Close()
	}
}

func (t *WebSocketTransport) SetACPPermissionHandler(handler ACPPermissionHandler) {
	t.mu.RLock()
	manager := t.acp
	t.mu.RUnlock()
	if manager != nil {
		manager.SetPermissionHandler(handler)
	}
}

// extractDeliveryID pulls the delivery_id out of an outbound frame regardless of
// whether it's a typed *proto.QueryFrame, a map, or carries no id at all.
func extractDeliveryID(v any) (string, bool) {
	switch f := v.(type) {
	case *proto.QueryFrame:
		if f.DeliveryID != nil {
			return *f.DeliveryID, true
		}
		return "", false
	case proto.QueryFrame:
		if f.DeliveryID != nil {
			return *f.DeliveryID, true
		}
		return "", false
	case map[string]any:
		if id, ok := f["delivery_id"].(string); ok {
			return id, true
		}
		return "", false
	}
	return "", false
}
