package hub

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// POST /ask to a non-addressable peer is a 409 peer_not_addressable, decided
// after authorization and BEFORE AskTracker.Register: no open ask, no
// delivery attempt, and the body names the peer so the caller can reroute.
func TestPostAskRefusesNonAddressableBeforeRegistration(t *testing.T) {
	from := peerWith("repow-1-aaaa", "alpha", "1", proto.StatusOnline)
	child := peerWith("repow-1-bbbb", "app-2-codex", "1", proto.StatusOnline)
	child.Provenance = proto.Provenance{Source: proto.SourceCodexAppServer, Initiator: proto.InitiatorAgent, ParentRuntimeID: "thread-parent", Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	reg := newAskFakeRegistry(from, child)
	f := &fakeTransport{ackFrame: map[string]any{"status": "injected"}}
	srv, asks := newAskTestHub(t, reg, f)

	resp := postJSON(t, srv.URL+"/ask", AskRequest{FromPeer: "alpha", ToPeer: "app-2-codex", Text: "do it"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	detail, _ := body["detail"].(map[string]any)
	if detail == nil {
		detail = body
	}
	if detail["error"] != "peer_not_addressable" || detail["peer_id"] != "repow-1-bbbb" || detail["reason"] != "subagent_direct_input_denied" {
		t.Fatalf("body = %v", body)
	}
	if _, present := detail["parent_peer_id"]; !present {
		t.Fatalf("body lacks parent_peer_id: %v", body)
	}
	if asks.OpenCount() != 0 || f.lastTarget != "" {
		t.Fatalf("refused ask registered or delivered: open=%d target=%s", asks.OpenCount(), f.lastTarget)
	}
}
