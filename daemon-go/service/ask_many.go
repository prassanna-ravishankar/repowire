package service

// In-memory ask-many fanout tracking. Parent anchors are never delivered;
// children are normal AskTracker rows with ParentID set. Mirrors the Python
// AskManyTracker: no timers, no persistence, timeout is derived lazily at read.

import (
	"sync"
	"time"
)

const (
	DefaultAskManyTimeoutSeconds = 300
	MaxAskManyPeers              = 50
)

type AskManyChild struct {
	PeerName      string
	CorrelationID *string
	PeerID        *string
	DeliveryError *string
}

type AskManyParent struct {
	ParentID       string
	FromPeerName   string
	Text           string
	Children       []AskManyChild
	CreatedAt      time.Time
	TimeoutSeconds int
}

type AskManyTracker struct {
	mu      sync.Mutex
	asks    *AskTracker
	parents map[string]*AskManyParent
}

func NewAskManyTracker(asks *AskTracker) *AskManyTracker {
	return &AskManyTracker{asks: asks, parents: map[string]*AskManyParent{}}
}

func (t *AskManyTracker) Create(fromPeerName, text string, timeoutSeconds int) *AskManyParent {
	if timeoutSeconds <= 0 {
		timeoutSeconds = DefaultAskManyTimeoutSeconds
	}
	parent := &AskManyParent{
		ParentID:       "askm-" + hex8(),
		FromPeerName:   fromPeerName,
		Text:           text,
		CreatedAt:      time.Now().UTC(),
		TimeoutSeconds: timeoutSeconds,
	}
	t.mu.Lock()
	t.parents[parent.ParentID] = parent
	t.mu.Unlock()
	return parent
}

func (t *AskManyTracker) AddChild(parentID string, child AskManyChild) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if parent := t.parents[parentID]; parent != nil {
		parent.Children = append(parent.Children, child)
	}
}

func (t *AskManyTracker) Status(parentID string, now time.Time) (map[string]any, bool) {
	t.mu.Lock()
	parent := t.parents[parentID]
	if parent == nil {
		t.mu.Unlock()
		return nil, false
	}
	cp := *parent
	cp.Children = append([]AskManyChild(nil), parent.Children...)
	t.mu.Unlock()

	if now.IsZero() {
		now = time.Now().UTC()
	}
	counts := map[string]int{"acked": 0, "replied": 0, "pending": 0, "failed": 0}
	children := make([]map[string]any, 0, len(cp.Children))
	for _, child := range cp.Children {
		var ask *Ask
		if child.CorrelationID != nil {
			if got, ok := t.asks.Get(*child.CorrelationID); ok {
				ask = got
			}
		}
		status := askManyChildStatus(ask)
		counts[status]++
		var reply, closeReason any
		if ask != nil {
			reply = ask.ReplyText
			closeReason = ask.CloseReason
		}
		children = append(children, map[string]any{
			"peer":           child.PeerName,
			"peer_id":        child.PeerID,
			"correlation_id": child.CorrelationID,
			"status":         status,
			"reply":          reply,
			"close_reason":   closeReason,
			"error":          child.DeliveryError,
		})
	}

	deadline := cp.CreatedAt.Add(time.Duration(cp.TimeoutSeconds) * time.Second)
	anyOpen := counts["pending"] > 0
	timedOut := anyOpen && !now.Before(deadline)
	state := "pending"
	if !anyOpen {
		state = "complete"
	} else if timedOut {
		state = "partial"
	}
	return map[string]any{
		"parent_id":  cp.ParentID,
		"from_peer":  cp.FromPeerName,
		"text":       cp.Text,
		"created_at": cp.CreatedAt.Format(time.RFC3339Nano),
		"deadline":   deadline.Format(time.RFC3339Nano),
		"state":      state,
		"timed_out":  timedOut,
		"rollup": map[string]int{
			"total":   len(cp.Children),
			"acked":   counts["acked"],
			"replied": counts["replied"],
			"pending": counts["pending"],
			"failed":  counts["failed"],
		},
		"children": children,
	}, true
}

func askManyChildStatus(ask *Ask) string {
	if ask == nil {
		return "failed"
	}
	if !ask.Closed {
		return "pending"
	}
	if ask.CloseReason == "send_failed" {
		return "failed"
	}
	if ask.ReplyText != nil {
		return "replied"
	}
	return "acked"
}
