package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

// A delivery queued while a peer was addressable must not be replayed on
// reconnect after the peer was demoted, even before the asynchronous
// demotion cleanup has dropped the row: the replay path checks the live
// verdict, drops the row, and records the drop.
func TestWSReplaySkipsAndDropsQueuedDeliveriesForNonAddressablePeer(t *testing.T) {
	h := newTestHub(t)
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.WithReadDeps(service.NewAskTracker(0), store)
	ctx := context.Background()
	path := "/work/app"

	accepting := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
		Metadata: map[string]any{"runtime_session_id": "thread-child"}, Provenance: &accepting})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var registered RegisterResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &registered)
	id := proto.PeerID(registered.PeerID)

	// Queue a notify while addressable (as the busy-deferral fallback would).
	if _, err := store.EnqueueDelivery(ctx, state.QueuedDelivery{DeliveryID: "d-1", PeerID: string(id), Kind: state.DeliveryNotify, FromPeerName: "alpha", ToPeerName: "app-codex", Text: "late"}, 3600, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Demote directly in the registry: no reconciliation seams are wired in
	// this hub, so nothing drops the row asynchronously. The replay gate must
	// handle it alone.
	denied := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	if found, err := h.reg.UpdateProvenance(ctx, string(id), denied, nil); err != nil || !found {
		t.Fatalf("demote: %v %v", found, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := wsjson.Write(dialCtx, c, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: "app-codex", Circle: "c", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &path, PeerID: &id, Provenance: &denied}); err != nil {
		t.Fatal(err)
	}
	var connected map[string]any
	if err := wsjson.Read(dialCtx, c, &connected); err != nil {
		t.Fatal(err)
	}
	if connected["type"] != string(proto.FrameConnected) {
		t.Fatalf("connect reply = %v", connected)
	}
	// No replayed frame may arrive.
	readCtx, cancelRead := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelRead()
	var frame map[string]any
	if err := wsjson.Read(readCtx, c, &frame); err == nil {
		t.Fatalf("queued delivery was replayed to a non-addressable peer: %v", frame)
	}
	left, err := store.ListDeliveries(ctx, string(id), 10, time.Now().UTC())
	if err != nil || len(left) != 0 {
		t.Fatalf("queued rows after replay = %d, %v; want 0", len(left), err)
	}
	var dropped bool
	for _, e := range h.reg.GetEvents() {
		if e["type"] == "peer_not_addressable" && e["peer_id"] == string(id) && e["via"] == "ws_replay" {
			dropped = true
		}
	}
	if !dropped {
		t.Fatal("no peer_not_addressable event recorded for the dropped replay")
	}
}

// Demotion between two replay writes stops the rest of the batch: with four
// queued rows, the peer is demoted right after the first frame is written;
// exactly one frame arrives, the remaining rows are dropped, and the drop is
// recorded. The gate override makes the interleaving deterministic.
func TestWSReplayStopsMidBatchWhenPeerIsDemoted(t *testing.T) {
	h := newTestHub(t)
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h.WithReadDeps(service.NewAskTracker(0), store)
	ctx := context.Background()
	path := "/work/app"
	accepting := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: true}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "app-codex", Path: &path, Backend: proto.AgentCodex, Circle: strptr("c"),
		Metadata: map[string]any{"runtime_session_id": "thread-child"}, Provenance: &accepting})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var registered RegisterResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &registered)
	id := proto.PeerID(registered.PeerID)
	now := time.Now().UTC()
	for i := 1; i <= 4; i++ {
		if _, err := store.EnqueueDelivery(ctx, state.QueuedDelivery{DeliveryID: "d-" + string(rune('0'+i)), PeerID: string(id), Kind: state.DeliveryNotify, FromPeerName: "alpha", ToPeerName: "app-codex", Text: "m"}, 3600, 10, now.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	denied := proto.Provenance{Source: proto.SourceCodexAppServer, Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	checks := 0
	h.replayGateOverride = func(ctx context.Context, id proto.PeerID, via string) bool {
		checks++
		// Check 1: before listing. Check 2: before frame 1. Demote just before
		// check 3 (before frame 2), as a concurrent demotion would.
		if checks == 3 {
			if found, err := h.reg.UpdateProvenance(ctx, string(id), denied, nil); err != nil || !found {
				t.Errorf("demote: %v %v", found, err)
			}
		}
		return h.dropQueuedIfNotAddressable(ctx, id, via)
	}

	srv := httptest.NewServer(http.HandlerFunc(h.HandleWS))
	defer srv.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := wsjson.Write(dialCtx, c, proto.ConnectFrame{Type: proto.FrameConnect, DisplayName: "app-codex", Circle: "c", Backend: proto.AgentCodex, Role: proto.RoleAgent, Path: &path, PeerID: &id, Provenance: &accepting}); err != nil {
		t.Fatal(err)
	}
	var connected map[string]any
	if err := wsjson.Read(dialCtx, c, &connected); err != nil || connected["type"] != string(proto.FrameConnected) {
		t.Fatalf("connect reply = %v, %v", connected, err)
	}
	var received []string
	for {
		readCtx, cancelRead := context.WithTimeout(ctx, 400*time.Millisecond)
		var frame map[string]any
		err := wsjson.Read(readCtx, c, &frame)
		cancelRead()
		if err != nil {
			break
		}
		received = append(received, frame["delivery_id"].(string))
	}
	if len(received) != 1 {
		t.Fatalf("frames after mid-batch demotion = %v, want exactly the one written before the denial", received)
	}
	left, err := store.ListDeliveries(ctx, string(id), 10, time.Now().UTC())
	if err != nil || len(left) != 0 {
		t.Fatalf("queued rows after stopped replay = %d, %v; want 0", len(left), err)
	}
	var dropped int
	for _, e := range h.reg.GetEvents() {
		if e["type"] == "peer_not_addressable" && e["peer_id"] == string(id) && e["via"] == "ws_replay" {
			dropped, _ = e["deliveries_dropped"].(int)
		}
	}
	if dropped != 3 {
		t.Fatalf("dropped count in event = %d, want 3", dropped)
	}
}
