package service

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

func TestSummarizeExitsTalliesSignalsAgainstTheWatch(t *testing.T) {
	t0 := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	row := func(min int, typ string, payload map[string]any) state.EventRow {
		return state.EventRow{Type: typ, At: t0.Add(time.Duration(min) * time.Minute), Payload: payload}
	}
	rows := []state.EventRow{
		row(-60, EventExitSignalChecked, map[string]any{"signal": "pane_died", "agent_alive": "true", "watched": true}), // before since
		row(1, EventAgentExitObserved, map[string]any{"peer_status": "online"}),
		row(2, EventExitSignalChecked, map[string]any{"signal": "pane_died", "agent_alive": "false", "watched": true}),
		row(3, EventAgentExitObserved, map[string]any{"peer_status": "offline"}),
		row(4, EventExitSignalChecked, map[string]any{"signal": "session_closed", "agent_alive": "true", "watched": true, "peer_id": "repow-a", "pane_id": "%3", "pid": float64(77)}),
		row(5, EventExitSignalChecked, map[string]any{"signal": "session_closed", "agent_alive": "unknown", "watched": false}),
		// A pane_died whose agent outlived the pane's SIGHUP by two seconds:
		// a dying agent, not a spurious offline.
		{Type: EventExitSignalChecked, At: t0.Add(6 * time.Minute), Payload: map[string]any{"signal": "pane_died", "agent_alive": "true", "watched": true, "peer_id": "repow-b", "pid": float64(88)}},
		{Type: EventAgentExitObserved, At: t0.Add(6*time.Minute + 2*time.Second), Payload: map[string]any{"peer_id": "repow-b", "pid": float64(88), "peer_status": "offline"}},
	}
	r := SummarizeExits(rows, t0)
	if r.Exits != 3 || r.LedTmux != 1 {
		t.Fatalf("exits %d led %d; want 3 and 1", r.Exits, r.LedTmux)
	}
	if pd := r.Signals["pane_died"]; pd == nil || *pd != (SignalTally{Confirmed: 1, Dying: 1}) {
		t.Fatalf("pane_died %+v; want one confirmed and one dying (the earlier refutation is before since)", pd)
	}
	if sc := r.Signals["session_closed"]; sc == nil || *sc != (SignalTally{Refuted: 1, Unknown: 1, Unwatched: 1}) {
		t.Fatalf("session_closed %+v", sc)
	}
	if len(r.Refuted) != 2 || r.Refuted[0].PeerID != "repow-a" || r.Refuted[0].PID != 77 || r.Refuted[0].PaneID != "%3" || r.Refuted[0].ExitAfter != nil ||
		r.Refuted[1].ExitAfter == nil || *r.Refuted[1].ExitAfter != 2*time.Second {
		t.Fatalf("refuted %+v", r.Refuted)
	}
	var out bytes.Buffer
	r.Write(&out)
	if !strings.Contains(out.String(), "session_closed         confirmed 0, refuted 1, dying 0, unknown 1 (unwatched 1)") ||
		!strings.Contains(out.String(), "refuted  2026-12-01T09:04:00Z  session_closed  repow-a pane %3 pid 77 (no exit observed)") ||
		!strings.Contains(out.String(), "dying    2026-12-01T09:06:00Z  pane_died  repow-b pane  pid 88 (exited 2s later)") {
		t.Fatalf("report:\n%s", out.String())
	}
}
