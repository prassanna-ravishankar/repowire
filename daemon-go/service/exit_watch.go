package service

import (
	"context"
	"log"
	"sync"

	"github.com/repowire/repowire/daemon-go/hooks"
	"github.com/repowire/repowire/daemon-go/proto"
)

// ExitWatch observes when a spawned agent's process exits
// (rfcs/agent-container.md, step 2). It only records: an agent_exit_observed
// event when a watch fires, and an exit_signal_checked event when a tmux
// signal offlines a peer, saying whether the bound agent was in fact still
// running. Nothing here changes peer state; those events are the evidence for
// letting the watch decide later (step 3).
//
// An agent's identity is its pid with its start time (hooks.ProcessStartTime,
// the same value the hooks record as agent_started), so a recycled pid is not
// mistaken for the agent.
// The events ExitWatch records; SummarizeExits reads them back.
const (
	EventAgentExitObserved = "agent_exit_observed"
	EventExitSignalChecked = "exit_signal_checked"
)

type ExitWatch struct {
	spawned func(p *proto.Peer) bool // the peer runs in a pane Repowire spawned
	emit    func(ctx context.Context, typ string, payload map[string]any) string
	status  func(id proto.PeerID) (proto.PeerStatus, bool)
	started func(pid int) (string, bool)
	wait    func(pid int, cancel <-chan struct{}, armed func() bool) (bool, error)

	// onExit, when set, acts on an observed exit (step 3: the watch decides).
	onExit func(id proto.PeerID, pane string, pid int)

	mu      sync.Mutex
	watches map[proto.PeerID]*exitWatch // kept after an exit, until the next bind
	binds   map[proto.PeerID]uint64     // the latest Bind per peer, in call order
	closed  bool
}

type exitWatch struct {
	pid     int
	started string
	pane    string
	cancel  chan struct{}
	exited  bool // the watch saw this process end (guarded by ExitWatch.mu)
}

// NewExitWatch builds the observer. spawned decides which peers are watched;
// emit records events (the registry's AddEvent); status reads a peer's
// current status for the record.
func NewExitWatch(spawned func(p *proto.Peer) bool, emit func(context.Context, string, map[string]any) string, status func(proto.PeerID) (proto.PeerStatus, bool)) *ExitWatch {
	return &ExitWatch{spawned: spawned, emit: emit, status: status, started: hooks.ProcessStartTime, wait: waitExit, watches: map[proto.PeerID]*exitWatch{}, binds: map[proto.PeerID]uint64{}}
}

// Bind arms a watch for p's agent when p is a spawned, pane-backed peer with a
// known agent pid (Adopt is Bind over the restored peers). A re-bind to the
// same process is a no-op; to another process it replaces the watch. It shells
// out, so callers on a request path use BindAsync.
func (w *ExitWatch) Bind(p proto.Peer) { w.bind(p, w.claim(p.PeerID)) }

// BindAsync is Bind in the background. The order of calls decides: a bind
// that finishes after a later one for the same peer is dropped, so a slow,
// stale registration cannot replace a newer runtime's watch.
func (w *ExitWatch) BindAsync(p proto.Peer) {
	seq := w.claim(p.PeerID)
	go w.bind(p, seq)
}

func (w *ExitWatch) claim(id proto.PeerID) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.binds[id]++
	return w.binds[id]
}

func (w *ExitWatch) bind(p proto.Peer, seq uint64) {
	if p.PaneID == nil || *p.PaneID == "" || p.AgentPID == nil || *p.AgentPID <= 0 || !w.spawned(&p) {
		return
	}
	pid := *p.AgentPID
	started, ok := w.started(pid)
	if !ok {
		return // not running now: no identity to watch
	}
	w.mu.Lock()
	if w.closed || w.binds[p.PeerID] != seq {
		w.mu.Unlock()
		return // closed, or a later bind for this peer has taken over
	}
	if cur := w.watches[p.PeerID]; cur != nil {
		if cur.pid == pid && cur.started == started && !cur.exited {
			w.mu.Unlock()
			return
		}
		if !cur.exited {
			close(cur.cancel)
		}
	}
	ew := &exitWatch{pid: pid, started: started, pane: *p.PaneID, cancel: make(chan struct{})}
	w.watches[p.PeerID] = ew
	w.mu.Unlock()
	go w.run(p.PeerID, ew)
}

