package relayserver

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
)

func TestTunnelDeadlineIsTheCallersCapped(t *testing.T) {
	s := New("")
	for header, want := range map[string]time.Duration{
		"":     defaultTunnelTimeout,
		"45":   45 * time.Second,
		"0.5":  500 * time.Millisecond,
		"90":   maxTunnelTimeout,
		"600":  maxTunnelTimeout,
		"1e30": maxTunnelTimeout, // never wraps into a negative duration
		"0":    defaultTunnelTimeout,
		"-5":   defaultTunnelTimeout,
		"soon": defaultTunnelTimeout,
		"NaN":  defaultTunnelTimeout,
	} {
		r := httptest.NewRequest(http.MethodPost, "/asks/x/wait", nil)
		if header != "" {
			r.Header.Set(TunnelTimeoutHeader, header)
		}
		if got := s.tunnelDeadline(r); got != want {
			t.Errorf("header %q: deadline %v, want %v", header, got, want)
		}
	}
}

// A long-poll that outlasts the default is answered when the caller states
// its deadline (#437), and the deadline reaches the daemon as timeout_ms; a
// caller that states none still gets the fast 504.
func TestTunnelHoldsALongPollForTheCallersDeadline(t *testing.T) {
	relay := New("")
	relay.timeout = 200 * time.Millisecond // stands in for the 30s default
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	key := "rw_test-connector-key"
	conn := connectDaemon(t, server.URL, key)

	frames := make(chan map[string]any, 4)
	go func() {
		for {
			var request map[string]any
			if wsjson.Read(context.Background(), conn, &request) != nil {
				return
			}
			frames <- request
			go func() {
				time.Sleep(500 * time.Millisecond) // the route waits longer than the default
				_ = wsjson.Write(context.Background(), conn, map[string]any{
					"type": "http_response", "request_id": request["request_id"], "status": 200,
					"headers": map[string]string{"Content-Type": "application/json"},
					"body":    base64.StdEncoding.EncodeToString([]byte(`{"status":"resolved"}`)),
				})
			}()
		}
	}()

	send := func(header string) int {
		req := cookieRequest(t, http.MethodPost, server.URL+"/asks/ask-1/wait", key, nil)
		if header != "" {
			req.Header.Set(TunnelTimeoutHeader, header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("2"); code != http.StatusOK {
		t.Fatalf("a long-poll with a 2s deadline: status %d, want 200", code)
	}
	if frame := <-frames; frame["timeout_ms"] != float64(2000) {
		t.Fatalf("frame timeout_ms = %v, want 2000", frame["timeout_ms"])
	}
	if code := send(""); code != http.StatusGatewayTimeout {
		t.Fatalf("no deadline: status %d, want 504 at the default", code)
	}
	if frame := <-frames; frame["timeout_ms"] != float64(200) {
		t.Fatalf("default frame timeout_ms = %v, want 200", frame["timeout_ms"])
	}
}

// The relay's own MCP tools tunnel daemon calls too: a machine's wait states
// that wait plus a margin as its deadline, and a call that does not wait keeps
// the default.
func TestDaemonMachineWaitStatesItsDeadline(t *testing.T) {
	relay := New("")
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	key := "rw_test-connector-key"
	conn := connectDaemonAs(t, server.URL, key, "m1")
	frames := make(chan map[string]any, 2)
	go func() {
		for {
			var request map[string]any
			if wsjson.Read(context.Background(), conn, &request) != nil {
				return
			}
			frames <- request
			_ = wsjson.Write(context.Background(), conn, map[string]any{
				"type": "http_response", "request_id": request["request_id"], "status": 200,
				"headers": map[string]string{}, "body": base64.StdEncoding.EncodeToString([]byte(`{}`)),
			})
		}
	}()
	grant := oauthGrant{UserID: "token-" + secretHash(key)}
	m, err := relay.machine(grant.UserID, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.wait(context.Background(), "relay-sender", "a", 15); err != nil {
		t.Fatal(err)
	}
	if frame := <-frames; frame["timeout_ms"] != float64((15*time.Second + tunnelWaitMargin).Milliseconds()) {
		t.Fatalf("waiting call: timeout_ms = %v", frame["timeout_ms"])
	}
	if _, err := m.agents(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if frame := <-frames; frame["timeout_ms"] != float64(defaultTunnelTimeout.Milliseconds()) {
		t.Fatalf("plain call: timeout_ms = %v", frame["timeout_ms"])
	}
}
