package relayserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Each browser SSE stream gets its own Worker /events WebSocket, so every
// subscriber starts from the Worker's snapshot. The Worker's UI sockets
// hibernate, so an idle stream costs nothing. Relay-side daemon changes are
// delivered to each of the owner's subscribers as `event: relay` frames.

const (
	managedSubscriberBuffer = 64
	managedPingInterval     = 30 * time.Second
	managedMaxBackoff       = 30 * time.Second
)

type eventDialer func(ctx context.Context, owner string) (*websocket.Conn, error)

type managedEvents struct {
	mu     sync.Mutex
	owners map[string]map[*eventSubscriber]struct{}
	dial   eventDialer
}

type eventSubscriber struct {
	frames   chan []byte
	overflow chan struct{}
	once     sync.Once
}

func newManagedEvents() *managedEvents {
	return &managedEvents{owners: map[string]map[*eventSubscriber]struct{}{}}
}

func (m *managedWorker) dialEvents(ctx context.Context, owner string) (*websocket.Conn, error) {
	u := m.url("/events", "")
	u = "ws" + strings.TrimPrefix(u, "http")
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: m.header(owner)})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(16 << 20)
	return conn, nil
}

// subscribe starts this subscriber's upstream; release stops it.
func (e *managedEvents) subscribe(owner string) (*eventSubscriber, func()) {
	sub := &eventSubscriber{frames: make(chan []byte, managedSubscriberBuffer), overflow: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	if e.owners[owner] == nil {
		e.owners[owner] = map[*eventSubscriber]struct{}{}
	}
	e.owners[owner][sub] = struct{}{}
	e.mu.Unlock()
	go e.pump(ctx, owner, sub)
	return sub, func() {
		cancel()
		e.mu.Lock()
		delete(e.owners[owner], sub)
		if len(e.owners[owner]) == 0 {
			delete(e.owners, owner)
		}
		e.mu.Unlock()
	}
}

// send applies backpressure to this subscriber's own upstream.
func (s *eventSubscriber) send(ctx context.Context, frame []byte) bool {
	select {
	case s.frames <- frame:
		return true
	case <-ctx.Done():
		return false
	}
}

// offer never blocks the daemon tunnel: a subscriber too far behind to take a
// relay frame is told to reconnect instead of silently missing it.
func (s *eventSubscriber) offer(frame []byte) {
	select {
	case s.frames <- frame:
	default:
		s.once.Do(func() { close(s.overflow) })
	}
}

func (e *managedEvents) publishDaemon(owner string, daemon relayDaemon) {
	frame := relayFrame(map[string]any{"type": "daemon", "daemon": daemon})
	e.mu.Lock()
	defer e.mu.Unlock()
	for sub := range e.owners[owner] {
		sub.offer(frame)
	}
}

func dataFrame(payload []byte) []byte {
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

func relayFrame(value any) []byte {
	payload, _ := json.Marshal(value)
	return append([]byte("event: relay\n"), dataFrame(payload)...)
}

// pump holds one subscriber's Worker socket until ctx ends, reconnecting with
// backoff. Each upstream transition is reported as a `gateway` relay frame.
func (e *managedEvents) pump(ctx context.Context, owner string, sub *eventSubscriber) {
	backoff := time.Second
	for {
		conn, err := e.dial(ctx, owner)
		if err == nil {
			backoff = time.Second
			sub.send(ctx, relayFrame(map[string]any{"type": "gateway", "connected": true}))
			err = forwardFrames(ctx, conn, sub)
			_ = conn.CloseNow()
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("relay managed events: owner %s upstream: %v", owner, err)
		sub.send(ctx, relayFrame(map[string]any{"type": "gateway", "connected": false, "error": err.Error()}))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, managedMaxBackoff)
	}
}

func forwardFrames(ctx context.Context, conn *websocket.Conn, sub *eventSubscriber) error {
	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go func() {
		ticker := time.NewTicker(managedPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if err := conn.Ping(pingCtx); err != nil {
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			log.Printf("relay managed events: dropped non-JSON frame: %v", err)
			continue
		}
		if !sub.send(ctx, dataFrame(compact.Bytes())) {
			return ctx.Err()
		}
	}
}

// managedEventsStream is GET /api/v1/managed/events. It opens with a relay
// frame listing this user's relay daemons; the Worker sends its own snapshot
// once the upstream connects.
func (s *Server) managedEventsStream(w http.ResponseWriter, r *http.Request, user string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		managedError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	sub, release := s.events.subscribe(user)
	defer release()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(relayFrame(map[string]any{"type": "daemons", "daemons": s.relayDaemons(user)}))
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		var chunk []byte
		select {
		case <-r.Context().Done():
			return
		case <-sub.overflow:
			_, _ = io.WriteString(w, "event: error\ndata: {\"error\":\"subscriber fell behind; reconnect\"}\n\n")
			flusher.Flush()
			return
		case <-heartbeat.C:
			chunk = []byte(": keepalive\n\n")
		case chunk = <-sub.frames:
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
		flusher.Flush()
	}
}
