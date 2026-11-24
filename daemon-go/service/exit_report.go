package service

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// ExitReport summarises the exit-watch soak (rfcs/agent-container.md, step
// 2): how often a tmux offline signal agreed with the agent's own process.
// Step 3 (the watch decides liveness) waits on this evidence.
type ExitReport struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// Signals counts tmux offline signals for spawned peers, by signal.
	Signals map[string]*SignalTally `json:"signals"`
	// Exits counts agent exits the watch saw; LedTmux those seen while the
	// peer was still online, before any tmux signal had offlined it.
	Exits   int `json:"exits_observed"`
	LedTmux int `json:"exits_before_tmux"`
	// Refuted lists the signals that offlined a peer whose agent was still
	// running. One whose agent exit the watch saw within DyingWindow is an
	// agent that was already dying (it outlived its pane's SIGHUP briefly),
	// not a spurious offline; the rest are what step 3 would avoid.
	Refuted []RefutedSignal `json:"refuted,omitempty"`
}

// DyingWindow is how soon after a refuted signal the agent's observed exit
// makes the signal a dying agent rather than a spurious offline.
const DyingWindow = 5 * time.Second

// SignalTally splits one signal's checks by what the watch said.
type SignalTally struct {
	Confirmed int `json:"confirmed"` // the agent had exited
	Refuted   int `json:"refuted"`   // the agent was still running and outlived DyingWindow: a spurious offline
	Dying     int `json:"dying"`     // the agent was still running but exited within DyingWindow
	Unknown   int `json:"unknown"`   // no identity to compare
	Unwatched int `json:"unwatched"` // no watch was armed for the peer (coverage gap)
}

// RefutedSignal is one signal the watch contradicted.
type RefutedSignal struct {
	At     time.Time `json:"at"`
	Signal string    `json:"signal"`
	PeerID string    `json:"peer_id"`
	PaneID string    `json:"pane_id,omitempty"`
	PID    int       `json:"pid"`
	// ExitAfter is how long after the signal the watch saw the agent exit,
	// when it did; within DyingWindow the signal counts as dying, not refuted.
	ExitAfter *time.Duration `json:"exit_after,omitempty"`
}

// SummarizeExits reads the exit-watch events since since (zero: all). rows
// are in time order (state.EventsOfTypes returns them so).
func SummarizeExits(rows []state.EventRow, since time.Time) ExitReport {
	r := ExitReport{Since: since, Signals: map[string]*SignalTally{}}
	type exit struct {
		at  time.Time
		pid int
	}
	exits := map[string][]exit{} // per peer, the observed exits
	for _, row := range rows {
		if row.Type == EventAgentExitObserved {
			peerID, _ := row.Payload["peer_id"].(string)
			pid, _ := row.Payload["pid"].(float64)
			exits[peerID] = append(exits[peerID], exit{row.At, int(pid)})
		}
	}
	exitAfter := func(peerID string, pid int, at time.Time) *time.Duration {
		for _, e := range exits[peerID] {
			if e.pid == pid && !e.at.Before(at) {
				d := e.at.Sub(at)
				return &d
			}
		}
		return nil
	}
	for _, row := range rows {
		if !since.IsZero() && row.At.Before(since) {
			continue
		}
		if row.At.After(r.Until) {
			r.Until = row.At
		}
		switch row.Type {
		case EventAgentExitObserved:
			r.Exits++
			if status, _ := row.Payload["peer_status"].(string); status == "online" || status == "busy" {
				r.LedTmux++
			}
		case EventExitSignalChecked:
			signal, _ := row.Payload["signal"].(string)
			t := r.Signals[signal]
			if t == nil {
				t = &SignalTally{}
				r.Signals[signal] = t
			}
			if watched, _ := row.Payload["watched"].(bool); !watched {
				t.Unwatched++
			}
			switch row.Payload["agent_alive"] {
			case "false":
				t.Confirmed++
			case "true":
				pane, _ := row.Payload["pane_id"].(string)
				pid, _ := row.Payload["pid"].(float64)
				peerID, _ := row.Payload["peer_id"].(string)
				after := exitAfter(peerID, int(pid), row.At)
				if after != nil && *after <= DyingWindow {
					t.Dying++
				} else {
					t.Refuted++
				}
				r.Refuted = append(r.Refuted, RefutedSignal{At: row.At, Signal: signal, PeerID: peerID, PaneID: pane, PID: int(pid), ExitAfter: after})
			default:
				t.Unknown++
			}
		}
	}
	return r
}

// Write prints the report for a human.
func (r ExitReport) Write(w io.Writer) {
	from := "the start of the journal"
	if !r.Since.IsZero() {
		from = r.Since.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(w, "exit-watch soak since %s\n", from)
	fmt.Fprintf(w, "  agent exits observed   %d (%d before tmux offlined the peer)\n", r.Exits, r.LedTmux)
	if len(r.Signals) == 0 {
		fmt.Fprintln(w, "  tmux offline signals   none checked")
	}
	names := make([]string, 0, len(r.Signals))
	for name := range r.Signals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := r.Signals[name]
		fmt.Fprintf(w, "  %-22s confirmed %d, refuted %d, dying %d, unknown %d (unwatched %d)\n", name, t.Confirmed, t.Refuted, t.Dying, t.Unknown, t.Unwatched)
	}
	for _, s := range r.Refuted {
		kind, exit := "refuted", "no exit observed"
		if s.ExitAfter != nil {
			exit = "exited " + s.ExitAfter.Round(time.Millisecond).String() + " later"
			if *s.ExitAfter <= DyingWindow {
				kind = "dying  "
			}
		}
		fmt.Fprintf(w, "  %s  %s  %s  %s pane %s pid %d (%s)\n", kind, s.At.UTC().Format(time.RFC3339), s.Signal, s.PeerID, s.PaneID, s.PID, exit)
	}
}
