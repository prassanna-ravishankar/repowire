package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// sessionFakeRegistry satisfies sessionRegistry over an in-memory peer slice,
// recording the by-id status/turn_state mutations and the by-name model/metadata
// patches so the handler tests can assert what was applied.
type sessionFakeRegistry struct {
	peers      []*proto.Peer
	byPane     map[string]*proto.Peer
	ambiguous  map[string]error // name -> forced ambiguous-resolution error
	statusSet  map[proto.PeerID]proto.PeerStatus
	turnSet    map[proto.PeerID]proto.TurnState
	modelSet   map[string]string
	metaSet    map[string]map[string]any
	modelFound bool
	metaFound  bool
}

func newSessionFakeRegistry(peers ...*proto.Peer) *sessionFakeRegistry {
	return &sessionFakeRegistry{
		peers:      peers,
		byPane:     map[string]*proto.Peer{},
		ambiguous:  map[string]error{},
		statusSet:  map[proto.PeerID]proto.PeerStatus{},
		turnSet:    map[proto.PeerID]proto.TurnState{},
		modelSet:   map[string]string{},
		metaSet:    map[string]map[string]any{},
		modelFound: true,
		metaFound:  true,
	}
}

func (r *sessionFakeRegistry) lookup(name string) *proto.Peer {
	for _, p := range r.peers {
		if string(p.DisplayName) == name || string(p.PeerID) == name {
			return p
		}
	}
	return nil
}

func (r *sessionFakeRegistry) GetPeerByPane(pane string) (*proto.Peer, bool) {
	p, ok := r.byPane[pane]
	return p, ok
}

func (r *sessionFakeRegistry) GetPeerByName(name string, circle *string) (*proto.Peer, error) {
	if err, ok := r.ambiguous[name]; ok {
		return nil, err
	}
	if p := r.lookup(name); p != nil {
		return p, nil
	}
	return nil, nil
}

func (r *sessionFakeRegistry) UpdateStatus(ctx context.Context, id proto.PeerID, status proto.PeerStatus) error {
	r.statusSet[id] = status
	return nil
}

func (r *sessionFakeRegistry) UpdateTurnState(ctx context.Context, id proto.PeerID, ts proto.TurnState) {
	r.turnSet[id] = ts
}

func (r *sessionFakeRegistry) UpdateModelByName(ctx context.Context, identifier, model string) (bool, error) {
	r.modelSet[identifier] = model
	return r.modelFound, nil
}

func (r *sessionFakeRegistry) UpdateMetadataByName(ctx context.Context, identifier string, metadata map[string]any) (bool, error) {
	r.metaSet[identifier] = metadata
	return r.metaFound, nil
}

// fakeDrainStore satisfies queuedDrainStore: returns canned rows and records that
// it was drained for the resolved peer_id (delete-on-drain is the store's job).
type fakeDrainStore struct {
	rows      []state.QueuedDelivery
	drainedID string
	drainErr  error
}

func (s *fakeDrainStore) DrainDeliveries(ctx context.Context, peerID string, maxResults int, now time.Time) ([]state.QueuedDelivery, error) {
	s.drainedID = peerID
	if s.drainErr != nil {
		return nil, s.drainErr
	}
	return s.rows, nil
}

