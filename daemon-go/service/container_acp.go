package service

import (
	"context"

	"github.com/repowire/repowire/daemon-go/proto"
)

// acpContainer is the ACP plug of the Container socket (rfcs/agent-container.md):
// a headless peer runs as a daemon child that the ACP manager starts on its
// first prompt. It owns those peers outright, so kill_peer stops them by peer. Such a child does not survive a daemon
// restart: the next prompt starts a fresh one.
type acpContainer struct {
	drop func(proto.PeerID)
}

// NewACPContainer returns the ACP plug; drop ends a peer's ACP child.
func NewACPContainer(drop func(proto.PeerID)) PeerContainer { return acpContainer{drop: drop} }

func (acpContainer) Kind() string { return "acp" }

// Owns claims only the peers the headless plug created (metadata.headless
// with a metadata.acp command): an ACP route set by some other registrant
// is not proof that the daemon owns that runtime.
func (acpContainer) Owns(p *proto.Peer) bool {
	if p == nil || p.Metadata == nil || p.Metadata["headless"] != true {
		return false
	}
	spec, ok := p.Metadata["acp"].(map[string]any)
	command, _ := spec["command"].(string)
	return ok && command != ""
}

// StopPeer ends the peer's ACP child (its stdio closed, the process killed).
// A peer with no running child is already stopped.
func (c acpContainer) StopPeer(_ context.Context, p *proto.Peer) error {
	c.drop(p.PeerID)
	return nil
}
