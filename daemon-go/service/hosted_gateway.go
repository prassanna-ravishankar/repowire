package service

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/repowire/repowire/daemon-go/proto"
)

// ============================================================================
// Hosted gateway: the daemon's one WebSocket to the runtime. Two frames
// arrive on it: a call (a mesh tool the agent invoked, run as that hosted
// peer and answered with a result), and settled (a run finished; the daemon
// settles it and acknowledges, and the runtime resends it on every connect
// until then). The runtime side hibernates the socket, so an idle mesh costs
// nothing and a sleeping agent stays asleep.
// ============================================================================

// Gateway timings: the keepalive ping (answered by the runtime without waking
// it) and the reconnect backoff bounds after a dropped connection.
const (
	gatewayPing       = 20 * time.Second
	gatewayDial       = 15 * time.Second
	gatewayBackoff    = time.Second
	gatewayBackoffMax = time.Minute
	gatewayReadLimit  = 1 << 20
)

// HostedToolRunner runs one mesh tool on behalf of a hosted peer and returns
// the text for the model, flagged when it is an error.
type HostedToolRunner interface {
	RunHostedTool(ctx context.Context, peerID proto.PeerID, tool string, args json.RawMessage) (text string, isError bool)
}

// gatewayFrame is every frame of the gateway protocol: call and settled from
// the runtime, result and ack from the daemon.
type gatewayFrame struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	Agent       string          `json:"agent,omitempty"`
	Tool        string          `json:"tool,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"`
	OperationID string          `json:"operationId,omitempty"`
	Status      string          `json:"status,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Text        string          `json:"text,omitempty"`
	Error       bool            `json:"error,omitempty"`
}

// Connect holds the gateway socket at base/gateway until Close, reconnecting
// after a drop. Calls are served while it is up; while it is down the
// runtime fails them loudly instead of queueing.
func (h *Hosted) Connect(base, token string, tools HostedToolRunner) {
	url := strings.TrimRight(base, "/") + "/gateway"
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		backoff := gatewayBackoff
		for h.ctx.Err() == nil {
			connected, err := h.serveGateway(url, token, tools)
			if h.ctx.Err() != nil {
				return
			}
			if connected {
				backoff = gatewayBackoff
			}
			log.Printf("hosted: gateway down, reconnecting in %s: %v", backoff, err)
			select {
			case <-h.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, gatewayBackoffMax)
		}
	}()
}

func (h *Hosted) serveGateway(url, token string, tools HostedToolRunner) (bool, error) {
	dial, cancel := context.WithTimeout(h.ctx, gatewayDial)
	conn, _, err := websocket.Dial(dial, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	cancel()
	if err != nil {
		return false, err
	}
	conn.SetReadLimit(gatewayReadLimit)
	defer conn.CloseNow()
	log.Printf("hosted: gateway connected")

	ctx, stop := context.WithCancel(h.ctx)
	defer stop()
	go func() {
		ticker := time.NewTicker(gatewayPing)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ping, cancel := context.WithTimeout(ctx, gatewayPing)
				err := conn.Ping(ping)
				cancel()
				if err != nil {
					conn.Close(websocket.StatusPolicyViolation, "ping timeout")
					return
				}
			}
		}
	}()

	var write sync.Mutex
	reply := func(frame gatewayFrame) {
		write.Lock()
		defer write.Unlock()
		_ = wsjson.Write(ctx, conn, frame)
	}
	for {
		var frame gatewayFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return true, err
		}
		if frame.Type != "call" && frame.Type != "settled" {
			continue
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			if frame.Type == "settled" {
				h.Report(h.ctx, frame.Agent, frame.OperationID, HostedResult{Status: frame.Status, Reason: frame.Reason, Text: frame.Text})
				reply(gatewayFrame{Type: "ack", ID: frame.ID})
				return
			}
			text, isError := tools.RunHostedTool(h.ctx, proto.PeerID(frame.Agent), frame.Tool, frame.Args)
			reply(gatewayFrame{Type: "result", ID: frame.ID, Text: text, Error: isError})
		}()
	}
}
