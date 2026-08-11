package peer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// --- test doubles ---

// memStore is an in-memory Store. It records nothing the tests assert beyond
// not erroring, so the Registry's lazy-flush calls have somewhere to go.
type memStore struct {
	mu       sync.Mutex
	mappings map[proto.PeerID]*proto.SessionMapping
	upserts  int
	retired  map[proto.PeerID]Retirement
	events   []Event
}

func newMemStore() *memStore {
	return &memStore{
		mappings: map[proto.PeerID]*proto.SessionMapping{},
		retired:  map[proto.PeerID]Retirement{},
	}
}

func (s *memStore) LoadMappings(context.Context) ([]*proto.SessionMapping, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*proto.SessionMapping, 0, len(s.mappings))
	for _, m := range s.mappings {
		copy := *m
		out = append(out, &copy)
	}
	return out, nil
}
func (s *memStore) UpsertMapping(_ context.Context, m *proto.SessionMapping) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *m
	s.mappings[m.SessionID] = &copy
	s.upserts++
	return nil
}
func (s *memStore) DeleteMapping(_ context.Context, id proto.PeerID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mappings, id)
	return nil
}
func (s *memStore) LoadRetired(_ context.Context, cutoff time.Time) (map[proto.PeerID]Retirement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[proto.PeerID]Retirement{}
	for id, retirement := range s.retired {
		if retirement.At.After(cutoff) {
			out[id] = retirement
		}
	}
	return out, nil
}
func (s *memStore) Retire(_ context.Context, id proto.PeerID, at time.Time, hard bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired[id] = Retirement{At: at, Hard: hard}
	return nil
}
func (s *memStore) Unretire(_ context.Context, id proto.PeerID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.retired, id)
	return nil
}
func (s *memStore) AppendEvent(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

// fakeLive answers PIDAlive from a set.
type fakeLive struct{ alive map[int]bool }

func (f fakeLive) PIDAlive(pid int) bool { return f.alive[pid] }

// fakeTransport answers IsConnected from a set; Close is a no-op.
type fakeTransport struct{ connected map[proto.PeerID]bool }

func (t fakeTransport) IsConnected(id proto.PeerID) bool { return t.connected[id] }
func (t fakeTransport) Close(proto.PeerID) error         { return nil }
func (t fakeTransport) Ping(context.Context, proto.PeerID, time.Duration) (map[string]any, error) {
	return nil, nil
}

// TestAllocate_StaleClaimIgnored proves a claimed peer_id is reused ONLY when it
// still describes the same identity (same backend + compatible path). A claim
// from a different backend or a conflicting path must mint a fresh id, not
// hijack the live peer — the stale-pane/cert misbind guard (parity with the
// Python PeerRegistry stale-claim checks).
func TestAllocate_StaleClaimIgnored(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	idA, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"),
		Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register A: %v", err)
	}

	// Wrong backend → must not take over idA.
	idWrongBackend, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &idA, Circle: "alpha", Backend: proto.AgentCodex,
		Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("wrong-backend claim: %v", err)
	}
	if idWrongBackend == idA {
		t.Fatalf("stale backend claim hijacked %s — must mint a fresh id", idA)
	}

	// Conflicting path (same backend) → also must not take over.
	idWrongPath, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &idA, Circle: "alpha", Backend: proto.AgentClaudeCode,
		Path: ptr("/work/OTHER"), Machine: "m", Role: proto.RoleAgent,
	})
	if idWrongPath == idA {
		t.Fatalf("stale path claim hijacked %s — must mint a fresh id", idA)
	}

	// Matching identity (same backend + same path) → legitimate reconnect reuse.
	idMatch, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &idA, Circle: "alpha", Backend: proto.AgentClaudeCode,
		Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("matching claim: %v", err)
	}
	if idMatch != idA {
		t.Fatalf("matching claim minted %s, want reuse of %s", idMatch, idA)
	}
}

// TestReconnect_PreservesDurableState proves a same-id reconnect updates
// liveness without demoting role, moving circle, or dropping metadata — a
// reconnect frame that omits role/circle must not silently downgrade an
// orchestrator to an agent (parity with PeerRegistry reconnect).
func TestReconnect_PreservesDurableState(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	idA, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "ops", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m",
		Role:     proto.RoleOrchestrator,
		Metadata: map[string]any{"project": "repowire", "branch": "main"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Reconnect: same id, role/circle omitted, only a fresh metadata key.
	idB, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &idA, Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m",
		Metadata: map[string]any{"caps": "receipts"},
	})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if idB != idA {
		t.Fatalf("reconnect minted %s, want reuse of %s", idB, idA)
	}
	p, ok := r.GetPeer(idA)
	if !ok {
		t.Fatal("peer gone after reconnect")
	}
	if p.Role != proto.RoleOrchestrator {
		t.Fatalf("role demoted to %q on reconnect, want orchestrator", p.Role)
	}
	if p.Circle != "ops" {
		t.Fatalf("circle changed to %q on reconnect, want ops", p.Circle)
	}
	if p.Metadata["project"] != "repowire" || p.Metadata["caps"] != "receipts" {
		t.Fatalf("metadata not merged on reconnect: %v", p.Metadata)
	}
}

