package peer

import (
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// DescriptionSetAt reports when a peer last changed its description, from the
// durable mapping. nil when the peer has never set one since this column
// existed.
func (r *Registry) DescriptionSetAt(id proto.PeerID) *time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.mappings[id]
	if !ok || m.DescriptionSetAt == nil {
		return nil
	}
	t := *m.DescriptionSetAt
	return &t
}

// CircleAccess applies the delivery path's circle rule to two already
// resolved peers, so routing can label candidates exactly as CheckAccess would.
func CircleAccess(from, to *proto.Peer, bypass bool) error {
	return checkCircleAccess(from, to, bypass)
}
