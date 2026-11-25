package hub

import (
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// #443: the experimental channel connects with no pid, session or
// certificate, so a reconnect claims the peer_id it was assigned. Two channel
// sessions in one folder keep their own identities when the second one
// reconnects first; without the claim the daemon can only match the folder's
// name tuple, and the first to reconnect took the first identity.
func TestChannelReconnectClaimsItsOwnIdentity(t *testing.T) {
	s := newInboxStack(t)
	frame := func(claim proto.PeerID) map[string]any {
		f := map[string]any{"display_name": "channel", "circle": "1", "backend": "claude-code", "path": "/work/app"}
		if claim != "" {
			f["peer_id"] = string(claim)
		}
		return f
	}
	a := s.connect(t, frame(""), nil)
	b := s.connect(t, frame(""), nil)
	if a.id == b.id {
		t.Fatal("two channel sessions share an identity")
	}
	_ = a.conn.CloseNow()
	_ = b.conn.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pa, _ := s.h.reg.GetPeer(a.id)
		pb, _ := s.h.reg.GetPeer(b.id)
		if pa.Status == proto.StatusOffline && pb.Status == proto.StatusOffline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dropped sessions never went offline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := s.connect(t, frame(b.id), nil).id; got != b.id {
		t.Fatalf("b reconnected as %s, want %s", got, b.id)
	}
	if got := s.connect(t, frame(a.id), nil).id; got != a.id {
		t.Fatalf("a reconnected as %s, want %s", got, a.id)
	}
}
