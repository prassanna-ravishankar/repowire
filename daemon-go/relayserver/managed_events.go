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

// The Worker pushes owner state over one WebSocket per owner. The relay opens
// it when the first browser subscribes, fans each frame out as an SSE data
// line, and closes it after the last subscriber leaves. Relay-side daemon
// changes go to the same subscribers as `event: relay` frames.

const (
	managedSubscriberBuffer = 64
	managedPingInterval     = 30 * time.Second
	managedMaxBackoff       = 30 * time.Second
)

type eventDialer func(ctx context.Context, owner string) (*websocket.Conn, error)

type managedEvents struct {
	mu     sync.Mutex
	owners map[string]*ownerStream
	dial   eventDialer
}

type ownerStream struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	cancel context.CancelFunc
}

func newManagedEvents() *managedEvents {
	return &managedEvents{owners: map[string]*ownerStream{}}
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

// subscribe returns a frame channel that closes if the subscriber falls behind,
// and a release func. The first subscriber for an owner starts the upstream.
func (e *managedEvents) subscribe(owner string) (<-chan []byte, func()) {
	ch := make(chan []byte, managedSubscriberBuffer)
	e.mu.Lock()
	stream := e.owners[owner]
	if stream == nil {
		ctx, cancel := context.WithCancel(context.Background())
		stream = &ownerStream{subs: map[chan []byte]struct{}{}, cancel: cancel}
		e.owners[owner] = stream
		go e.pump(ctx, owner, stream)
	}
	stream.mu.Lock()
	stream.subs[ch] = struct{}{}
	stream.mu.Unlock()
	e.mu.Unlock()

	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		stream.mu.Lock()
		if _, ok := stream.subs[ch]; ok {
			delete(stream.subs, ch)
			close(ch)
		}
		empty := len(stream.subs) == 0
		stream.mu.Unlock()
		if empty && e.owners[owner] == stream {
			delete(e.owners, owner)
			stream.cancel()
		}
	}
}

// publish never blocks: a subscriber whose buffer is full is dropped (its
// channel closes) so one slow browser cannot stall the owner's stream.
func (o *ownerStream) publish(frame []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for ch := range o.subs {
		select {
		case ch <- frame:
		default:
			delete(o.subs, ch)
			close(ch)
		}
	}
}

func (e *managedEvents) publishDaemon(owner string, daemon relayDaemon) {
	e.mu.Lock()
	stream := e.owners[owner]
	e.mu.Unlock()
	if stream != nil {
		stream.publish(relayFrame(map[string]any{"type": "daemon", "daemon": daemon}))
	}
}

func dataFrame(payload []byte) []byte {
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

func relayFrame(value any) []byte {
	payload, _ := json.Marshal(value)
	return append([]byte("event: relay\n"), dataFrame(payload)...)
}

// pump holds the Worker socket for one owner until ctx ends, reconnecting with
// backoff. Subscribers see each upstream transition as a `gateway` relay frame.
func (e *managedEvents) pump(ctx context.Context, owner string, stream *ownerStream) {
	backoff := time.Second
	for {
		conn, err := e.dial(ctx, owner)
		if err == nil {
			backoff = time.Second
			stream.publish(relayFrame(map[string]any{"type": "gateway", "connected": true}))
			err = forwardFrames(ctx, conn, stream)
			_ = conn.CloseNow()
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("relay managed events: owner %s upstream: %v", owner, err)
		stream.publish(relayFrame(map[string]any{"type": "gateway", "connected": false, "error": err.Error()}))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, managedMaxBackoff)
	}
}

func forwardFrames(ctx context.Context, conn *websocket.Conn, stream *ownerStream) error {
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
		stream.publish(dataFrame(compact.Bytes()))
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
	frames, release := s.events.subscribe(user)
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
		case <-heartbeat.C:
			chunk = []byte(": keepalive\n\n")
		case frame, open := <-frames:
			if !open {
				_, _ = io.WriteString(w, "event: error\ndata: {\"error\":\"subscriber fell behind; reconnect\"}\n\n")
				flusher.Flush()
				return
			}
			chunk = frame
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
		flusher.Flush()
	}
}
