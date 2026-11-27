package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/proto"
)

// A share names its peer to the relay by peer_id as well as by its current
// name, so the link follows the peer through a native session rename (#652).
// A name the registry does not know still goes through as given.
func TestShareSendsThePeerIDToTheRelay(t *testing.T) {
	var got map[string]any
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"share_id":"sh_1"}`))
	}))
	defer relay.Close()
	h := newTestHub(t).WithShares(&RelayConfig{Enabled: true, URL: relay.URL, APIKey: "rw_test"})
	path := "/tmp/worker"
	id := registerTestPeer(t, h, peer.AllocateParams{Circle: "default", Backend: proto.AgentClaudeCode, Path: &path, Role: proto.RoleAgent})
	p, _ := h.reg.GetPeer(id)

	if _, err := h.createShareDirect(context.Background(), shareRequest{PeerName: string(p.DisplayName)}); err != nil {
		t.Fatal(err)
	}
	if got["peer_id"] != string(id) || got["peer_name"] != string(p.DisplayName) {
		t.Fatalf("relay got %v, want peer_id %s and name %s", got, id, p.DisplayName)
	}
	if _, err := h.createShareDirect(context.Background(), shareRequest{PeerName: "nobody-here"}); err != nil {
		t.Fatal(err)
	}
	if _, has := got["peer_id"]; has || got["peer_name"] != "nobody-here" {
		t.Fatalf("unknown name: relay got %v, want the name alone", got)
	}
}
