package peer

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/proto"
)

// events.go adds the in-memory dashboard event window and the addressing-side
// peer lookups (by pane, by string identifier) that the HTTP routes/delivery
// layer call. The buffer mirrors the Python daemon.event_log.EventLog: a bounded
// (last 500) deque of wire dicts shaped {"id","type","timestamp",...data},
// gap-recoverable via events_since. appendEvent writes each new event to the
// Store and mirrors it here; startup hydrates this live read window from the
// durable journal without writing the rows again.

// eventsBufferCapacity bounds the in-memory window. Matches the Python EventLog
// max_events default (last 500).
const eventsBufferCapacity = 500

// eventBuffer is a bounded FIFO of event maps with a coarse mutex. Reads return
// snapshot copies of the slice (the maps themselves are not deep-copied; callers
// treat them as read-only, exactly like the Python list(self.events)).
//
// subscribers is the SSE fan-out set (mirrors the Python asyncio.Event subscriber
// set in PeerRegistry): each live /events/stream connection registers a buffered
// signal channel here. push() does a non-blocking send to every subscriber after
// appending, so a stream that has not yet drained does not block the writer (its
// channel stays "raised" with a single pending token, exactly like a set
// asyncio.Event). The set lives on the buffer (not the Registry struct) so adding
// SSE fan-out does not edit registry.go.
type eventBuffer struct {
	mu          sync.Mutex
	events      []map[string]any
	subscribers map[chan struct{}]struct{}
}

// push appends a fully-formed event map, evicting the oldest beyond capacity,
// then wakes every live SSE subscriber with a non-blocking send.
func (b *eventBuffer) push(ev map[string]any) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	if over := len(b.events) - eventsBufferCapacity; over > 0 {
		b.events = b.events[over:]
	}
	// Snapshot subscriber channels under the lock; signal outside it. The send is
	// non-blocking: a buffered (cap 1) channel that already holds a token is
	// "already raised" and we skip — the subscriber will drain everything since
	// its cursor in one pass, so a coalesced wake loses no events.
	subs := make([]chan struct{}, 0, len(b.subscribers))
	for ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// subscribe registers a buffered (cap 1) signal channel in the fan-out set and
// returns it with an unsubscribe closure. The channel is buffered so push()
// never blocks and an event raised between subscribe and the first drain is not
// lost. Mirrors PeerRegistry.subscribe_events / unsubscribe_events.
func (b *eventBuffer) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subscribers == nil {
		b.subscribers = make(map[chan struct{}]struct{})
	}
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, ch)
			b.mu.Unlock()
		})
	}
	return ch, unsubscribe
}

// appendStructured records a lifecycle Event (the journal shape the registry
// emits) into the dashboard window using the same wire keys the Python EventLog
// produces, so a dashboard reading GET /events sees lifecycle and chat events in
// one stream. Only non-empty/non-zero fields are projected.
func (b *eventBuffer) appendStructured(e Event) {
	ev := map[string]any{
		"id":        e.EventID,
		"type":      e.Type,
		"timestamp": e.Timestamp.UTC().Format(time.RFC3339Nano),
	}
	if e.PeerID != "" {
		ev["peer_id"] = string(e.PeerID)
	}
	if e.PeerName != "" {
		ev["peer_name"] = string(e.PeerName)
	}
	if e.SessionID != "" {
		ev["session_id"] = string(e.SessionID)
	}
	for k, v := range e.Payload {
		if k == "id" || k == "type" || k == "timestamp" {
			continue
		}
		ev[k] = v
	}
	b.push(ev)
}

// AddEvent records a dashboard event of the given type, merging data into the
// wire shape {"id","type","timestamp",...data} and returning the minted event id.
// Mirrors PeerRegistry.add_event / EventLog.add_event. Callers (the chat-ingest
// and query/response routes) pass already-wire-shaped data maps.
func (r *Registry) AddEvent(ctx context.Context, eventType string, data map[string]any) string {
	id := uuid.NewString()
	event := Event{
		EventID: id, Type: eventType, Timestamp: time.Now().UTC(), Payload: data,
	}
	if value, _ := data["peer_id"].(string); value != "" {
		event.PeerID = proto.PeerID(value)
	}
	if value, _ := data["peer_name"].(string); value != "" {
		event.PeerName = proto.DisplayName(value)
	} else if value, _ := data["peer"].(string); value != "" {
		event.PeerName = proto.DisplayName(value)
	}
	if value, _ := data["session_id"].(string); value != "" {
		event.SessionID = proto.PeerID(value)
	} else if value, _ := data["repowire_session_id"].(string); value != "" {
		event.SessionID = proto.PeerID(value)
	}
	r.appendEvent(ctx, event)
	return id
}

