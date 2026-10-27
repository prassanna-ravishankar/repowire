package relayserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cross-machine requests (rfcs/cross-machine-circles.md). A daemon sends
// machine_request naming another of its owner's machines; the relay re-issues
// it to that machine as remote_request under a relay-minted id, with
// from_machine taken from the sending connection, and answers the source with
// machine_response. Only /remote/ paths cross, and the relay never interprets
// the body.

const (
	machineWorkers = 16
	machineQueue   = 64
)

type machineWork struct {
	s      *Server
	src    *daemonConn
	queue  chan map[string]any
	mu     sync.Mutex
	active map[string]bool
}

// startMachineWorkers runs a bounded pool for one connection's
// machine_requests, so the connection's read loop never blocks on one.
func (s *Server) startMachineWorkers(ctx context.Context, src *daemonConn) *machineWork {
	w := &machineWork{s: s, src: src, queue: make(chan map[string]any, machineQueue), active: map[string]bool{}}
	for range machineWorkers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-w.queue:
					w.run(ctx, msg)
				}
			}
		}()
	}
	return w
}

// admit queues a request without blocking; a full queue is refused at once.
func (w *machineWork) admit(ctx context.Context, msg map[string]any) {
	id := stringValue(msg["id"])
	if id == "" {
		return
	}
	w.mu.Lock()
	duplicate := w.active[id]
	if !duplicate {
		w.active[id] = true
	}
	w.mu.Unlock()
	if duplicate {
		w.respond(ctx, id, http.StatusConflict, errorBody("request id already in flight"), false)
		return
	}
	select {
	case w.queue <- msg:
	default:
		w.respond(ctx, id, http.StatusTooManyRequests, errorBody("overloaded, not processed"), true)
	}
}

func (w *machineWork) respond(ctx context.Context, id string, status int, body []byte, release bool) {
	if release {
		w.mu.Lock()
		delete(w.active, id)
		w.mu.Unlock()
	}
	_ = w.src.write(ctx, map[string]any{"type": "machine_response", "id": id, "status": status, "body": base64.StdEncoding.EncodeToString(body)})
}

func (w *machineWork) run(ctx context.Context, msg map[string]any) {
	id := stringValue(msg["id"])
	status, body := w.forward(ctx, msg)
	w.respond(ctx, id, status, body, true)
}

func (w *machineWork) forward(ctx context.Context, msg map[string]any) (int, []byte) {
	machine, route := stringValue(msg["machine"]), stringValue(msg["path"])
	if machine == "" || machine == w.src.daemonID {
		return http.StatusBadRequest, errorBody("machine must name another machine")
	}
	if !remotePath(route) {
		return http.StatusForbidden, errorBody("only /remote/ paths cross machines")
	}
	deadline := time.Now().Add(w.s.timeout)
	if machine == managedMachineID && w.s.worker != nil {
		body, err := base64.StdEncoding.DecodeString(stringValue(msg["body"]))
		if err != nil {
			return http.StatusBadRequest, errorBody("body must be base64")
		}
		return w.s.worker.remote(ctx, w.src.userID, w.src.daemonID, stringValue(msg["method"]), route, body, deadline)
	}
	target := w.s.daemon(w.src.userID, machine)
	if target == nil {
		return http.StatusNotFound, errorBody("machine offline")
	}
	return w.s.remoteRequest(ctx, target, w.src.daemonID, stringValue(msg["method"]), route, stringValue(msg["body"]), deadline)
}

// remoteRequest re-issues one /remote request to target under a relay-minted
// id and waits for that connection's answer until deadline or ctx ends.
func (s *Server) remoteRequest(ctx context.Context, target *daemonConn, fromMachine, method, route, body string, deadline time.Time) (int, []byte) {
	id := randomID("mreq_", 16)
	ch := s.addPending(id, target)
	defer s.removePending(id)
	out := map[string]any{
		"type": "remote_request", "request_id": id, "from_machine": fromMachine,
		"method": method, "path": route, "body": body, "deadline": deadline.UnixMilli(),
	}
	if err := target.write(ctx, out); err != nil {
		return http.StatusBadGateway, errorBody("failed to reach machine")
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case response := <-ch:
		if stringValue(response["type"]) != "http_response" {
			return http.StatusBadGateway, errorBody("machine disconnected")
		}
		body, _ := base64.StdEncoding.DecodeString(stringValue(response["body"]))
		return intValue(response["status"], http.StatusOK), body
	case <-timer.C:
		return http.StatusGatewayTimeout, errorBody("machine did not respond in time")
	case <-ctx.Done():
		return http.StatusBadGateway, errorBody("source disconnected")
	}
}

// answerMachineList names the owner's other online machines.
// With the managed service configured, managed is always one of them.
func (s *Server) answerMachineList(ctx context.Context, src *daemonConn, msg map[string]any) {
	machines := s.onlineDaemons(src.userID, src.daemonID)
	if s.worker != nil {
		machines = append(machines, managedMachineID)
		sort.Strings(machines)
	}
	body, _ := json.Marshal(map[string]any{"machines": machines})
	_ = src.write(ctx, map[string]any{"type": "machine_response", "id": stringValue(msg["id"]), "status": http.StatusOK, "body": base64.StdEncoding.EncodeToString(body)})
}

// onlineDaemons names the owner's connected daemons, except one.
func (s *Server) onlineDaemons(userID, except string) []string {
	machines := []string{}
	for _, conn := range s.allDaemons(userID) {
		if conn.daemonID != except {
			machines = append(machines, conn.daemonID)
		}
	}
	sort.Strings(machines)
	return machines
}

// remotePath accepts only a clean, unescaped path under /remote/.
func remotePath(route string) bool {
	return strings.HasPrefix(route, "/remote/") && !strings.ContainsAny(route, "%?#\\") && path.Clean(route) == route
}

func errorBody(detail string) []byte {
	body, _ := json.Marshal(map[string]string{"detail": detail})
	return body
}
