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
