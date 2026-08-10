package service

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestACPManagerPromptRoundTripAndSessionReuse(t *testing.T) {
	manager := NewACPManager()
	defer manager.Close()
	spec := ACPPeerSpec{
		PeerID: "repow-test-acp", Command: os.Args[0], Args: []string{"-test.run=^TestACPHelperProcess$"},
		CWD: t.TempDir(), Env: map[string]string{"REPOWIRE_ACP_TEST_HELPER": "1"},
	}
	for _, prompt := range []string{"one", "two"} {
		done := make(chan struct {
			result ACPPromptResult
			err    error
		}, 1)
		if err := manager.Prompt(spec, prompt, func(result ACPPromptResult, err error) {
			done <- struct {
				result ACPPromptResult
				err    error
			}{result, err}
		}); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.result.Text != "[echo] "+prompt || got.result.StopReason != "end_turn" {
				t.Fatalf("unexpected result: %#v", got.result)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ACP prompt timed out")
		}
	}
}

func TestACPHelperProcess(t *testing.T) {
	if os.Getenv("REPOWIRE_ACP_TEST_HELPER") != "1" {
		return
	}
	type request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message request
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		var result any = map[string]any{}
		switch message.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}}
		case "session/new":
			result = map[string]any{"sessionId": "echo-session"}
		case "session/prompt":
			text := message.Params.Prompt[0].Text
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "echo-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "[echo] " + text}}}}); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{"stopReason": "end_turn"}
		case "session/cancel":
			continue
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result}); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestACPRouteRequiresFlagAndValidMetadata(t *testing.T) {
	transport := NewWebSocketTransport()
	peer := &proto.Peer{PeerID: "repow-test-acp", Path: "/tmp", Metadata: map[string]any{"acp": map[string]any{"command": "agent", "args": []any{"--stdio"}}}}
	if _, ok := transport.ACPRoute(peer); ok {
		t.Fatal("ACP routed while disabled")
	}
	transport.EnableACP(true)
	defer transport.CloseACP()
	decision, ok := transport.ACPRoute(peer)
	if !ok || decision.Spec.Command != "agent" || len(decision.Spec.Args) != 1 {
		t.Fatalf("unexpected decision: %#v, %v", decision, ok)
	}
}
