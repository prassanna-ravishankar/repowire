package peer

import (
	"errors"

	"github.com/repowire/repowire/daemon-go/proto"
)

// LifecycleState is the peer lifecycle FSM state. It is DISTINCT from
// proto.PeerStatus: the FSM is richer (it has Unregistered and Retired, which
// are not wire-facing liveness values). Status changes only ever flow through
// Apply, never by assigning peer.Status directly outside the registry — that is
// what makes the stale-status bug class uncompilable at this layer.
type LifecycleState string

const (
	StateUnregistered LifecycleState = "Unregistered"
	StateOnline       LifecycleState = "Online"
	StateBusy         LifecycleState = "Busy"
	StateOffline      LifecycleState = "Offline"
	StateRetired      LifecycleState = "Retired"
)

// LifecycleEvent is an input to the FSM. Every transition is enumerated in
// Apply; an unenumerated (state, event) pair is rejected with
// ErrIllegalTransition.
type LifecycleEvent string

const (
	EventConnect              LifecycleEvent = "Connect"
	EventUserPromptSubmit     LifecycleEvent = "UserPromptSubmit"
	EventNotification         LifecycleEvent = "Notification"
	EventStop                 LifecycleEvent = "Stop"
	EventTransportDisconnect  LifecycleEvent = "TransportDisconnect"
	EventGhostDemote          LifecycleEvent = "GhostDemote"
	EventPaneDisplaced        LifecycleEvent = "PaneDisplaced"
	EventTerminalOffline      LifecycleEvent = "TerminalOffline"
	EventReconnect            LifecycleEvent = "Reconnect"
	EventReap                 LifecycleEvent = "Reap"
	EventReclaimWithLiveAgent LifecycleEvent = "ReclaimWithLiveAgent"
)

// ErrIllegalTransition is returned by Apply for any (state, event) pair that is
// not an enumerated transition.
var ErrIllegalTransition = errors.New("peer: illegal lifecycle transition")

// Apply is the exhaustive transition function. It is the single authority on
// legal lifecycle moves; the registry routes every state change through it and
// emits a contradiction (fail loud) when Apply rejects a move.
func Apply(s LifecycleState, e LifecycleEvent) (LifecycleState, error) {
	switch s {
	case StateUnregistered:
		switch e {
		case EventConnect:
			return StateOnline, nil
		}
	case StateOnline:
		switch e {
		case EventUserPromptSubmit:
			return StateBusy, nil
		case EventNotification:
			return StateOnline, nil
		case EventStop:
			return StateOnline, nil
		case EventTransportDisconnect:
			return StateOffline, nil
		case EventGhostDemote:
			return StateOffline, nil
		case EventPaneDisplaced:
			return StateOffline, nil
		case EventTerminalOffline:
			return StateRetired, nil
		}
	case StateBusy:
		switch e {
		case EventStop:
			return StateOnline, nil
		case EventNotification:
			return StateOnline, nil
		case EventUserPromptSubmit:
			return StateBusy, nil
		case EventTransportDisconnect:
			return StateOffline, nil
		case EventGhostDemote:
			return StateOffline, nil
		case EventPaneDisplaced:
			return StateOffline, nil
		case EventTerminalOffline:
			return StateRetired, nil
		}
	case StateOffline:
		switch e {
		case EventTerminalOffline:
			return StateRetired, nil
		case EventReconnect:
			return StateOnline, nil
		case EventReap:
			return StateRetired, nil
		}
	case StateRetired:
		switch e {
		case EventReclaimWithLiveAgent:
			return StateOnline, nil
		}
	}
	return s, ErrIllegalTransition
}

// ToStatus maps a lifecycle state onto the wire-facing proto.PeerStatus. The
// bool is false when there is no wire status to report (Unregistered), so a
// caller cannot silently treat a pre-registration peer as offline.
func (s LifecycleState) ToStatus() (proto.PeerStatus, bool) {
	switch s {
	case StateOnline:
		return proto.StatusOnline, true
	case StateBusy:
		return proto.StatusBusy, true
	case StateOffline, StateRetired:
		return proto.StatusOffline, true
	default: // StateUnregistered
		return "", false
	}
}
