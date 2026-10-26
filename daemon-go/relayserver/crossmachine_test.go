package relayserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/relay"
)

func connectMachine(t *testing.T, ctx context.Context, f *authFixture, id string, handler http.Handler) *relay.Client {
	t.Helper()
	unused := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(unused.Close)
	c := relay.NewClient("ws"+strings.TrimPrefix(f.http.URL, "http"), f.key, id, unused.URL).WithRemoteHandler(handler)
	c.Start(ctx)
	t.Cleanup(c.Stop)
	deadline := time.Now().Add(3 * time.Second)
	for !c.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.Status().Connected {
		t.Fatalf("%s did not connect", id)
	}
	return c
}

// The target sees the source machine the relay names, never one the source
// claims, and only /remote/ paths cross.
func TestMachineRequestCrossesWithRelayNamedSource(t *testing.T) {
	f := newAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{"from": relay.RemoteMachine(r.Context()), "path": r.URL.Path, "body": string(body)})
	})
	a := connectMachine(t, ctx, f, "alpha", echo)
	connectMachine(t, ctx, f, "beta", echo)

	status, body, err := a.MachineRequest(ctx, "beta", http.MethodPost, "/remote/ask", []byte(`{"x":1}`))
	if err != nil || status != 200 {
		t.Fatalf("status %d err %v body %s", status, err, body)
	}
	var got map[string]string
	_ = json.Unmarshal(body, &got)
	if got["from"] != "alpha" || got["path"] != "/remote/ask" || got["body"] != `{"x":1}` {
		t.Fatalf("got %v", got)
	}

	for _, tc := range []struct {
		machine, path string
		status        int
	}{
		{"beta", "/ask", http.StatusForbidden},
		{"beta", "/remote/../ask", http.StatusForbidden},
		{"beta", "/remote/%61sk", http.StatusForbidden},
		{"alpha", "/remote/ask", http.StatusBadRequest},
		{"gamma", "/remote/ask", http.StatusNotFound},
	} {
		status, _, err := a.MachineRequest(ctx, tc.machine, http.MethodPost, tc.path, nil)
		if err != nil || status != tc.status {
			t.Fatalf("%s %s: status %d err %v, want %d", tc.machine, tc.path, status, err, tc.status)
		}
	}

	// The fixture also registers "laptop"; the caller is never listed.
	machines, err := a.Machines(ctx)
	if err != nil || strings.Join(machines, ",") != "beta,laptop" {
		t.Fatalf("machines %v err %v", machines, err)
	}
}

// A response from a connection the request was not sent to is ignored.
func TestRelayResponsesBindToTheirTargetConnection(t *testing.T) {
	f := newAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	slow := make(chan struct{})
	connectMachine(t, ctx, f, "beta", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-slow
		_, _ = io.WriteString(w, `{"from":"beta"}`)
	}))
	a := connectMachine(t, ctx, f, "alpha", http.NotFoundHandler())

	// An impostor connection on the same owner answers every request id it
	// can guess; none of its frames may complete the request.
	impostor, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.http.URL, "http")+"/ws/relay?api_key="+f.key+"&daemon_id=impostor", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer impostor.CloseNow()

	done := make(chan []byte, 1)
	go func() {
		_, body, _ := a.MachineRequest(ctx, "beta", http.MethodPost, "/remote/peers", nil)
		done <- body
	}()
	time.Sleep(100 * time.Millisecond)
	f.s.mu.Lock()
	ids := []string{}
	for id := range f.s.pending {
		ids = append(ids, id)
	}
	f.s.mu.Unlock()
	for _, id := range ids {
		_ = wsjson.Write(ctx, impostor, map[string]any{"type": "http_response", "request_id": id, "status": 200, "body": "eyJmcm9tIjoiaW1wb3N0b3IifQ=="})
	}
	time.Sleep(100 * time.Millisecond)
	close(slow)
	if body := <-done; !strings.Contains(string(body), "beta") {
		t.Fatalf("response %s", body)
	}
}
