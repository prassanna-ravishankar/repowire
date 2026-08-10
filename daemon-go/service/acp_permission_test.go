package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestACPPermissionHandlerAllowsOnlyAnsweredOption(t *testing.T) {
	asks := NewAskTracker(time.Hour)
	requestIDs := make(chan string, 1)
	handler := NewACPPermissionHandler(asks, func(kind string, data map[string]any) {
		if kind == "acp_permission_request" {
			requestIDs <- data["request_id"].(string)
		}
	})
	done := make(chan map[string]any, 1)
	go func() {
		done <- handler(proto.PeerID("repow-test-acp"), json.RawMessage(`{"sessionId":"s1","toolCall":{"title":"Run shell"},"options":[{"optionId":"allow_once","name":"Allow once"}]}`))
	}()
	var cid string
	select {
	case cid = <-requestIDs:
	case <-time.After(time.Second):
		t.Fatal("permission ask not emitted")
	}
	option := "allow_once"
	if _, err := asks.Answer(context.Background(), cid, Answer{Outcome: "answered", OptionID: &option}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		outcome := result["outcome"].(map[string]any)
		if outcome["outcome"] != "selected" || outcome["optionId"] != option {
			t.Fatalf("unexpected ACP outcome: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("permission handler did not resolve")
	}
}
