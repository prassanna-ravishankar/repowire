package service

import (
	"context"

	"github.com/repowire/repowire/daemon-go/proto"
)

// acpContainer is the ACP plug of the Container socket (rfcs/agent-container.md):
// a peer whose metadata.acp names a command runs as a daemon child that the
// ACP manager starts on its first prompt. It owns those peers outright, so
// kill_peer stops them by peer. Such a child does not survive a daemon
// restart: the next prompt starts a fresh one.
type acpContainer struct {
	drop func(proto.PeerID)
}

// NewACPContainer returns the ACP plug; drop ends a peer's ACP child.
func NewACPContainer(drop func(proto.PeerID)) PeerContainer { return acpContainer{drop: drop} }

func (acpContainer) Kind() string { return "acp" }

func (acpContainer) Owns(p *proto.Peer) bool {
	if p == nil || p.Metadata == nil {
		return false
	}
	spec, ok := p.Metadata["acp"].(map[string]any)
	return ok && spec["command"] != nil
}

// StopPeer ends the peer's ACP child (its stdio closed, the process killed).
// A peer with no running child is already stopped.
func (c acpContainer) StopPeer(_ context.Context, p *proto.Peer) error {
	c.drop(p.PeerID)
	return nil
}
