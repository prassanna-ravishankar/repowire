package proto

import "testing"

func TestSharedCircle(t *testing.T) {
	for circle, shared := range map[string]bool{
		"repowire": true, "proj-x": true, "window": true, "window-a": true,
		"": false, "*": false, "default": false, "0": false, "17": false, "window-12": false,
	} {
		if SharedCircle(circle) != shared {
			t.Errorf("SharedCircle(%q) = %v", circle, !shared)
		}
	}
}

func TestRemotePeerIDRoundTrip(t *testing.T) {
	id := RemotePeerID("laptop", "repow-1-abc")
	machine, peer, ok := ParseRemotePeerID(id)
	if !ok || machine != "laptop" || peer != "repow-1-abc" || !IsRemotePeerID(id) {
		t.Fatalf("%s -> %q %q %v", id, machine, peer, ok)
	}
	for _, bad := range []PeerID{"repow-1-abc", "remote:", "remote:laptop", "remote::x", "remote:laptop:"} {
		if _, _, ok := ParseRemotePeerID(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}