func TestSanitizeFolderName(t *testing.T) {
	for in, want := range map[string]string{
		"myrepo":   "myrepo",
		"my repo":  "my-repo",
		"a@#b":     "a-b",
		"--lead--": "lead",
		"":         "peer",
		"ok._-n":   "ok._-n",
	} {
		if got := sanitizeFolder(in); got != want {
			t.Errorf("sanitizeFolder(%q) = %q, want %q", in, got, want)
		}
	}
	if got := baseFolder("/work/my repo/"); got != "my-repo" {
		t.Errorf("baseFolder with spaces = %q, want my-repo", got)
	}
}

// TestReconnect_ResetsStaleBusy proves a re-register of a BUSY peer resets it to
// ONLINE (parity with PeerRegistry applying initial_status on reconnect); a genuine
// in-progress turn re-reports BUSY via the next UserPromptSubmit.
func TestReconnect_ResetsStaleBusy(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "x", Backend: proto.AgentClaudeCode, Path: ptr("/w"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.UpdateStatus(ctx, id, proto.StatusBusy); err != nil {
		t.Fatalf("set busy: %v", err)
	}
	if p, _ := r.GetPeer(id); p.Status != proto.StatusBusy {
		t.Fatalf("setup: want busy, got %q", p.Status)
	}
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		ClaimedPeerID: &id, Circle: "x", Backend: proto.AgentClaudeCode, Path: ptr("/w"), Machine: "m", Role: proto.RoleAgent,
	}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if p, _ := r.GetPeer(id); p.Status != proto.StatusOnline {
		t.Fatalf("stale BUSY survived reconnect: status %q, want online", p.Status)
	}
}

// TestReclaim_MappingAdoptedByIdentity proves that after a "restart" (mapping
// persists, in-memory peer gone) a re-registration WITHOUT an explicit peer_id
// adopts the durable mapping by identity (display_name/circle/backend) and
// restores role — instead of minting a fresh id and demoting the orchestrator.
func TestReclaim_MappingAdoptedByIdentity(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	id, name, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "ops", Backend: proto.AgentClaudeCode, Path: ptr("/w/x"), Machine: "m",
		Role: proto.RoleOrchestrator,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Simulate a daemon restart: the durable mapping survives, the in-memory peer
	// does not.
	r.mu.Lock()
	delete(r.peers, id)
	r.mu.Unlock()

	// Re-register by identity, NO peer_id, role omitted.
	id2, name2, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "ops", Backend: proto.AgentClaudeCode, Path: ptr("/w/x"), Machine: "m",
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if id2 != id {
		t.Fatalf("minted fresh id %s, want adoption of %s", id2, id)
	}
	if name2 != name {
		t.Fatalf("display name changed on adoption: %q vs %q", name2, name)
	}
	if p, _ := r.GetPeer(id); p.Role != proto.RoleOrchestrator {
		t.Fatalf("role demoted to %q on mapping adoption, want orchestrator", p.Role)
	}
}

// TestReclaim_DoesNotCrossAdoptMappings proves registration never silently
// restores a peer to a mapping in another circle.
func TestReclaim_DoesNotCrossAdoptMappings(t *testing.T) {
	ctx := context.Background()

	seed := func() (*Registry, *memStore, proto.PeerID) {
		r, s := newRegistry(t)
		id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
			Circle: "ops", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"),
			Machine: "m", Role: proto.RoleOrchestrator,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		delete(r.peers, id) // simulate restart: mapping survives, peer gone
		return r, s, id
	}

	// A default-circle registration must not adopt the old ops mapping.
	r, _, seedID := seed()
	got, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "default", Backend: proto.AgentClaudeCode,
		Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got == seedID {
		t.Fatalf("cross-adopted mapping %s — must mint fresh", seedID)
	}
}

