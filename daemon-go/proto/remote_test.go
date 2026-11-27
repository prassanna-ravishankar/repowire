package proto

import (
	"encoding/json"
	"os"
	"testing"
)

// The vectors are shared with the Worker's port (hosted/src/circles.ts), so
// both sides classify a circle the same way.
func TestSharedCircle(t *testing.T) {
	raw, err := os.ReadFile("testdata/shared_circles.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Circle string `json:"circle"`
		Shared bool   `json:"shared"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		if SharedCircle(v.Circle) != v.Shared {
			t.Errorf("SharedCircle(%q) = %v", v.Circle, !v.Shared)
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