// newSessionTestHub builds a hub with the session route group wired over fakes
// plus the httptest server serving its mux.
func newSessionTestHub(t *testing.T, reg sessionRegistry, _ any, store queuedDrainStore) *httptest.Server {
	t.Helper()
	h := &Hub{authToken: ""}
	h.WithSessionRoutes(reg, store)
	mux := http.NewServeMux()
	h.registerSessionRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionUpdateStatusByName is the primary path: POST /session/update with a
// status resolves the peer by name and applies the status by its peer_id through
// the FSM seam, returning {ok:true}.
func TestSessionUpdateStatusByName(t *testing.T) {
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := newSessionFakeRegistry(beta)
	srv := newSessionTestHub(t, reg, nil, nil)

	busy := "busy"
	resp := postJSON(t, srv.URL+"/session/update", SessionUpdateRequest{
		PeerName: strPtr("beta"), Status: &busy,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := reg.statusSet[beta.PeerID]; got != proto.StatusBusy {
		t.Fatalf("status must apply by peer_id; got %q for %s", got, beta.PeerID)
	}
}

// TestSessionUpdateRequiresAField rejects a body with none of status/turn_state/
// model (400), matching messages.py update_session.
func TestSessionUpdateRequiresAField(t *testing.T) {
	reg := newSessionFakeRegistry(peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline))
	srv := newSessionTestHub(t, reg, nil, nil)

	resp := postJSON(t, srv.URL+"/session/update", SessionUpdateRequest{PeerName: strPtr("beta")})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty update, got %d", resp.StatusCode)
	}
}

// TestSessionUpdateInvalidStatus rejects a status outside online|busy|offline
// (400) before any resolution.
func TestSessionUpdateInvalidStatus(t *testing.T) {
	reg := newSessionFakeRegistry(peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline))
	srv := newSessionTestHub(t, reg, nil, nil)

	bad := "sleepy"
	resp := postJSON(t, srv.URL+"/session/update", SessionUpdateRequest{PeerName: strPtr("beta"), Status: &bad})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid status, got %d", resp.StatusCode)
	}
}

// TestSessionUpdateUnknownPane 404s when no peer owns the supplied pane.
func TestSessionUpdateUnknownPane(t *testing.T) {
	reg := newSessionFakeRegistry()
	srv := newSessionTestHub(t, reg, nil, nil)

	online := "online"
	resp := postJSON(t, srv.URL+"/session/update", SessionUpdateRequest{PaneID: strPtr("%99"), Status: &online})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown pane, got %d", resp.StatusCode)
	}
}

// TestDeliveriesPendingDrains: GET /deliveries/pending?peer_id=... resolves the
// peer and maps drained rows to the asks.py wire shape (from_peer/to_peer names,
// kind, attachments defaulted to []).
func TestDeliveriesPendingDrains(t *testing.T) {
	beta := peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline)
	reg := newSessionFakeRegistry(beta)
	store := &fakeDrainStore{rows: []state.QueuedDelivery{{
		DeliveryID:   "qd-1",
		PeerID:       string(beta.PeerID),
		Kind:         state.DeliveryNotify,
		FromPeerName: "alpha",
		ToPeerName:   "beta",
		Text:         "queued hi",
		CreatedAt:    "2026-01-01T00:00:00.000000+00:00",
		ExpiresAt:    "2026-01-02T00:00:00.000000+00:00",
	}}}
	srv := newSessionTestHub(t, reg, nil, store)

	resp, err := http.Get(srv.URL + "/deliveries/pending?peer_id=repow-default-bbbb")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if store.drainedID != string(beta.PeerID) {
		t.Fatalf("drain must key on resolved peer_id, got %q", store.drainedID)
	}
	var out PendingDeliveriesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(out.Deliveries))
	}
	d := out.Deliveries[0]
	if d.DeliveryID != "qd-1" || d.Kind != "notify" || d.FromPeer != "alpha" || d.ToPeer != "beta" || d.Text != "queued hi" {
		t.Fatalf("delivery shape wrong: %+v", d)
	}
	if d.Attachments == nil {
		t.Fatalf("attachments must default to [], got nil")
	}
}

// TestDeliveriesPendingBothArgs400 rejects passing both pane_id and peer_id.
func TestDeliveriesPendingBothArgs400(t *testing.T) {
	reg := newSessionFakeRegistry()
	srv := newSessionTestHub(t, reg, nil, &fakeDrainStore{})

	resp, err := http.Get(srv.URL + "/deliveries/pending?pane_id=%251&peer_id=x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for both args, got %d", resp.StatusCode)
	}
}

// TestDeliveriesPendingNilStoreEmpty: a nil store returns an empty list (queue
// disabled), never a 500.
func TestDeliveriesPendingNilStoreEmpty(t *testing.T) {
	reg := newSessionFakeRegistry(peerWith("repow-default-bbbb", "beta", "default", proto.StatusOnline))
	srv := newSessionTestHub(t, reg, nil, nil)

	resp, err := http.Get(srv.URL + "/deliveries/pending?peer_id=repow-default-bbbb")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out PendingDeliveriesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Deliveries) != 0 {
		t.Fatalf("expected empty list for nil store, got %d", len(out.Deliveries))
	}
}
