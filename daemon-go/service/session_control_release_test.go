package service

import (
	"context"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// releaseWork is a tracked job whose current attempt acquired a spawned
// executor in pane %7 for peer repow-x.
func releaseWork() *state.TrackedWork {
	return &state.TrackedWork{WorkID: "w-1", Provenance: map[string]any{"runner": map[string]any{
		"current_attempt_id": "a-1",
		"attempts": []any{map[string]any{"attempt_id": "a-1", "acquisition": map[string]any{
			"release_handle": map[string]any{"kind": "tmux_pane", "peer_id": "repow-x", "pane_id": "%7", "operation_id": "op-acq"},
		}}},
	}}}
}

// #586: releasing a job's executor stops its pane through StopProven, the
// same proven path kill_peer takes, instead of only unregistering the peer.
func TestReleaseExecutorStopsTheExecutorsPane(t *testing.T) {
	pane := "%7"
	agent := 4242
	live := &proto.Peer{PeerID: "repow-x", PaneID: &pane, AgentPID: &agent}
	for _, tc := range []struct {
		name        string
		stop        func(p *proto.Peer) StopOutcome
		status      string
		paneStopped bool
		skipped     string
	}{
		{"proven and stopped", nil, "released", true, ""},
		{"no proof: the pane is left alone", func(*proto.Peer) StopOutcome {
			return StopOutcome{Proof: DestructiveProof{ErrCode: "missing_pane_metadata"}}
		}, "released", false, "missing_pane_metadata"},
		{"proven but tmux refused", func(p *proto.Peer) StopOutcome {
			return StopOutcome{Proof: DestructiveProof{OK: true, PaneID: *p.PaneID}}
		}, "failed", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawner := &scSpawner{stop: tc.stop}
			c := NewSessionControl(&scRegistry{peers: []*proto.Peer{live}}, spawner, scStore(t))
			out, err := c.ReleaseExecutorForWork(context.Background(), releaseWork(), "completed")
			if err != nil {
				t.Fatalf("release: %v", err)
			}
			if out["status"] != tc.status {
				t.Fatalf("status = %v, want %s (%v)", out["status"], tc.status, out)
			}
			if len(spawner.stopped) != 1 || *spawner.stopped[0].PaneID != pane || spawner.stopped[0].AgentPID == nil {
				t.Fatalf("StopProven got %v, want the live peer in pane %s", spawner.stopped, pane)
			}
			if tc.status == "released" {
				if out["pane_stopped"] != tc.paneStopped {
					t.Fatalf("pane_stopped = %v, want %v", out["pane_stopped"], tc.paneStopped)
				}
				if got, _ := out["stop_skipped"].(string); got != tc.skipped {
					t.Fatalf("stop_skipped = %q, want %q", got, tc.skipped)
				}
			}
		})
	}
}

// A release handle whose peer the registry no longer knows names a pane that
// may since be someone else's: nothing is stopped.
func TestReleaseExecutorLeavesAnUnregisteredExecutorsPaneAlone(t *testing.T) {
	spawner := &scSpawner{}
	c := NewSessionControl(&scRegistry{}, spawner, scStore(t))
	out, err := c.ReleaseExecutorForWork(context.Background(), releaseWork(), "completed")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(spawner.stopped) != 0 || out["pane_stopped"] != false || out["stop_skipped"] != "executor_not_registered" {
		t.Fatalf("got %v with stops %v; want no stop, skipped executor_not_registered", out, spawner.stopped)
	}
}

type stopContainer struct {
	evidence map[string]*TmuxPaneEvidence
	stopped  []string
}

func (c *stopContainer) Kind() string                               { return "tmux" }
func (c *stopContainer) Reserve(SpawnConfig) (PanePlacement, error) { return PanePlacement{}, nil }
func (c *stopContainer) Launch(SpawnConfig) (SpawnResult, error)    { return SpawnResult{}, nil }
func (c *stopContainer) Probe(pane string) *TmuxPaneEvidence        { return c.evidence[pane] }
func (c *stopContainer) Stop(pane string) bool                      { c.stopped = append(c.stopped, pane); return true }

// StopProven pinned to a pane refuses a proof for any other pane.
func TestStopProvenRefusesAProofForAnotherPane(t *testing.T) {
	t.Setenv("REPOWIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // StopProven clears ws-hook pane files under ~/.cache
	ct := &stopContainer{evidence: map[string]*TmuxPaneEvidence{}}
	own := NewFileOwnership("test-host", ct.Probe)
	own.MarkSpawned("%8")
	svc := NewSpawnService(ct, own, nil, nil)
	pane := "%8"
	p := &proto.Peer{PeerID: "repow-x", PaneID: &pane}

	if out := svc.StopProven(NewProcessControl(), p, "%7"); out.Stopped || out.Proof.ErrCode != "pane_mismatch" || len(ct.stopped) != 0 {
		t.Fatalf("pinned to %%7: got %+v, stopped %v; want pane_mismatch and no stop", out, ct.stopped)
	}
	if out := svc.StopProven(NewProcessControl(), p, "%8"); !out.Stopped || len(ct.stopped) != 1 || ct.stopped[0] != "%8" {
		t.Fatalf("pinned to %%8: got %+v, stopped %v; want %%8 stopped", out, ct.stopped)
	}
}
