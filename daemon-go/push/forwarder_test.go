package push

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/state"
)

func TestNotificationMapsHumanEvents(t *testing.T) {
	approval, ok := Notification(map[string]any{
		"id": "e1", "type": "ask", "from": "backend", "to": "human", "text": "Run rm -rf build?", "correlation_id": "ask-1",
		"question": map[string]any{"scope": "tool_permission", "options": []any{map[string]any{"id": "allow", "title": "Allow"}}},
	})
	if !ok || approval["category"] != "APPROVAL" || approval["title"] != "@backend needs approval" {
		t.Fatalf("approval = %v", approval)
	}
	if data := approval["data"].(map[string]any); data["correlation_id"] != "ask-1" || data["options"] == nil {
		t.Fatalf("approval data = %v", data)
	}
	note, ok := Notification(map[string]any{"id": "e2", "type": "notification", "from": "@ci", "to": "@dashboard", "text": "deploy done"})
	if !ok || note["category"] != "MESSAGE" || note["title"] != "@ci" {
		t.Fatalf("notification = %v", note)
	}
	for _, event := range []map[string]any{
		{"type": "notification", "from": "a", "to": "worker"},
		{"type": "chat_turn", "to": "human"},
		{"type": "ask", "to": "telegram"},
	} {
		if frame, ok := Notification(event); ok {
			t.Fatalf("event %v should not push: %v", event, frame)
		}
	}
}

type fakeEvents struct {
	mu     sync.Mutex
	events []map[string]any
	wake   chan struct{}
}

func (f *fakeEvents) SubscribeEvents() (<-chan struct{}, func()) { return f.wake, func() {} }
func (f *fakeEvents) GetEvents() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.events...)
}
func (f *fakeEvents) EventsSince(id string) []map[string]any {
	all := f.GetEvents()
	for i, event := range all {
		if event["id"] == id {
			return all[i+1:]
		}
	}
	return all
}
func (f *fakeEvents) add(event map[string]any) {
	f.mu.Lock()
	f.events = append(f.events, event)
	f.mu.Unlock()
	f.wake <- struct{}{}
}

type fakeDevices []state.PushDevice

func (f fakeDevices) ListPushDevices(context.Context) ([]state.PushDevice, error) { return f, nil }

type fakeSender chan map[string]any

func (f fakeSender) Push(_ context.Context, frame map[string]any) error {
	f <- frame
	return nil
}

func TestRunPushesOnlyNewHumanEvents(t *testing.T) {
	old := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	events := &fakeEvents{wake: make(chan struct{}, 1), events: []map[string]any{
		{"id": "old", "type": "ask", "to": "human", "from": "a", "timestamp": old},
	}}
	sender := make(fakeSender, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, events, fakeDevices{{Token: "tok", Environment: "sandbox"}}, sender)

	time.Sleep(20 * time.Millisecond)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	events.add(map[string]any{"id": "skip", "type": "notification", "to": "worker", "from": "a", "timestamp": now})
	events.add(map[string]any{"id": "new", "type": "ask", "to": "human", "from": "b", "text": "ok?", "timestamp": now})

	select {
	case frame := <-sender:
		devices := frame["devices"].([]map[string]any)
		if frame["push_id"] != "new" || devices[0]["token"] != "tok" {
			t.Fatalf("frame = %v", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push sent")
	}
	select {
	case frame := <-sender:
		t.Fatalf("unexpected extra push %v", frame)
	case <-time.After(50 * time.Millisecond):
	}
}
