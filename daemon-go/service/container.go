package service

import (
	"context"

	"github.com/repowire/repowire/daemon-go/proto"
)

// Container is the socket that owns a spawned agent's execution: where it
// runs, how it starts and stops, and the evidence that it is still there
// (rfcs/agent-container.md). The tmux plug (container_tmux.go) is the
// default and today the only one; ACP, Codex App Server threads and hosted
// agents are later plugs. A place is named by a plug-scoped id (for tmux,
// the pane id).
//
// Slice 1 extracts the socket with today's semantics unchanged: Probe still
// answers with tmux evidence, and the destructive proof stays in the routes.
type Container interface {
	// Kind names the plug ("tmux").
	Kind() string
	// Reserve creates a place without launching into it (restart reserves
	// the new place before it stops the old runtime).
	Reserve(cfg SpawnConfig) (PanePlacement, error)
	// Launch starts the agent, in cfg.Reserved when set, else in a new place.
	Launch(cfg SpawnConfig) (SpawnResult, error)
	// Stop removes the place and what runs in it; true on success. Callers
	// prove ownership first.
	Stop(placeID string) bool
	// Probe returns live evidence for a place, or nil when it is gone.
	Probe(placeID string) *TmuxPaneEvidence
}

// PeerContainer is a plug that owns some peers outright: their execution has
// no pane to prove, and the plug stops it by peer (a hosted agent is one).
// kill_peer asks each in turn; the first that Owns the peer stops it, and the
// caller then applies the operator close, as for every backend.
type PeerContainer interface {
	// Kind names the plug ("hosted").
	Kind() string
	// Owns reports whether this plug runs p's execution.
	Owns(p *proto.Peer) bool
	// StopPeer ends p's execution for good. It must not change registry
	// state: the caller closes the peer once StopPeer succeeds, and keeps it
	// registered when it fails.
	StopPeer(ctx context.Context, p *proto.Peer) error
}
