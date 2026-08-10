// Package peer owns the in-memory peer identity/lifecycle state and DEFINES
// the persistence seam it depends on (the Store interface). Dependency
// inversion: peer DEFINES this, state IMPLEMENTS it, main WIRES it. peer must
// NEVER import state.
package peer

import (
	"context"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// Store is the persistence seam the Registry needs. All identity arguments use
// proto.PeerID; the compiler forbids passing a DisplayName. The implementation
// (package state) reads the existing schema-v12 SQLite db unchanged.
type Store interface {
	// LoadMappings returns every persisted peer_session_mappings row, so the
	// Registry can hydrate its in-memory mappings on startup.
	LoadMappings(ctx context.Context) ([]*proto.SessionMapping, error)

	// UpsertMapping persists one mapping (INSERT OR REPLACE into
	// peer_session_mappings). Called by lazy_repair, not on every mutation.
	UpsertMapping(ctx context.Context, m *proto.SessionMapping) error

	// DeleteMapping removes a mapping when a peer is reaped/evicted.
	DeleteMapping(ctx context.Context, id proto.PeerID) error

	// LoadRetired returns retired peer_ids whose retired_at is newer than
	// `cutoff` (TTL filter applied by the caller's clock).
	LoadRetired(ctx context.Context, cutoff time.Time) (map[proto.PeerID]time.Time, error)

	// Retire records a terminal peer_id (INSERT OR REPLACE retired_peers) so an
	// orphan ws-hook cannot resurrect it without live-agent proof.
	Retire(ctx context.Context, id proto.PeerID, at time.Time) error

	// Unretire clears a retirement when a reconnect proves a live agent_pid.
	Unretire(ctx context.Context, id proto.PeerID) error

	// AppendEvent writes one immutable journal row to the events table.
	AppendEvent(ctx context.Context, e Event) error
}

// Event is the journal payload the Registry emits (lifecycle transitions,
// contradictions). The state package maps these onto the events table columns.
type Event struct {
	EventID   string            // UUID; state generates if empty
	Type      string            // e.g. "peer_online", "peer_offline", "peer_contradiction"
	Timestamp time.Time         // UTC
	PeerID    proto.PeerID      // "" allowed
	PeerName  proto.DisplayName //
	SessionID proto.PeerID      // == PeerID for peer events
	Payload   map[string]any    // serialized to payload_json
}