// HydrateEvents seeds the in-memory dashboard window from SQLite without
// writing the rows again or waking subscribers during daemon startup.
func (r *Registry) HydrateEvents(events []map[string]any) {
	for _, event := range events {
		r.evlog.push(event)
	}
}

// SubscribeEvents registers an SSE subscriber: returns a buffered wake channel
// (signaled non-blocking on every AddEvent/appendEvent push) and an idempotent
// unsubscribe. Mirrors PeerRegistry.subscribe_events; the hub /events/stream
// handler does an initial flush of GetEvents(), then selects on this channel vs a
// keepalive ticker, replaying EventsSince(lastID) on each wake. The send-to-each
// happens in push() OUTSIDE r.mu, so AddEvent (which may run while r.mu is held)
// never re-enters the registry lock.
func (r *Registry) SubscribeEvents() (<-chan struct{}, func()) {
	return r.evlog.subscribe()
}

// GetEvents returns a snapshot of the full buffered window (last 500), oldest
// first. Mirrors PeerRegistry.get_events.
func (r *Registry) GetEvents() []map[string]any {
	b := r.evlog
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]map[string]any, len(b.events))
	copy(out, b.events)
	return out
}

// EventsSince returns events after the given id. If the id is empty or has been
// evicted from the buffer, it returns the full window (gap-recovery fallback).
// Mirrors PeerRegistry.events_since / EventLog.events_since.
func (r *Registry) EventsSince(eventID string) []map[string]any {
	b := r.evlog
	b.mu.Lock()
	defer b.mu.Unlock()
	if eventID == "" {
		out := make([]map[string]any, len(b.events))
		copy(out, b.events)
		return out
	}
	for i, ev := range b.events {
		if id, _ := ev["id"].(string); id == eventID {
			tail := b.events[i+1:]
			out := make([]map[string]any, len(tail))
			copy(out, tail)
			return out
		}
	}
	// Evicted id → gap-recovery: return everything we still hold.
	out := make([]map[string]any, len(b.events))
	copy(out, b.events)
	return out
}

// SubscribeEvents is defined above (the registry-access-seams area landed an
// identical declaration). Not redeclared here.

// GetPeerByPane lives in registry.go (pre-existing). Not redeclared here.

// GetAllPeers returns every registered peer (live snapshot). Mirrors
// PeerRegistry.get_all_peers.
func (r *Registry) GetAllPeers() []*proto.Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*proto.Peer, 0, len(r.peers))
	for _, ps := range r.peers {
		r.applyDescriptionTTLLocked(ps.peer)
		out = append(out, clonePeer(ps.peer))
	}
	return out
}

// ResolveByIdentifier resolves an addressing string that may be a canonical
// PeerID or a DisplayName. PeerID is tried first (exact identity match); falling
// back to DisplayName returns the most-recently-seen match so a stale offline
// ghost never shadows a live peer holding the reclaimed name. Returns (nil,false)
// when nothing matches; an ambiguous DisplayName is NOT an error here (chat
// ingest only needs a best-effort peer to scope the event), matching the
// best-effort get_peer the Python chat route uses.
func (r *Registry) ResolveByIdentifier(identifier string) (*proto.Peer, bool) {
	if identifier == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ps, ok := r.peers[proto.PeerID(identifier)]; ok {
		return clonePeer(ps.peer), true
	}
	var best *proto.Peer
	for _, ps := range r.peers {
		if string(ps.peer.DisplayName) != identifier {
			continue
		}
		if best == nil {
			best = ps.peer
			continue
		}
		if lastSeenAfter(ps.peer, best) {
			best = ps.peer
		}
	}
	// Clone at the public boundary (off-lock route reader).
	return clonePeer(best), best != nil
}

// lastSeenAfter reports whether a was seen more recently than b (nil last_seen
// sorts oldest).
func lastSeenAfter(a, b *proto.Peer) bool {
	if a.LastSeen == nil {
		return false
	}
	if b.LastSeen == nil {
		return true
	}
	return a.LastSeen.After(*b.LastSeen)
}
