package peer

import (
	"context"
	"slices"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// fakeSessionIndex stands in for session_bindings: the hub records each
// registration's (backend, runtime session) -> peer_id after the identity
// decision, and the newest binding wins.
type fakeSessionIndex struct {
	bySession map[string][2]string // backend|session -> {peer_id, path}
	byPeer    map[proto.PeerID]string
}

func newFakeSessionIndex() *fakeSessionIndex {
	return &fakeSessionIndex{bySession: map[string][2]string{}, byPeer: map[proto.PeerID]string{}}
}

func (f *fakeSessionIndex) bind(backend proto.AgentType, session string, id proto.PeerID, path string) {
	f.bySession[string(backend)+"|"+session] = [2]string{string(id), path}
	f.byPeer[id] = session
}

func (f *fakeSessionIndex) PeerForRuntimeSession(_ context.Context, backend proto.AgentType, session string) (proto.PeerID, string, error) {
	b := f.bySession[string(backend)+"|"+session]
	return proto.PeerID(b[0]), b[1], nil
}

func (f *fakeSessionIndex) RuntimeSessionOf(_ context.Context, id proto.PeerID) (string, error) {
	return f.byPeer[id], nil
}

type sessionHarness struct {
	t     *testing.T
	r     *Registry
	store *memStore
	idx   *fakeSessionIndex
	live  fakeLive
}

func newSessionHarness(t *testing.T) *sessionHarness {
	t.Helper()
	live := fakeLive{alive: map[int]bool{}}
	r, store := newRegistryWith(t, fakeTransport{connected: map[proto.PeerID]bool{}}, live)
	idx := newFakeSessionIndex()
	r.WithSessionIndex(idx)
	return &sessionHarness{t: t, r: r, store: store, idx: idx, live: live}
}

// restart flushes mappings and rebuilds the registry from the same store and
// index, as a daemon restart does.
func (h *sessionHarness) restart() {
	h.t.Helper()
	h.r.persistMappings(context.Background())
	r, err := NewRegistry(context.Background(), h.store, h.live, fakeTransport{connected: map[proto.PeerID]bool{}})
	if err != nil {
		h.t.Fatal(err)
	}
	r.WithSessionIndex(h.idx)
	h.r = r
}

// register registers and then records the session binding, like the hub.
func (h *sessionHarness) register(backend proto.AgentType, path, session string, pid int) (proto.PeerID, proto.DisplayName) {
	h.t.Helper()
	params := AllocateParams{Circle: "work", Backend: backend, Path: ptr(path), Machine: "m", Role: proto.RoleAgent}
	if session != "" {
		key := "runtime_session_id"
		if backend == proto.AgentClaudeCode {
			key = "hook_session_id"
		}
		params.Metadata = map[string]any{key: session}
	}
	if pid > 0 {
		params.AgentPID = &pid
		h.live.alive[pid] = true
	}
	id, name, err := h.r.AllocateAndRegister(context.Background(), params)
	if err != nil {
		h.t.Fatalf("register %s %s: %v", backend, session, err)
	}
	if session != "" {
		h.idx.bind(backend, session, id, path)
	}
	return id, name
}

func (h *sessionHarness) offline(id proto.PeerID, pid int) {
	h.t.Helper()
	if _, err := h.r.MarkOffline(context.Background(), id, false); err != nil {
		h.t.Fatal(err)
	}
	delete(h.live.alive, pid)
}

// disconnect drops the record offline with its process still running (a pane
// handoff, a WebSocket drop).
func (h *sessionHarness) disconnect(id proto.PeerID) {
	h.t.Helper()
	if _, err := h.r.MarkOffline(context.Background(), id, false); err != nil {
		h.t.Fatal(err)
	}
}

func (h *sessionHarness) status(id proto.PeerID) proto.PeerStatus {
	h.t.Helper()
	p, ok := h.r.GetPeer(id)
	if !ok {
		h.t.Fatalf("peer %s missing", id)
	}
	return p.Status
}

// #490: a second Codex thread in the same workspace must not take over an
// offline sibling thread's identity, before or after a daemon restart, and the
// sibling keeps its own identity when it comes back.
func TestSessionIdentity_CodexSiblingThreadGetsItsOwnPeer(t *testing.T) {
	for _, restart := range []bool{false, true} {
		h := newSessionHarness(t)
		a, aName := h.register(proto.AgentCodex, "/p/repowire", "thread-A", 0)
		h.offline(a, 0)
		if restart {
			h.restart()
		}

		b, bName := h.register(proto.AgentCodex, "/p/repowire", "thread-B", 0)
		if b == a {
			t.Fatalf("restart=%v: thread-B reclaimed thread-A's peer %s", restart, a)
		}
		if bName == aName {
			t.Fatalf("restart=%v: thread-B shares address %s with thread-A", restart, aName)
		}

		back, backName := h.register(proto.AgentCodex, "/p/repowire", "thread-A", 0)
		if back != a || backName != aName {
			t.Fatalf("restart=%v: thread-A came back as %s/%s, want %s/%s", restart, back, backName, a, aName)
		}
		if h.status(b) != proto.StatusOnline || h.status(a) != proto.StatusOnline {
			t.Fatalf("restart=%v: both threads should be online on distinct peers", restart)
		}
	}
}

// Without a session index (or before any binding exists) the live record's
// session metadata alone refuses the cross-thread tuple reclaim.
func TestSessionIdentity_CodexSiblingRefusedWithoutIndex(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistry(t)
	register := func(thread string) proto.PeerID {
		id, _, err := r.AllocateAndRegister(ctx, AllocateParams{
			Circle: "work", Backend: proto.AgentCodex, Path: ptr("/p/repowire"), Machine: "m", Role: proto.RoleAgent,
			Metadata: map[string]any{"runtime_session_id": thread},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := register("thread-A")
	if _, err := r.MarkOffline(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	if b := register("thread-B"); b == a {
		t.Fatalf("thread-B reclaimed thread-A's peer %s", a)
	}
}

// #474: `claude --resume <sid>` by hand starts a new process with no peer_id
// hint. The session binding returns the original peer even when the name tuple
// would not (here a second offline peer makes the tuple ambiguous), and even
// when the old record still says online with a dead pid.
func TestSessionIdentity_ClaudeResumeReclaimsBoundPeer(t *testing.T) {
	h := newSessionHarness(t)
	orig, origName := h.register(proto.AgentClaudeCode, "/p/orchestrator", "sess-1", 100)
	other, _ := h.register(proto.AgentClaudeCode, "/p/orchestrator", "sess-2", 101)
	h.offline(other, 101)
	h.r.mu.Lock()
	h.r.peers[other].peer.DisplayName = origName // two identities on one tuple
	h.r.mu.Unlock()
	delete(h.live.alive, 100) // the original process exited without an offline

	got, gotName := h.register(proto.AgentClaudeCode, "/p/orchestrator", "sess-1", 200)
	if got != orig || gotName != origName {
		t.Fatalf("resume got %s/%s, want %s/%s", got, gotName, orig, origName)
	}
	if n := len(eventsOfType(h.store, "identity_reclaim_ambiguous")); n != 0 {
		t.Fatalf("session binding should decide before the tuple, got %d ambiguity events", n)
	}

	// After a daemon restart the binding still resolves it.
	h.offline(orig, 200)
	h.restart()
	if again, _ := h.register(proto.AgentClaudeCode, "/p/orchestrator", "sess-1", 300); again != orig {
		t.Fatalf("resume after restart got %s, want %s", again, orig)
	}
}

// A soft retirement (SessionEnd) is lifted by a resume of the bound session; a
// hard operator close is not.
func TestSessionIdentity_ResumeAndRetirement(t *testing.T) {
	ctx := context.Background()
	h := newSessionHarness(t)
	soft, _ := h.register(proto.AgentCodex, "/p/a", "thread-soft", 0)
	if _, err := h.r.MarkOffline(ctx, soft, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.register(proto.AgentCodex, "/p/a", "thread-soft", 0); got != soft || h.status(soft) != proto.StatusOnline {
		t.Fatalf("soft-retired bound session should resume as %s, got %s", soft, got)
	}

	hard, _ := h.register(proto.AgentCodex, "/p/b", "thread-hard", 0)
	if _, err := h.r.ClosePeer(ctx, hard, "operator_close"); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.register(proto.AgentCodex, "/p/b", "thread-hard", 0); got == hard {
		t.Fatalf("hard-retired peer %s must not be reclaimed through its session binding", hard)
	}
}

// Bindings written before this fix can point several threads at one peer. A
// thread whose binding is historical (the peer has since run another thread)
// does not reclaim it, while the peer is online on the other thread and after
// a restart.
func TestSessionIdentity_HistoricalBindingDoesNotReclaim(t *testing.T) {
	for _, restart := range []bool{false, true} {
		h := newSessionHarness(t)
		p, _ := h.register(proto.AgentCodex, "/p/repowire", "thread-A", 0)
		h.idx.bind(proto.AgentCodex, "thread-B", p, "/p/repowire") // legacy second binding, newest
		h.r.mu.Lock()
		h.r.peers[p].peer.Metadata["runtime_session_id"] = "thread-B" // online on thread-B
		h.r.mu.Unlock()
		if restart {
			h.offline(p, 0)
			h.restart()
		}

		a, _ := h.register(proto.AgentCodex, "/p/repowire", "thread-A", 0)
		if a == p {
			t.Fatalf("restart=%v: thread-A reclaimed %s through a historical binding", restart, p)
		}
		if b, _ := h.register(proto.AgentCodex, "/p/repowire", "thread-B", 0); b != p {
			t.Fatalf("restart=%v: thread-B got %s, want %s", restart, b, p)
		}
	}
}

// A fork briefly reports its parent's session id from another live process.
// It gets its own peer and address; the parent is untouched, also when the
// parent's record is offline while its process still runs.
func TestSessionIdentity_ForkGetsDistinctPeer(t *testing.T) {
	t.Run("parent online", func(t *testing.T) { testForkGetsDistinctPeer(t, false) })
	t.Run("parent offline, process alive", func(t *testing.T) { testForkGetsDistinctPeer(t, true) })
}

func testForkGetsDistinctPeer(t *testing.T, parentOffline bool) {
	h := newSessionHarness(t)
	parent, parentName := h.register(proto.AgentClaudeCode, "/p/app", "sess-1", 100)
	if parentOffline {
		h.disconnect(parent)
	}

	fork, forkName := h.register(proto.AgentClaudeCode, "/p/app", "sess-1", 200)
	if fork == parent || forkName == parentName {
		t.Fatalf("fork shares the parent's identity: %s/%s", fork, forkName)
	}
	if !parentOffline && h.status(parent) != proto.StatusOnline {
		t.Fatalf("parent displaced by its fork")
	}
	p, _ := h.r.GetPeer(parent)
	if p.AgentPID == nil || *p.AgentPID != 100 {
		t.Fatalf("parent record rewritten by its fork: %+v", p.AgentPID)
	}
}

// Several offline identities on one name tuple: adopt none, mint, journal it.
func TestSessionIdentity_AmbiguousTupleMints(t *testing.T) {
	h := newSessionHarness(t)
	a, aName := h.register(proto.AgentClaudeCode, "/p/app", "", 100)
	b, _ := h.register(proto.AgentClaudeCode, "/p/app", "", 101)
	h.offline(a, 100)
	h.offline(b, 101)
	h.r.mu.Lock()
	h.r.peers[b].peer.DisplayName = aName
	h.r.mappings[b].DisplayName = aName
	h.r.mu.Unlock()

	assertMinted := func(stage string, got proto.PeerID, gotName proto.DisplayName, wantEvents int) {
		t.Helper()
		if got == a || got == b {
			t.Fatalf("%s: ambiguous tuple adopted %s", stage, got)
		}
		if gotName == aName {
			t.Fatalf("%s: minted peer shares address %s", stage, aName)
		}
		evs := eventsOfType(h.store, "identity_reclaim_ambiguous")
		if len(evs) != wantEvents {
			t.Fatalf("%s: want %d ambiguity events, got %d", stage, wantEvents, len(evs))
		}
		cands, _ := evs[len(evs)-1].Payload["candidates"].([]string)
		want := []string{string(a), string(b)}
		slices.Sort(want)
		if evs[len(evs)-1].PeerID != got || !slices.Equal(cands, want) {
			t.Fatalf("%s: ambiguity event %+v, want peer %s candidates %v", stage, evs[len(evs)-1], got, want)
		}
	}

	got, gotName := h.register(proto.AgentClaudeCode, "/p/app", "", 300)
	assertMinted("offline", got, gotName, 1)

	// The mapping tuple after a restart follows the same rule.
	h.offline(got, 300)
	h.r.mu.Lock()
	delete(h.r.mappings, got)
	h.r.mu.Unlock()
	h.restart()
	got, gotName = h.register(proto.AgentClaudeCode, "/p/app", "", 400)
	assertMinted("mapping", got, gotName, 2)
}

// The documented same-directory Claude takeover still works: a new session in
// the folder of an offline Claude peer (new pid, new session id, no peer_id
// hint) takes over that identity, live and after a restart.
func TestSessionIdentity_SameDirectoryClaudeTakeover(t *testing.T) {
	h := newSessionHarness(t)
	old, oldName := h.register(proto.AgentClaudeCode, "/p/app", "sess-old", 100)
	h.offline(old, 100)

	got, gotName := h.register(proto.AgentClaudeCode, "/p/app", "sess-new", 200)
	if got != old || gotName != oldName {
		t.Fatalf("takeover got %s/%s, want %s/%s", got, gotName, old, oldName)
	}

	h.offline(got, 200)
	h.restart()
	if again, _ := h.register(proto.AgentClaudeCode, "/p/app", "sess-newer", 300); again != old {
		t.Fatalf("takeover after restart got %s, want %s", again, old)
	}
}

// #503: a claim for a peer_id held by a live session in another pane must not
// merge the claimant into that peer. The same pane keeps the id even when the
// pid differs (the ws-hook may report a wrapper such as caffeinate), and a dead
// holder is a takeover, not a twin.
func TestSessionIdentity_ClaimHeldByOtherLiveSessionMints(t *testing.T) {
	h := newSessionHarness(t)
	claim := func(cid *proto.PeerID, pid int, pane string) proto.PeerID {
		t.Helper()
		h.live.alive[pid] = true
		id, _, err := h.r.AllocateAndRegister(context.Background(), AllocateParams{
			Circle: "work", Backend: proto.AgentClaudeCode, Path: ptr("/p/app"), Machine: "m",
			Role: proto.RoleAgent, ClaimedPeerID: cid, AgentPID: &pid, PaneID: ptr(pane),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	holder := claim(nil, 100, "%a")

	if got := claim(&holder, 200, "%b"); got == holder {
		t.Fatalf("claim from another live pane reused holder %s", holder)
	}
	if p, _ := h.r.GetPeer(holder); p.PaneID == nil || *p.PaneID != "%a" {
		t.Fatalf("holder pane moved: %v", p.PaneID)
	}
	if got := claim(&holder, 150, "%a"); got != holder {
		t.Fatalf("same-pane wrapper pid got %s, want %s", got, holder)
	}

	delete(h.live.alive, 150)
	if got := claim(&holder, 300, "%c"); got != holder {
		t.Fatalf("claim over dead holder got %s, want %s", got, holder)
	}
}
