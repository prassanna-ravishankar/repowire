package service

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ============================================================================
// Hosted gateway: the daemon's one WebSocket to the runtime. A settled frame
// arrives on it when a run finishes; the daemon settles it and acknowledges,
// and the runtime resends it on every connect until then. The runtime side hibernates the socket, so an idle mesh costs
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

// gatewayFrame is every frame of the gateway protocol: settled from the
// runtime, ack from the daemon.
type gatewayFrame struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Agent       string `json:"agent,omitempty"`
	OperationID string `json:"operationId,omitempty"`
	Status      string `json:"status,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Text        string `json:"text,omitempty"`
}

// Connect holds the gateway socket at base/gateway until Close, reconnecting
// after a drop. Reports wait in the runtime while it is down.
func (h *Hosted) Connect(base, token string) {
	url := strings.TrimRight(base, "/") + "/gateway"
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		backoff := gatewayBackoff
		for h.ctx.Err() == nil {
			connected, err := h.serveGateway(url, token)
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

func (h *Hosted) serveGateway(url, token string) (bool, error) {
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
		if frame.Type != "settled" {
			continue
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.Report(h.ctx, frame.Agent, frame.OperationID, HostedResult{Status: frame.Status, Reason: frame.Reason, Text: frame.Text})
			reply(gatewayFrame{Type: "ack", ID: frame.ID})
		}()
	}
}
