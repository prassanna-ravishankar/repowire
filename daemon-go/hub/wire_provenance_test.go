package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// The daemon's own ask, delegate and reassign paths record each party on the
// stage that names it, so a thread they create is not protected for missing
// provenance, and a peer that chooses private protects its threads.
func TestAskPathsRecordProvenance(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma, delta := alphaPeer(), betaPeer(), gammaPeer(), deltaPeer()
	r := newDelegateRig(t, alpha, beta, gamma, delta)
	r.h.deliveryTraces = r.store
	w := service.NewWire(r.asks, r.store, r.reg, r.store, config.Defaults().Wire.Stuck)
	r.h.WithWire(w)
	thread := func(cid string) service.WireThread {
		t.Helper()
		got, ok, err := w.Thread(ctx, cid)
		if err != nil || !ok {
			t.Fatalf("thread %s: %v %v", cid, ok, err)
		}
		return got
	}
	parties := func(th service.WireThread) []proto.PeerID {
		var out []proto.PeerID
		for _, p := range th.Provenance {
			if p.Circle != "default" || p.Role != proto.RoleAgent || p.Private || p.Unknown {
				t.Fatalf("recorded %+v", p)
			}
			out = append(out, p.PeerID)
		}
		return out
	}
	same := func(got []proto.PeerID, want ...*proto.Peer) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i].PeerID {
				return false
			}
		}
		return true
	}

	cid := r.ask(t, alpha, beta, "who owns the release?")
	if th := thread(cid); th.Protected || !same(parties(th), alpha, beta) {
		t.Fatalf("created: protected %q, provenance %+v", th.ProtectedBy, th.Provenance)
	}
	if status, out := r.delegate(t, cid, beta, gamma, "yours"); status != http.StatusOK {
		t.Fatalf("delegate: %d %v", status, out)
	}
	if th := thread(cid); th.Protected || !same(parties(th), alpha, beta, beta, gamma) {
		t.Fatalf("delegated: protected %q, provenance %+v", th.ProtectedBy, th.Provenance)
	}
	if _, err := r.h.reassignAsk(ctx, operator, cid, "delta", "release owner"); err != nil {
		t.Fatal(err)
	}
	if th := thread(cid); th.Protected || !same(parties(th), alpha, beta, beta, gamma, delta) {
		t.Fatalf("reassigned: protected %q, provenance %+v", th.ProtectedBy, th.Provenance)
	}

	if _, err := r.h.setWireVisibility(ctx, "gamma", "hidden"); err == nil {
		t.Fatal("an unknown visibility was accepted")
	}
	if _, err := r.h.setWireVisibility(ctx, "gamma", service.WirePrivate); err != nil {
		t.Fatal(err)
	}
	if th := thread(cid); th.ProtectedBy != service.WirePrivate {
		t.Fatalf("a private former holder: protected_by %q", th.ProtectedBy)
	}
	// The choice is durable: a fresh view over the same store reads it.
	fresh := service.NewWire(r.asks, r.store, r.reg, r.store, config.Defaults().Wire.Stuck)
	if !fresh.Private(ctx, gamma.PeerID) {
		t.Fatal("visibility did not persist")
	}
}

// A notify records its parties on resolved_peer, where they are first known.
func TestNotifyRecordsProvenance(t *testing.T) {
	ctx := context.Background()
	alpha, beta := alphaPeer(), betaPeer()
	reg := &fakeRegistry{peers: []*proto.Peer{alpha, beta}}
	ft := &fakeTransport{connected: map[proto.PeerID]bool{beta.PeerID: true}, ackFrame: map[string]any{"status": "injected"}}
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mr := NewMessagingRoutes(service.NewPeerDelivery(reg, newRouterWithFake(ft), ft, nil, &fakeQueue{}), noopRepairer{}, store)
	mr.wire = service.NewWire(service.NewAskTracker(0), store, reg, store, config.Defaults().Wire.Stuck)
	_, id, err := mr.notifyTraced(ctx, service.NotifyParams{FromPeer: "alpha", ToPeer: "beta", Text: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	th, ok, err := mr.wire.Thread(ctx, id)
	if err != nil || !ok || th.Protected || len(th.Provenance) != 2 || th.Provenance[0].PeerID != alpha.PeerID || th.Provenance[1].PeerID != beta.PeerID {
		t.Fatalf("notify thread: %v %v %+v", ok, err, th)
	}
}

// A peer sets its own visibility over MCP. The operator route takes only the
// daemon token.
func TestWireVisibilitySurfaces(t *testing.T) {
	alpha, beta := alphaPeer(), betaPeer()
	r := newDelegateRig(t, alpha, beta)
	r.h.WithWire(service.NewWire(r.asks, r.store, r.reg, r.store, config.Defaults().Wire.Stuck))
	r.h.authToken = "tok"
	mux := http.NewServeMux()
	r.h.registerWireRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	post := func(token, body string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/peers/beta/wire-visibility", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := post("", `{"mode":"private"}`); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status := post("tok", `{"mode":"hidden"}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("bad mode: %d", status)
	}
	if status := post("tok", `{"mode":"private"}`); status != http.StatusOK || !r.h.wire.Private(context.Background(), beta.PeerID) {
		t.Fatalf("operator set: %d", status)
	}
}
