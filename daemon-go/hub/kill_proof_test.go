package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"syscall"
	"testing"

	clienthooks "github.com/repowire/repowire/daemon-go/hooks"
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
	t.Setenv("HOME", t.TempDir()) // pane metadata lives under ~/.cache
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

// startShellWithChild runs a shell (the pane root) with an agent child under it.
func startShellWithChild(t *testing.T) (shell, child int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return cmd.Process.Pid, pid
}

// Real ancestry: an agent running under the pane's shell is in the pane; a
// sibling process outside it is not, and a live registered agent elsewhere
// outvotes a metadata pid that happens to sit in the pane.
func TestPaneRunsPeerFollowsProcessAncestry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	shell, child := startShellWithChild(t)
	sibling := startOccupant(t)
	pane := "%210"
	ev := &service.TmuxPaneEvidence{PaneID: pane, PanePID: strconv.Itoa(shell)}
	proc := service.NewProcessControl()
	peerWith := func(pid int) *proto.Peer { return &proto.Peer{PeerID: "repow-1-x", AgentPID: &pid} }

	if !service.PaneRunsPeer(proc, ev, peerWith(child)) {
		t.Fatal("an agent under the pane's shell must count as running in the pane")
	}
	if service.PaneRunsPeer(proc, ev, peerWith(sibling)) {
		t.Fatal("a sibling process outside the pane's tree must not count")
	}
	writeSpawnTestPaneMeta(t, pane, map[string]any{"peer_id": "repow-1-x", "agent_pid": child})
	if service.PaneRunsPeer(proc, ev, peerWith(sibling)) {
		t.Fatal("a live registered agent elsewhere must outvote a metadata pid inside the pane")
	}
	if !service.PaneRunsPeer(proc, ev, &proto.Peer{PeerID: "repow-1-x"}) {
		t.Fatal("with no registered agent, the metadata pid under the shell proves the pane")
	}
}

// #536: a pid is not an identity on its own. Metadata records the agent's start
// time, and a pid whose process started at another time was recycled.
func TestPaneRunsPeerRejectsARecycledMetadataPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	shell, child := startShellWithChild(t)
	pane := "%211"
	ev := &service.TmuxPaneEvidence{PaneID: pane, PanePID: strconv.Itoa(shell)}
	proc := service.NewProcessControl()
	p := &proto.Peer{PeerID: "repow-1-y"}

	started, ok := clienthooks.ProcessStartTime(child)
	if !ok || started == "" {
		t.Fatalf("start time of a live process: %q, %v", started, ok)
	}
	writeSpawnTestPaneMeta(t, pane, map[string]any{"peer_id": "repow-1-y", "agent_pid": child, "agent_started": "Mon Jan  1 00:00:00 2001"})
	if service.PaneRunsPeer(proc, ev, p) {
		t.Fatal("a metadata pid whose process started at another time must not prove the pane")
	}
	writeSpawnTestPaneMeta(t, pane, map[string]any{"peer_id": "repow-1-y", "agent_pid": child, "agent_started": started})
	if !service.PaneRunsPeer(proc, ev, p) {
		t.Fatal("the metadata pid with its own start time proves the pane")
	}
	writeSpawnTestPaneMeta(t, pane, map[string]any{"peer_id": "repow-1-y", "agent_pid": child})
	if !service.PaneRunsPeer(proc, ev, p) {
		t.Fatal("metadata written before start times were recorded keeps today's check")
	}
}
