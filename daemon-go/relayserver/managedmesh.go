package relayserver

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The managed machine in the cross-machine protocol
// (rfcs/managed-in-the-mesh.md). Daemons reach managed agents through
// machine_request to "managed", which the relay forwards to the Worker's
// /remote API. The Worker reaches daemons through two control-token routes,
// and its requests carry from_machine "managed", which nothing else can
// obtain.

const (
	managedMeshPerOwner = 16
	managedMeshGlobal   = 256
	managedMeshMaxWait  = 25 * time.Second
	managedMeshMaxBody  = 1 << 20
)

// remote forwards one daemon's /remote request to the Worker. A transport
// failure or timeout means the outcome is unknown; it is never retried.
func (m *managedWorker) remote(ctx context.Context, owner, fromMachine, method, route string, body []byte, deadline time.Time) (int, []byte) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, m.url(route, ""), strings.NewReader(string(body)))
	if err != nil {
		return http.StatusBadRequest, errorBody(err.Error())
	}
	req.Header = m.header(owner)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Repowire-From-Machine", fromMachine)
	req.Header.Set("X-Repowire-Deadline", strconv.FormatInt(deadline.UnixMilli(), 10))
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return http.StatusGatewayTimeout, errorBody("managed did not respond in time")
		}
		return http.StatusBadGateway, errorBody("failed to reach managed")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, managedMeshMaxBody))
	if err != nil {
		return http.StatusBadGateway, errorBody("managed response was cut off")
	}
	return resp.StatusCode, out
}

// meshSlots bounds Worker-originated requests: per owner and overall. A slot
// belongs to one HTTP request and is released when it ends.
type meshSlots struct {
	mu     sync.Mutex
	owners map[string]int
	total  int
}

func (q *meshSlots) take(owner string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.total >= managedMeshGlobal || q.owners[owner] >= managedMeshPerOwner {
		return false
	}
	if q.owners == nil {
		q.owners = map[string]int{}
	}
	q.owners[owner]++
	q.total++
	return true
}

func (q *meshSlots) release(owner string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.total--
	if q.owners[owner]--; q.owners[owner] <= 0 {
		delete(q.owners, owner)
	}
}

func (s *Server) managedMeshRoutes() {
	s.mux.HandleFunc("POST "+managedPrefix+"/machines", s.workerOnly(s.workerMachines))
	s.mux.HandleFunc("POST "+managedPrefix+"/machine-request", s.workerOnly(s.workerMachineRequest))
}

// workerOnly admits the Worker's control token and nothing else: no cookie,
// no relay key. The owner is the X-Repowire-Owner header.
func (s *Server) workerOnly(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.worker == nil {
			writeError(w, http.StatusServiceUnavailable, "managed agents are not configured on this relay")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.worker.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "control token required")
			return
		}
		owner := strings.TrimSpace(r.Header.Get("X-Repowire-Owner"))
		if owner == "" {
			writeError(w, http.StatusBadRequest, "X-Repowire-Owner is required")
			return
		}
		// An owner taken off the list keeps no path to its daemons.
		if _, err := s.managedFor(owner); err != nil {
			writeError(w, managedStatus(err), err.Error())
			return
		}
		h(w, r, owner)
	}
}

func (s *Server) workerMachines(w http.ResponseWriter, _ *http.Request, owner string) {
	writeJSON(w, http.StatusOK, map[string]any{"machines": s.onlineDaemons(owner, "")})
}

// workerMachineRequest re-issues one Worker request to a daemon as
// remote_request from "managed" and answers with the daemon's status and
// body. Relay-made refusals carry X-Repowire-Relay so the Worker can tell
// them from the daemon's.
func (s *Server) workerMachineRequest(w http.ResponseWriter, r *http.Request, owner string) {
	var req struct {
		Machine  string          `json:"machine"`
		Method   string          `json:"method"`
		Path     string          `json:"path"`
		Body     json.RawMessage `json:"body"`
		Deadline int64           `json:"deadline"`
	}
	refuse := func(status int, detail string) {
		w.Header().Set("X-Repowire-Relay", "1")
		writeError(w, status, detail)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, managedMeshMaxBody)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !remotePath(req.Path) {
		refuse(http.StatusForbidden, "only /remote/ paths cross machines")
		return
	}
	if req.Method != http.MethodPost {
		refuse(http.StatusMethodNotAllowed, "remote requests are POST")
		return
	}
	target := s.daemon(owner, req.Machine)
	if req.Machine == managedMachineID || target == nil {
		refuse(http.StatusNotFound, "machine offline")
		return
	}
	if !s.meshSlots.take(owner) {
		refuse(http.StatusTooManyRequests, "overloaded, not processed")
		return
	}
	defer s.meshSlots.release(owner)
	deadline := time.Now().Add(managedMeshMaxWait)
	if asked := time.UnixMilli(req.Deadline); req.Deadline > 0 && asked.Before(deadline) {
		deadline = asked
	}
	status, body := s.remoteRequest(r.Context(), target, managedMachineID, req.Method, req.Path, base64.StdEncoding.EncodeToString(req.Body), deadline)
	if status == http.StatusBadGateway || status == http.StatusGatewayTimeout {
		w.Header().Set("X-Repowire-Relay", "1")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
