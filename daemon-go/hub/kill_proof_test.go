package hub

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// startOccupant runs a real process standing in for whatever agent a pane runs
// now; its pid is the pane's root pid.
func startOccupant(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start occupant: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// deadPID returns the pid of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	return cmd.Process.Pid
}

// killProofFixture is the 2026-10-17 shape (#529): a stale claude peer, its pane
// long gone, and a live pane in the same project whose spawn-ownership record
// carries no peer_id and which now runs a different agent.
type killProofFixture struct {
	peer     *proto.Peer
	reg      *fakeSpawnRegistry
	tmux     *fakeTmux
	svc      *service.SpawnService
	srv      func(KillPeerRequest) KillResponse
	pane     string
	occupant int
}

func newKillProofFixture(t *testing.T, agentPID *int) *killProofFixture {
	t.Helper()
	path := t.TempDir()
	pane, occupant := "%109", startOccupant(t)
	p := &proto.Peer{PeerID: "repow-1-e9cb2c1d", DisplayName: "repowire-4-claude-code",
		Backend: proto.AgentClaudeCode, Circle: "1", Role: proto.RoleAgent, Path: path, AgentPID: agentPID}
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{p}}
	tmux := &fakeTmux{killOK: true, evidence: map[string]*service.TmuxPaneEvidence{
		pane: {PaneID: pane, SessionName: "1", TmuxSession: "1:repowire", CurrentPath: path, PanePID: strconv.Itoa(occupant), WindowPanes: 1},
	}}
	srv, svc := newSpawnTestHub(t, reg, tmux)
	svc.Ownership().Record(service.OwnershipRecord{PaneID: pane, Path: path, Backend: string(proto.AgentClaudeCode),
		Circle: "1", Role: string(proto.RoleAgent), TmuxSession: "1:repowire"})
	return &killProofFixture{peer: p, reg: reg, tmux: tmux, svc: svc, pane: pane, occupant: occupant,
		srv: func(req KillPeerRequest) KillResponse {
			resp := postSpawnJSON(t, srv, "/kill-peer", req)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("kill-peer status = %d", resp.StatusCode)
			}
			var out KillResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return out
		}}
}

func (f *killProofFixture) assertPaneSpared(t *testing.T, out KillResponse) {
	t.Helper()
	if len(f.tmux.killed) != 0 || out.TmuxKilled != nil {
		t.Fatalf("killed %v (tmux_killed=%v); the pane runs another agent and must be spared", f.tmux.killed, out.TmuxKilled)
	}
	if v := f.svc.Ownership().ValidateBootstrap(f.pane); v.Record == nil || v.Record.PeerID != nil {
		t.Fatalf("ownership record = %+v; an unproven adoption must not backfill a peer_id", v.Record)
	}
}

func TestKillPeerSparesUnboundOwnedPaneRunningAnotherAgent(t *testing.T) {
	dead := deadPID(t)
	f := newKillProofFixture(t, &dead)
	f.assertPaneSpared(t, f.srv(KillPeerRequest{PeerIdentifier: string(f.peer.PeerID)}))
	if len(f.reg.unregistered) != 1 {
		t.Fatalf("unregistered = %v, want the stale peer deregistered", f.reg.unregistered)
	}
}

func TestKillPeerSparesUnboundOwnedPaneWhenAgentUnknown(t *testing.T) {
	f := newKillProofFixture(t, nil)
	f.assertPaneSpared(t, f.srv(KillPeerRequest{PeerIdentifier: string(f.peer.PeerID)}))
}

func TestKillPeerKillsUnboundOwnedPaneThatRunsThePeer(t *testing.T) {
	f := newKillProofFixture(t, nil)
	f.peer.AgentPID = &f.occupant
	out := f.srv(KillPeerRequest{PeerIdentifier: string(f.peer.PeerID)})
	if out.TmuxKilled == nil || !*out.TmuxKilled || len(f.tmux.killed) != 1 || f.tmux.killed[0] != f.pane {
		t.Fatalf("killed %v (tmux_killed=%v), want %s: the pane runs this peer's agent", f.tmux.killed, out.TmuxKilled, f.pane)
	}
}

// Pane metadata outlives its occupant (it is only cleared on a daemon kill), so
// a meta file naming the peer is not proof unless its agent still runs there.
func TestDestructivePaneProofRejectsStalePaneMetadata(t *testing.T) {
	f := newKillProofFixture(t, nil)
	f.svc.Ownership().Forget(f.pane)
	f.peer.PaneID = &f.pane
	h := &Hub{}
	h.WithSpawn(f.svc, f.reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)

	writeSpawnTestPaneMeta(t, f.pane, map[string]any{"peer_id": string(f.peer.PeerID), "agent_pid": deadPID(t)})
	if proof := h.destructivePaneProof(f.peer); proof.ok {
		t.Fatalf("stale pane metadata proved ownership (mode %q)", proof.mode)
	}
	writeSpawnTestPaneMeta(t, f.pane, map[string]any{"peer_id": string(f.peer.PeerID), "agent_pid": f.occupant})
	if proof := h.destructivePaneProof(f.peer); !proof.ok || proof.mode != "verified_pane_metadata" {
		t.Fatalf("live pane metadata: ok=%v mode=%q err=%q", proof.ok, proof.mode, proof.errCode)
	}
}

// A known agent that is not in the pane vetoes even the spawned-set proof.
func TestDestructivePaneProofVetoesPaneWithoutThePeersAgent(t *testing.T) {
	f := newKillProofFixture(t, nil)
	f.svc.Ownership().Forget(f.pane)
	f.svc.Ownership().MarkSpawned(f.pane)
	f.peer.PaneID = &f.pane
	elsewhere := startOccupant(t)
	f.peer.AgentPID = &elsewhere
	h := &Hub{}
	h.WithSpawn(f.svc, f.reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	if proof := h.destructivePaneProof(f.peer); proof.ok || proof.errCode != "agent_not_in_pane" {
		t.Fatalf("proof = %+v, want agent_not_in_pane", proof)
	}
}
