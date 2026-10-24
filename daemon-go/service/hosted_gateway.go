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
// Hosted gateway: the outbound half of a hosted agent. The daemon holds one
// WebSocket to the runtime's gateway; a tool call the agent makes (notify a
// peer, open an ask, list peers) arrives as a call frame and runs as that
// hosted peer. The runtime side hibernates the socket, so an idle mesh costs
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

// gatewayFrame is both directions of the gateway protocol: a call from the
// runtime, or the daemon's result for it.
type gatewayFrame struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Agent string          `json:"agent,omitempty"`
	Tool  string          `json:"tool,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
	Text  string          `json:"text,omitempty"`
	Error bool            `json:"error,omitempty"`
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
	for {
		var frame gatewayFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return true, err
		}
		if frame.Type != "call" {
			continue
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			text, isError := tools.RunHostedTool(h.ctx, proto.PeerID(frame.Agent), frame.Tool, frame.Args)
			write.Lock()
			defer write.Unlock()
			_ = wsjson.Write(ctx, conn, gatewayFrame{Type: "result", ID: frame.ID, Text: text, Error: isError})
		}()
	}
}