func newRegistry(t *testing.T) (*Registry, *memStore) {
	t.Helper()
	store := newMemStore()
	r, err := NewRegistry(context.Background(), store,
		fakeLive{alive: map[int]bool{}},
		fakeTransport{connected: map[proto.PeerID]bool{}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, store
}

// --- (a) legal vs illegal transition ---

func TestApply_LegalVsIllegal(t *testing.T) {
	// Legal: Online --UserPromptSubmit--> Busy.
	got, err := Apply(StateOnline, EventUserPromptSubmit)
	if err != nil {
		t.Fatalf("legal transition errored: %v", err)
	}
	if got != StateBusy {
		t.Fatalf("Online+UserPromptSubmit = %q, want Busy", got)
	}

	// Illegal: a Retired peer cannot receive a Stop hook. The FSM is the single
	// authority that rejects this; there is no way to express the bad move as a
	// direct status assignment at this layer.
	got, err = Apply(StateRetired, EventStop)
	if err != ErrIllegalTransition {
		t.Fatalf("illegal transition: err = %v, want ErrIllegalTransition", err)
	}
	if got != StateRetired {
		t.Fatalf("illegal transition mutated state to %q, want unchanged Retired", got)
	}

	// The one legal way out of Retired is a live-agent reclaim.
	if got, err := Apply(StateRetired, EventReclaimWithLiveAgent); err != nil || got != StateOnline {
		t.Fatalf("Retired+ReclaimWithLiveAgent = (%q,%v), want (Online,nil)", got, err)
	}
}

// --- (b) routing by PeerID works; DisplayName collision does NOT misroute ---

func TestRouting_PeerIDNotDisplayName(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)

	// Two peers in DIFFERENT circles that resolve to the SAME display_name
	// (folder "shared" + same backend). Identity is the PeerID; addressing is
	// the colliding DisplayName.
	idA, nameA, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m",
		Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	idB, nameB, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "beta", Backend: proto.AgentClaudeCode, Path: ptr("/work/shared"), Machine: "m",
		Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register B: %v", err)
	}

	if nameA != nameB {
		t.Fatalf("test precondition: expected colliding display names, got %q and %q", nameA, nameB)
	}
	if idA == idB {
		t.Fatalf("PeerIDs must be distinct even when display names collide: %q == %q", idA, idB)
	}

	// Routing by PeerID is exact: each id resolves to its own peer, in its own
	// circle, despite the shared name.
	pa, ok := r.GetPeer(idA)
	if !ok || pa.PeerID != idA || pa.Circle != "alpha" {
		t.Fatalf("GetPeer(idA) = %+v ok=%v, want PeerID=%q circle=alpha", pa, ok, idA)
	}
	pb, ok := r.GetPeer(idB)
	if !ok || pb.PeerID != idB || pb.Circle != "beta" {
		t.Fatalf("GetPeer(idB) = %+v ok=%v, want PeerID=%q circle=beta", pb, ok, idB)
	}

	// Drive a status change on B only, by PeerID. A must be untouched — a
	// display-name-keyed router would have misrouted to both.
	if err := r.UpdateStatus(ctx, idB, proto.StatusBusy); err != nil {
		t.Fatalf("UpdateStatus(idB): %v", err)
	}
	if pa, _ := r.GetPeer(idA); pa.Status != proto.StatusOnline {
		t.Fatalf("peer A status leaked to %q via display-name collision; want online", pa.Status)
	}
	if pb, _ := r.GetPeer(idB); pb.Status != proto.StatusBusy {
		t.Fatalf("peer B status = %q, want busy", pb.Status)
	}

	// The compiler is the other half of the guarantee: GetPeer takes a
	// proto.PeerID; passing the DisplayName below would not compile.
	//   _, _ = r.GetPeer(nameA) // proto.DisplayName is not a proto.PeerID
	_ = nameA
}

func TestMarkOffline_Terminal_Retires(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)

	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := r.MarkOffline(ctx, id, true); err != nil {
		t.Fatalf("MarkOffline terminal: %v", err)
	}

	store.mu.Lock()
	_, retired := store.retired[id]
	store.mu.Unlock()
	if !retired {
		t.Fatalf("terminal offline did not persist retirement for %q", id)
	}

	// A reconnect claiming the retired id without a live agent is rejected.
	claimed := id
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m",
		Role: proto.RoleAgent, ClaimedPeerID: &claimed,
	}); err != ErrPeerRetired {
		t.Fatalf("retired reclaim without live agent: err = %v, want ErrPeerRetired", err)
	}

	// With a proven-live agent, the reclaim succeeds (retirement cleared).
	r.live = fakeLive{alive: map[int]bool{4242: true}}
	pid := 4242
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Path: ptr("/work/x"), Machine: "m",
		Role: proto.RoleAgent, ClaimedPeerID: &claimed, AgentPID: &pid,
	}); err != nil {
		t.Fatalf("retired reclaim with live agent: %v", err)
	}
}

func TestClosePeer_HardRetirementRejectsLiveAgentReclaim(t *testing.T) {
	ctx := context.Background()
	r, store := newRegistry(t)
	pid := 4242
	r.live = fakeLive{alive: map[int]bool{pid: true}}
	id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentCodex, Path: ptr("/work/x"), Machine: "m",
		Role: proto.RoleAgent, AgentPID: &pid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClosePeer(ctx, id, "operator_close"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	retirement := store.retired[id]
	store.mu.Unlock()
	if !retirement.Hard {
		t.Fatal("operator close did not persist a hard retirement")
	}
	claimed := id
	if _, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "alpha", Backend: proto.AgentCodex, Path: ptr("/work/x"), Machine: "m",
		Role: proto.RoleAgent, ClaimedPeerID: &claimed, AgentPID: &pid,
	}); err != ErrPeerRetired {
		t.Fatalf("hard-retired live reclaim err = %v, want ErrPeerRetired", err)
	}
}

func ptr[T any](v T) *T { return &v }
