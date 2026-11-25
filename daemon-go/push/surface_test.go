package push

import (
	"context"
	"errors"
	"testing"

	"github.com/repowire/repowire/daemon-go/relay"
	"github.com/repowire/repowire/daemon-go/state"
)

type fakeDevices struct {
	devices []state.PushDevice
	deleted []string
}

func (f *fakeDevices) ListPushDevices(context.Context) ([]state.PushDevice, error) {
	return append([]state.PushDevice(nil), f.devices...), nil
}

func (f *fakeDevices) DeletePushDevices(_ context.Context, tokens ...string) (int, error) {
	f.deleted = append(f.deleted, tokens...)
	kept := f.devices[:0]
	for _, d := range f.devices {
		dead := false
		for _, t := range tokens {
			dead = dead || d.Token == t
		}
		if !dead {
			kept = append(kept, d)
		}
	}
	f.devices = kept
	return len(tokens), nil
}

type fakeRelay struct {
	connected bool
	frames    []map[string]any
	result    relay.PushResult
	err       error
}

func (f *fakeRelay) Status() relay.Status { return relay.Status{Connected: f.connected} }

func (f *fakeRelay) Push(_ context.Context, frame map[string]any) (relay.PushResult, error) {
	f.frames = append(f.frames, frame)
	return f.result, f.err
}

func TestAlertMapsInboxItems(t *testing.T) {
	approval := Alert(&state.InboxItem{
		ID: "inb-1", Kind: "ask", FromName: "backend", CorrelationID: "ask-1", Text: "Run rm -rf build?",
		Question: map[string]any{"scope": "tool_permission", "options": []any{map[string]any{"id": "allow", "title": "Allow"}}},
	})
	if approval["category"] != "APPROVAL" || approval["title"] != "@backend needs approval" || approval["push_id"] != "inb-1" || approval["thread_id"] != "ask-1" {
		t.Fatalf("approval = %v", approval)
	}
	if data := approval["data"].(map[string]any); data["item_id"] != "inb-1" || data["correlation_id"] != "ask-1" || data["options"] == nil {
		t.Fatalf("approval data = %v", data)
	}
	for _, tc := range []struct {
		item            state.InboxItem
		title, category string
	}{
		{state.InboxItem{ID: "inb-2", Kind: "ask", FromName: "@ci", Text: "ship?"}, "@ci asks", "QUESTION"},
		{state.InboxItem{ID: "inb-3", Kind: "notify", FromName: "ci", Text: "deploy done"}, "@ci", "MESSAGE"},
		{state.InboxItem{ID: "inb-4", Kind: "answer", FromName: "ci", CorrelationID: "ask-9", Text: "done"}, "@ci replied", "MESSAGE"},
		{state.InboxItem{ID: "inb-5", Kind: "broadcast", Text: "hello"}, "@repowire", "MESSAGE"},
	} {
		got := Alert(&tc.item)
		if got["title"] != tc.title || got["category"] != tc.category {
			t.Errorf("%s: title %v category %v", tc.item.ID, got["title"], got["category"])
		}
	}
	long := make([]rune, 500)
	for i := range long {
		long[i] = 'x'
	}
	if body := []rune(Alert(&state.InboxItem{Text: string(long)})["body"].(string)); len(body) != maxBody {
		t.Fatalf("body length = %d, want %d", len(body), maxBody)
	}
}

func TestSurfaceLiveNeedsRelayAndADevice(t *testing.T) {
	devices, relayFake := &fakeDevices{}, &fakeRelay{connected: true}
	s := NewSurface(devices, relayFake)
	if s.Name() != "ios" || s.Live() {
		t.Fatalf("no devices: name %q live %v", s.Name(), s.Live())
	}
	devices.devices = []state.PushDevice{{Token: "aa", Environment: "sandbox"}}
	if !s.Live() {
		t.Fatal("a registered device with the relay up should be live")
	}
	relayFake.connected = false
	if s.Live() {
		t.Fatal("relay down should not be live")
	}
}

func TestSurfacePushSendsToEveryDeviceAndForgetsDeadTokens(t *testing.T) {
	devices := &fakeDevices{devices: []state.PushDevice{{Token: "live", Environment: "production"}, {Token: "dead", Environment: "sandbox"}}}
	relayFake := &fakeRelay{connected: true, result: relay.PushResult{InvalidTokens: []string{"dead"}}}
	s := NewSurface(devices, relayFake)

	if err := s.Push(context.Background(), &state.InboxItem{ID: "inb-1", Kind: "notify", FromName: "ci", Text: "hi"}); err != nil {
		t.Fatalf("push with one live device = %v", err)
	}
	if len(relayFake.frames) != 1 || len(relayFake.frames[0]["devices"].([]map[string]any)) != 2 {
		t.Fatalf("frames = %v", relayFake.frames)
	}
	if len(devices.deleted) != 1 || devices.deleted[0] != "dead" {
		t.Fatalf("deleted = %v", devices.deleted)
	}

	relayFake.result = relay.PushResult{InvalidTokens: []string{"live"}}
	if err := s.Push(context.Background(), &state.InboxItem{ID: "inb-2"}); err == nil {
		t.Fatal("a push every device rejected must fail")
	}
	if err := s.Push(context.Background(), &state.InboxItem{ID: "inb-3"}); err == nil {
		t.Fatal("a push with no devices must fail")
	}
}

func TestSurfacePushReportsRelayErrors(t *testing.T) {
	devices := &fakeDevices{devices: []state.PushDevice{{Token: "t", Environment: "production"}}}
	s := NewSurface(devices, &fakeRelay{err: relay.ErrRelayDown})
	if err := s.Push(context.Background(), &state.InboxItem{ID: "inb-1"}); !errors.Is(err, relay.ErrRelayDown) {
		t.Fatalf("err = %v", err)
	}
}
