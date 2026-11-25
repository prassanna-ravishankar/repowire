// Package push renders the human inbox as APNs alerts on the registered
// installs of the iOS app. It is one of the inbox's local surfaces: the inbox
// decides what reaches the human and records each push's outcome; this
// package turns an item into an alert; the relay holds the APNs key and only
// sends.
package push

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/state"
)

// SurfaceName is the inbox surface name: traces, item push records, and
// human.surfaces.ios in config use it.
const SurfaceName = "ios"

const maxBody = 240

// Devices is the device-token store seam; *state.Store satisfies it.
type Devices interface {
	ListPushDevices(ctx context.Context) ([]state.PushDevice, error)
	DeletePushDevices(ctx context.Context, tokens ...string) (int, error)
}

// Relay is the relay client seam: whether it is up, and the push round trip.
type Relay interface {
	Status() relay.Status
	Push(ctx context.Context, frame map[string]any) (relay.PushResult, error)
}

// Surface is the iOS app as an inbox surface (service.LocalSurface).
type Surface struct {
	devices Devices
	relay   Relay
	preview bool
}

// NewSurface wires the surface to the device store and the relay client.
// preview (human.surfaces.ios.preview) puts message text in the alert.
func NewSurface(devices Devices, relay Relay, preview bool) *Surface {
	return &Surface{devices: devices, relay: relay, preview: preview}
}

// Name is the surface name the inbox records.
func (s *Surface) Name() string { return SurfaceName }

// Live reports whether a push could leave now: the relay is connected and at
// least one install is registered.
func (s *Surface) Live() bool {
	if !s.relay.Status().Connected {
		return false
	}
	devices, err := s.devices.ListPushDevices(context.Background())
	return err == nil && len(devices) > 0
}

// Push sends the item to every registered install and returns once the relay
// has APNs' answer. Tokens APNs reports dead are forgotten. The push fails when
// no install accepted it.
func (s *Surface) Push(ctx context.Context, item *state.InboxItem) error {
	devices, err := s.devices.ListPushDevices(ctx)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return errors.New("no registered devices")
	}
	frame := Alert(item, s.preview)
	targets := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		targets = append(targets, map[string]any{"token": device.Token, "environment": device.Environment})
	}
	frame["devices"] = targets
	result, err := s.relay.Push(ctx, frame)
	if len(result.InvalidTokens) > 0 {
		if _, dropErr := s.devices.DeletePushDevices(ctx, result.InvalidTokens...); dropErr != nil {
			log.Printf("push: drop dead tokens: %v", dropErr)
		}
		if err == nil && len(result.InvalidTokens) >= len(devices) {
			err = errors.New("every registered device token was rejected")
		}
	}
	return err
}

// Alert maps an inbox item to the relay's push frame. The app reads the item
// itself through GET /inbox/{id}; the alert carries what the lock screen shows
// and what its actions need.
//
// Without preview the alert is content-free: a title naming the sender and
// kind, no text, no question options, and the MESSAGE category, so approving
// or answering means opening the app and reading what is being approved.
func Alert(item *state.InboxItem, preview bool) map[string]any {
	from := strings.TrimPrefix(item.FromName, "@")
	if from == "" {
		from = "repowire"
	}
	title, category := "@"+from, "MESSAGE"
	switch {
	case item.Question != nil && text(item.Question, "scope") == "tool_permission":
		title, category = "@"+from+" needs approval", "APPROVAL"
	case item.Kind == "ask":
		title, category = "@"+from+" asks", "QUESTION"
	case item.Kind == "answer":
		title = "@" + from + " replied"
	}
	data := map[string]any{"item_id": item.ID, "kind": item.Kind, "from": from}
	if item.CorrelationID != "" {
		data["correlation_id"] = item.CorrelationID
	}
	if options, ok := item.Question["options"].([]any); ok && len(options) > 0 {
		data["options"] = options
	}
	thread := from
	if item.CorrelationID != "" {
		thread = item.CorrelationID
	}
	body := truncate(item.Text, maxBody)
	if !preview {
		body, category = "Open Repowire to read it.", "MESSAGE"
		delete(data, "options")
	}
	return map[string]any{
		"push_id":   item.ID,
		"title":     title,
		"body":      body,
		"thread_id": thread,
		"category":  category,
		"data":      data,
	}
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
