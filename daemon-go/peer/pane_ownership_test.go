package peer

import (
	"context"
	"errors"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// fakeProc is an injectable ProcessProbe for the destructive pane-claim proof.
type fakeProc struct {
	ancestors map[int]map[int]struct{}
	paneRoot  map[string]int
}

func (p fakeProc) Ancestors(pid int) (map[int]struct{}, bool) {
	a, ok := p.ancestors[pid]
	return a, ok
}
func (p fakeProc) PaneRootPID(pane string) (int, bool) {
	r, ok := p.paneRoot[pane]
	return r, ok
}

func noTransport() fakeTransport { return fakeTransport{connected: map[proto.PeerID]bool{}} }

// A subprocess inheriting TMUX_PANE (parent_pid == the live holder's agent_pid)
// must NOT take the holder's pane — hard reject, holder untouched.
func TestRegister_DirectChildHijackRejected(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistryWith(t, noTransport(), fakeLive{alive: map[int]bool{1000: true}})

	holder, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w/holder"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%1"), AgentPID: ptr(1000),
	})
	if err != nil {
		t.Fatalf("holder register: %v", err)
	}

	_, _, err = r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentCodex, Path: ptr("/w/child"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%1"), AgentPID: ptr(2000), ParentPID: ptr(1000),
	})
	if !errors.Is(err, ErrPaneHijackRejected) {
		t.Fatalf("want ErrPaneHijackRejected, got %v", err)
	}
	if p, _ := r.GetPeer(holder); p.PaneID == nil || *p.PaneID != "%1" {
		t.Fatalf("holder lost its pane: %v", p.PaneID)
	}
}

// A holder whose agent process is dead is the legitimate pane-reuse case: a new
// peer takes the pane and the dead holder is released offline.
func TestRegister_DeadHolderPaneReclaimed(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistryWith(t, noTransport(), fakeLive{alive: map[int]bool{}}) // 1001 NOT alive

	holder, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w/old"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%2"), AgentPID: ptr(1001),
	})
	claimant, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentCodex, Path: ptr("/w/new"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%2"), AgentPID: ptr(2002),
	})
	if err != nil {
		t.Fatalf("claimant register: %v", err)
	}
	if cp, _ := r.GetPeer(claimant); cp.PaneID == nil || *cp.PaneID != "%2" {
		t.Fatalf("claimant should hold pane %%2, got %v", cp.PaneID)
	}
	hp, _ := r.GetPeer(holder)
	if hp.PaneID != nil {
		t.Fatalf("dead holder should be released, still holds %v", hp.PaneID)
	}
	if hp.Status != proto.StatusOffline {
		t.Fatalf("released holder status=%q, want offline", hp.Status)
	}
}

// A live orchestrator's pane is sticky: a temporary same-pane claimant registers
// pane-less and the orchestrator keeps the pane and stays online.
func TestRegister_StickyOrchestratorPreserved(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistryWith(t, noTransport(), fakeLive{alive: map[int]bool{1003: true}})

	orch, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w/orch"), Machine: "m",
		Role: proto.RoleOrchestrator, PaneID: ptr("%3"), AgentPID: ptr(1003),
	})
	claimant, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentCodex, Path: ptr("/w/tmp"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%3"), AgentPID: ptr(2003),
	})
	if err != nil {
		t.Fatalf("claimant register: %v", err)
	}
	if cp, _ := r.GetPeer(claimant); cp.PaneID != nil {
		t.Fatalf("claimant should be pane-less, got %v", cp.PaneID)
	}
	op, _ := r.GetPeer(orch)
	if op.PaneID == nil || *op.PaneID != "%3" {
		t.Fatalf("orchestrator lost its pane: %v", op.PaneID)
	}
	if op.Status != proto.StatusOnline {
		t.Fatalf("orchestrator status=%q, want online", op.Status)
	}
}

// A claimant that cannot prove it runs in the pane (not the holder's agent, not
// in the pane's process tree) registers pane-less; the live holder keeps the pane.
func TestRegister_UnprovenClaimPaneLess(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistryWith(t, noTransport(), fakeLive{alive: map[int]bool{1004: true}})
	r.WithProcessProbe(fakeProc{
		ancestors: map[int]map[int]struct{}{2004: {9999: {}}}, // unrelated ancestor
		paneRoot:  map[string]int{"%4": 5000},
	})

	holder, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w/h"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%4"), AgentPID: ptr(1004),
	})
	claimant, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentCodex, Path: ptr("/w/c"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%4"), AgentPID: ptr(2004),
	})
	if err != nil {
		t.Fatalf("claimant register: %v", err)
	}
	if cp, _ := r.GetPeer(claimant); cp.PaneID != nil {
		t.Fatalf("unproven claimant should be pane-less, got %v", cp.PaneID)
	}
	if hp, _ := r.GetPeer(holder); hp.PaneID == nil || *hp.PaneID != "%4" {
		t.Fatalf("live holder should keep its pane, got %v", hp.PaneID)
	}
}

// A claimant whose process tree reaches the pane's root pid HAS proof: it takes
// the pane and the prior holder is displaced.
func TestRegister_ProvenClaimTakesPane(t *testing.T) {
	ctx := context.Background()
	r, _ := newRegistryWith(t, noTransport(), fakeLive{alive: map[int]bool{1005: true}})
	r.WithProcessProbe(fakeProc{
		ancestors: map[int]map[int]struct{}{2005: {5005: {}}}, // ancestor is the pane root
		paneRoot:  map[string]int{"%5": 5005},
	})

	holder, _, _ := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentClaudeCode, Path: ptr("/w/h"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%5"), AgentPID: ptr(1005),
	})
	claimant, _, err := r.AllocateAndRegister(ctx, AllocateParams{
		Circle: "c", Backend: proto.AgentCodex, Path: ptr("/w/c"), Machine: "m",
		Role: proto.RoleAgent, PaneID: ptr("%5"), AgentPID: ptr(2005),
	})
	if err != nil {
		t.Fatalf("claimant register: %v", err)
	}
	if cp, _ := r.GetPeer(claimant); cp.PaneID == nil || *cp.PaneID != "%5" {
		t.Fatalf("proven claimant should take the pane, got %v", cp.PaneID)
	}
	if hp, _ := r.GetPeer(holder); hp.PaneID != nil {
		t.Fatalf("displaced holder should be pane-less, got %v", hp.PaneID)
	}
}
