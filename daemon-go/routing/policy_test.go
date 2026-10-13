package routing

import (
	"testing"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/state"
)

func TestSelectionPolicyAndExclusions(t *testing.T) {
	cases := []struct {
		cfg                   config.RouterConfig
		policy                string
		agent, service, human bool // excluded?
	}{
		{config.RouterConfig{}, PolicyExcludeServiceTargets, false, true, true},
		{config.RouterConfig{AllowHumanTargets: true}, PolicyAllowHumanTargets, false, true, false},
		{config.RouterConfig{AllowServiceTargets: true}, PolicyAllowServiceTargets, false, false, false},
		{config.RouterConfig{AllowServiceTargets: true, AllowHumanTargets: true}, PolicyAllowServiceTargets, false, false, false},
	}
	for _, c := range cases {
		policy := (&Router{cfg: c.cfg}).SelectionPolicy()
		if policy != c.policy {
			t.Fatalf("%+v: policy = %s, want %s", c.cfg, policy, c.policy)
		}
		got := [3]bool{PolicyExcludes(policy, "agent"), PolicyExcludes(policy, "service"), PolicyExcludes(policy, "human")}
		if got != [3]bool{c.agent, c.service, c.human} {
			t.Fatalf("%s: excludes(agent,service,human) = %v", policy, got)
		}
	}
}

// An ask the human's inbox stored counts as delivered for replay, whatever a
// surface push reported afterwards; surface stages alone prove nothing.
func TestDeliveryOutcomeCountsInboxStoredAsDelivered(t *testing.T) {
	stored := []state.TraceRow{{Stage: "created"}, {Stage: "inbox_stored"}, {Stage: "surface_failed"}}
	if got := deliveryOutcome(stored); got != DeliveryDelivered {
		t.Fatalf("inbox_stored outcome = %s", got)
	}
	if got := deliveryOutcome([]state.TraceRow{{Stage: "created"}, {Stage: "surface_pushed"}}); got != DeliveryUnknown {
		t.Fatalf("surface stage alone = %s, want unknown", got)
	}
}
