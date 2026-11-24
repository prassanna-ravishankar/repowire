package service

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

type recordedEvents struct {
	mu     sync.Mutex
	events []map[string]any
	types  []string
	got    chan string
}

func newRecordedEvents() *recordedEvents { return &recordedEvents{got: make(chan string, 16)} }

func (r *recordedEvents) emit(_ context.Context, typ string, payload map[string]any) string {
	r.mu.Lock()
	r.types = append(r.types, typ)
	r.events = append(r.events, payload)
	r.mu.Unlock()
	r.got <- typ
	return ""
}

func (r *recordedEvents) next(t *testing.T) (string, map[string]any) {
	t.Helper()
	select {
	case typ := <-r.got:
		r.mu.Lock()
		defer r.mu.Unlock()
		return typ, r.events[len(r.events)-1]
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
		return "", nil
	}
}

func startAgent(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func watchedPeer(pid int) proto.Peer {
	pane := "%42"
	return proto.Peer{PeerID: "repow-w", PaneID: &pane, AgentPID: &pid}
}

func newTestExitWatch(rec *recordedEvents) *ExitWatch {
	return NewExitWatch(func(*proto.Peer) bool { return true }, rec.emit,
		func(proto.PeerID) (proto.PeerStatus, bool) { return proto.StatusOnline, true })
}

// The watch sees a real agent process exit and records it with the peer's
// status at that moment (online here: the tmux signal has not come yet).
func TestExitWatchObservesTheAgentProcessExit(t *testing.T) {
	rec := newRecordedEvents()
	w := newTestExitWatch(rec)
	defer w.Close()
	agent := startAgent(t)
	p := watchedPeer(agent.Process.Pid)
	w.Bind(p)
	w.Bind(p) // the same process again: still one watch

	w.CheckSignal(context.Background(), "pane_died", &p)
	if typ, ev := rec.next(t); typ != "exit_signal_checked" || ev["agent_alive"] != "true" || ev["watched"] != true {
		t.Fatalf("before exit: %s %v; want exit_signal_checked agent_alive=true watched", typ, ev)
	}

	_ = agent.Process.Kill()
	_ = agent.Wait()
	typ, ev := rec.next(t)
	if typ != "agent_exit_observed" || ev["pid"] != agent.Process.Pid || ev["pane_id"] != "%42" || ev["peer_status"] != "online" {
		t.Fatalf("exit: %s %v; want agent_exit_observed for pid %d in %%42 while online", typ, ev, agent.Process.Pid)
	}

	w.CheckSignal(context.Background(), "session_closed", &p)
	if typ, ev := rec.next(t); typ != "exit_signal_checked" || ev["agent_alive"] != "false" {
		t.Fatalf("after exit: %s %v; want agent_alive=false", typ, ev)
	}
}

// Close cancels a watch: the agent's later exit records nothing, and no
// watch is armed after Close.
func TestExitWatchCloseCancelsWithoutAnEvent(t *testing.T) {
	rec := newRecordedEvents()
	w := newTestExitWatch(rec)
	agent := startAgent(t)
	w.Bind(watchedPeer(agent.Process.Pid))
	w.Close()
	w.Bind(watchedPeer(agent.Process.Pid))
	_ = agent.Process.Kill()
	_ = agent.Wait()
	select {
	case typ := <-rec.got:
		t.Fatalf("got %s after Close; want nothing", typ)
	case <-time.After(500 * time.Millisecond):
	}
}

// A peer that is not spawned, or has no pane or pid, is never watched.
func TestExitWatchOnlyWatchesSpawnedPaneAgents(t *testing.T) {
	rec := newRecordedEvents()
	w := NewExitWatch(func(*proto.Peer) bool { return false }, rec.emit,
		func(proto.PeerID) (proto.PeerStatus, bool) { return proto.StatusOnline, true })
	defer w.Close()
	agent := startAgent(t)
	p := watchedPeer(agent.Process.Pid)
	w.Bind(p)
	w.CheckSignal(context.Background(), "pane_died", &p)
	w.mu.Lock()
	n := len(w.watches)
	w.mu.Unlock()
	if n != 0 || len(rec.got) != 0 {
		t.Fatalf("watches=%d events=%d; want none for a peer Repowire did not spawn", n, len(rec.got))
	}
}

// A pane-died signal checked after the watch saw the exit still records it,
// although pane death has already forgotten the spawn ownership by then.
func TestExitWatchKeepsAnExitForALaterSignal(t *testing.T) {
	rec := newRecordedEvents()
	spawned := true
	var mu sync.Mutex
	w := NewExitWatch(func(*proto.Peer) bool { mu.Lock(); defer mu.Unlock(); return spawned }, rec.emit,
		func(proto.PeerID) (proto.PeerStatus, bool) { return proto.StatusOnline, true })
	defer w.Close()
	agent := startAgent(t)
	p := watchedPeer(agent.Process.Pid)
	w.Bind(p)
	_ = agent.Process.Kill()
	_ = agent.Wait()
	if typ, _ := rec.next(t); typ != "agent_exit_observed" {
		t.Fatalf("got %s, want agent_exit_observed", typ)
	}
	mu.Lock()
	spawned = false // pane death forgot the ownership
	mu.Unlock()
	w.CheckSignal(context.Background(), "pane_died", &p)
	if typ, ev := rec.next(t); typ != "exit_signal_checked" || ev["agent_alive"] != "false" || ev["watched"] != true {
		t.Fatalf("got %s %v; want exit_signal_checked agent_alive=false watched", typ, ev)
	}
}

// Binds apply in call order: one that finishes after a later bind for the
// same peer is dropped, so a stale registration cannot replace a newer watch.
func TestExitWatchDropsAStaleBind(t *testing.T) {
	rec := newRecordedEvents()
	w := newTestExitWatch(rec)
	defer w.Close()
	older, newer := startAgent(t), startAgent(t)
	staleSeq := w.claim("repow-w")
	w.Bind(watchedPeer(newer.Process.Pid))
	w.bind(watchedPeer(older.Process.Pid), staleSeq)
	w.mu.Lock()
	got := w.watches["repow-w"]
	w.mu.Unlock()
	if got == nil || got.pid != newer.Process.Pid {
		t.Fatalf("watching %+v; want the newer agent %d", got, newer.Process.Pid)
	}
}
