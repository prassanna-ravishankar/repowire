// Package push turns daemon events addressed to the human into APNs pushes for
// registered native app installs. The daemon decides what is push-worthy; the
// relay holds the APNs key and only sends.
package push

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

// Events is the registry seam: the in-process dashboard event log.
type Events interface {
	SubscribeEvents() (<-chan struct{}, func())
	EventsSince(eventID string) []map[string]any
	GetEvents() []map[string]any
}

// Devices is the device-token store seam.
type Devices interface {
	ListPushDevices(ctx context.Context) ([]state.PushDevice, error)
}

// Sender delivers a push frame (the relay client).
type Sender interface {
	Push(ctx context.Context, frame map[string]any) error
}

// humanTargets are the addresses that mean "the person holding the phone".
var humanTargets = map[string]bool{"human": true, "dashboard": true}

const maxBody = 240

// Run forwards push-worthy events until ctx ends. It wakes on event-log
// appends (no timer) and only considers events newer than its start.
func Run(ctx context.Context, events Events, devices Devices, sender Sender) {
	wake, unsubscribe := events.SubscribeEvents()
	defer unsubscribe()
	lastID, since := latestID(events.GetEvents()), time.Now().UTC()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		for _, event := range events.EventsSince(lastID) {
			if id, _ := event["id"].(string); id != "" {
				lastID = id
			}
			if !newerThan(event, since) {
				continue
			}
			if frame, ok := Notification(event); ok {
				deliver(ctx, devices, sender, frame)
			}
		}
	}
}

func deliver(ctx context.Context, devices Devices, sender Sender, frame map[string]any) {
	registered, err := devices.ListPushDevices(ctx)
	if err != nil {
		log.Printf("push: list devices: %v", err)
		return
	}
	if len(registered) == 0 {
		return
	}
	targets := make([]map[string]any, 0, len(registered))
	for _, device := range registered {
		targets = append(targets, map[string]any{"token": device.Token, "environment": device.Environment})
	}
	frame["devices"] = targets
	if err := sender.Push(ctx, frame); err != nil {
		log.Printf("push: %s not sent: %v", frame["push_id"], err)
	}
}

// Notification maps a dashboard event to a push frame, or reports false when
// the event is not addressed to the human.
func Notification(event map[string]any) (map[string]any, bool) {
	kind := text(event, "type")
	if kind != "ask" && kind != "query" && kind != "notification" {
		return nil, false
	}
	if !humanTargets[strings.ToLower(strings.TrimPrefix(text(event, "to"), "@"))] {
		return nil, false
	}
	from := strings.TrimPrefix(text(event, "from"), "@")
	if from == "" {
		from = "repowire"
	}
	question, _ := event["question"].(map[string]any)
	title, category := "@"+from, "MESSAGE"
	switch {
	case question != nil && text(question, "scope") == "tool_permission":
		title, category = "@"+from+" needs approval", "APPROVAL"
	case kind != "notification":
		title, category = "@"+from+" asks", "QUESTION"
	}
	data := map[string]any{"event_id": text(event, "id"), "event_type": kind, "from": from}
	if cid := text(event, "correlation_id"); cid != "" {
		data["correlation_id"] = cid
	}
	if options, ok := question["options"].([]any); ok && len(options) > 0 {
		data["options"] = options
	}
	return map[string]any{
		"push_id":   text(event, "id"),
		"title":     title,
		"body":      truncate(text(event, "text"), maxBody),
		"thread_id": from,
		"category":  category,
		"data":      data,
	}, true
}

func latestID(events []map[string]any) string {
	if len(events) == 0 {
		return ""
	}
	return text(events[len(events)-1], "id")
}

func newerThan(event map[string]any, since time.Time) bool {
	stamp, err := time.Parse(time.RFC3339Nano, text(event, "timestamp"))
	return err != nil || !stamp.Before(since)
}

func text(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
