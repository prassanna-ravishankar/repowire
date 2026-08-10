package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

func TestACPManagerPromptRoundTripAndSessionReuse(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	script := filepath.Join(t.TempDir(), "echo_acp.py")
	source := `import json, sys
session = "echo-session"
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method")
    if method == "initialize": result = {"protocolVersion": 1, "agentCapabilities": {}}
    elif method == "session/new": result = {"sessionId": session}
    elif method == "session/prompt":
        text = msg["params"]["prompt"][0]["text"]
        print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":session,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"[echo] "+text}}}}), flush=True)
        result = {"stopReason":"end_turn"}
    elif method == "session/close": result = {}
    elif method == "session/cancel": continue
    else: result = {}
    print(json.dumps({"jsonrpc":"2.0","id":msg["id"],"result":result}), flush=True)
`
	if err := os.WriteFile(script, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewACPManager()
	defer manager.Close()
	spec := ACPPeerSpec{PeerID: "repow-test-acp", Command: python, Args: []string{script}, CWD: t.TempDir()}
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