func (w *ExitWatch) run(id proto.PeerID, ew *exitWatch) {
	exited, err := w.wait(ew.pid, ew.cancel, func() bool {
		// Re-check identity once armed: an exit between Bind's check and the
		// arming would otherwise be missed, or a recycled pid watched.
		s, ok := w.started(ew.pid)
		return ok && s == ew.started
	})
	w.mu.Lock()
	current := w.watches[id] == ew
	if current && exited {
		ew.exited = true // kept, so a signal checked after the exit still sees it
	} else if current {
		delete(w.watches, id)
	}
	w.mu.Unlock()
	if err != nil {
		log.Printf("exit watch: %s pid %d: %v", id, ew.pid, err)
		return
	}
	if !current || !exited {
		return // replaced or cancelled
	}
	status, known := w.status(id)
	payload := map[string]any{"peer_id": string(id), "pid": ew.pid, "pane_id": ew.pane, "peer_known": known}
	if known {
		payload["peer_status"] = string(status)
	}
	w.emit(context.Background(), EventAgentExitObserved, payload)
	if w.onExit != nil {
		w.onExit(id, ew.pane, ew.pid)
	}
}

// OnExit makes the watch act on what it observes (rfcs/agent-container.md,
// step 3): fn runs after each recorded exit. Set it once, while wiring.
func (w *ExitWatch) OnExit(fn func(id proto.PeerID, pane string, pid int)) { w.onExit = fn }

// AgentAlive reports p's agent as the watch sees it. watched is false when
// no watch covers p's current agent pid; the caller then keeps its own
// evidence. It reads only the watch's state, never the process table.
func (w *ExitWatch) AgentAlive(p *proto.Peer) (alive, watched bool) {
	if p == nil || p.AgentPID == nil {
		return false, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ew := w.watches[p.PeerID]
	if ew == nil || ew.pid != *p.AgentPID {
		return false, false
	}
	return !ew.exited, true
}

// CheckSignal records, for a tmux signal about to offline p, whether p's bound
// agent was still running: a still-running agent marks the signal as one the
// exit watch would not have acted on.
func (w *ExitWatch) CheckSignal(ctx context.Context, signal string, p *proto.Peer) {
	if p == nil || p.AgentPID == nil || *p.AgentPID <= 0 {
		return
	}
	pid := *p.AgentPID
	w.mu.Lock()
	ew := w.watches[p.PeerID]
	exited := ew != nil && ew.exited
	w.mu.Unlock()
	// A watched peer counts as spawned even once its pane is gone (pane death
	// forgets the ownership before this runs).
	if ew == nil && !w.spawned(p) {
		return
	}
	alive := "unknown"
	if ew != nil && ew.pid == pid {
		if exited {
			alive = "false"
		} else if s, ok := w.started(pid); ok && s == ew.started {
			alive = "true"
		} else {
			alive = "false"
		}
	} else if _, ok := w.started(pid); !ok {
		alive = "false"
	}
	payload := map[string]any{"signal": signal, "peer_id": string(p.PeerID), "pid": pid, "agent_alive": alive, "watched": ew != nil}
	if p.PaneID != nil {
		payload["pane_id"] = *p.PaneID
	}
	w.emit(ctx, EventExitSignalChecked, payload)
}

// Close cancels every watch; no watch is armed afterwards.
func (w *ExitWatch) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for id, ew := range w.watches {
		if !ew.exited {
			close(ew.cancel)
		}
		delete(w.watches, id)
	}
}
