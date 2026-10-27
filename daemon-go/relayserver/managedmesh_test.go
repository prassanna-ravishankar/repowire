package relayserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/relay"
)

func managedMesh(t *testing.T) (*authFixture, *fakeWorker) {
	t.Helper()
	f := newAuthFixture(t)
	worker := newFakeWorker(t)
	if err := f.s.ConfigureManaged(ManagedConfig{URL: worker.URL, ControlToken: controlToken}); err != nil {
		t.Fatal(err)
	}
	return f, worker
}

func workerPost(t *testing.T, f *authFixture, path, token, owner, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+managedPrefix+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if owner != "" {
		req.Header.Set("X-Repowire-Owner", owner)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res, string(out)
}

// A daemon reaches managed like any machine: listed, and its /remote request
// forwarded to the Worker naming the owner and the sending machine.
func TestDaemonReachesTheManagedMachine(t *testing.T) {
	f, worker := managedMesh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	alpha := connectMachine(t, ctx, f, "alpha", http.NotFoundHandler())

	machines, err := alpha.Machines(ctx)
	if err != nil || strings.Join(machines, ",") != "laptop,managed" {
		t.Fatalf("machines %v %v", machines, err)
	}
	status, body, err := alpha.MachineRequest(ctx, "managed", http.MethodPost, "/remote/peers", []byte(`{"from":{}}`))
	if err != nil || status != 200 || !strings.Contains(string(body), `"/remote/peers"`) {
		t.Fatalf("forward %d %s %v", status, body, err)
	}
	req, sent := worker.last()
	owner, _ := f.s.tokens.validate(f.key)
	if req.Header.Get("X-Repowire-From-Machine") != "alpha" || req.Header.Get("X-Repowire-Owner") != owner.UserID || req.Header.Get("X-Repowire-Deadline") == "" || sent != `{"from":{}}` {
		t.Fatalf("worker saw %v %q", req.Header, sent)
	}
	if status, _, _ := alpha.MachineRequest(ctx, "managed", http.MethodPost, "/agents", nil); status != http.StatusForbidden {
		t.Fatalf("non-remote path to managed %d", status)
	}
}

// The Worker reaches daemons only with the control token, always as
// "managed", and only an online daemon of the named owner.
func TestWorkerReachesDaemonsAsManaged(t *testing.T) {
	f, _ := managedMesh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connectMachine(t, ctx, f, "beta", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"from": relay.RemoteMachine(r.Context()), "path": r.URL.Path, "body": string(body)})
	}))
	owner, _ := f.s.tokens.validate(f.key)

	res, listed := workerPost(t, f, "/machines", controlToken, owner.UserID, "")
	if res.StatusCode != 200 || !strings.Contains(listed, `"beta"`) || strings.Contains(listed, "managed") {
		t.Fatalf("machines %d %s", res.StatusCode, listed)
	}

	res, out := workerPost(t, f, "/machine-request", controlToken, owner.UserID, `{"machine":"beta","method":"POST","path":"/remote/ask","body":{"x":1}}`)
	var got map[string]string
	_ = json.Unmarshal([]byte(out), &got)
	if res.StatusCode != http.StatusCreated || got["from"] != "managed" || got["path"] != "/remote/ask" || got["body"] != `{"x":1}` || res.Header.Get("X-Repowire-Relay") != "" {
		t.Fatalf("forward %d %s %v", res.StatusCode, out, res.Header)
	}

	for _, tc := range []struct {
		name, token, owner, body string
		status                   int
	}{
		{"no token", "", owner.UserID, `{"machine":"beta","method":"POST","path":"/remote/ask"}`, http.StatusUnauthorized},
		{"relay key", f.key, owner.UserID, `{"machine":"beta","method":"POST","path":"/remote/ask"}`, http.StatusUnauthorized},
		{"no owner", controlToken, "", `{"machine":"beta","method":"POST","path":"/remote/ask"}`, http.StatusBadRequest},
		{"other owner", controlToken, "token-someone-else", `{"machine":"beta","method":"POST","path":"/remote/ask"}`, http.StatusNotFound},
		{"itself", controlToken, owner.UserID, `{"machine":"managed","method":"POST","path":"/remote/ask"}`, http.StatusNotFound},
		{"offline", controlToken, owner.UserID, `{"machine":"gamma","method":"POST","path":"/remote/ask"}`, http.StatusNotFound},
		{"not remote", controlToken, owner.UserID, `{"machine":"beta","method":"POST","path":"/ask"}`, http.StatusForbidden},
		{"escape", controlToken, owner.UserID, `{"machine":"beta","method":"POST","path":"/remote/../ask"}`, http.StatusForbidden},
		{"method", controlToken, owner.UserID, `{"machine":"beta","method":"GET","path":"/remote/ask"}`, http.StatusMethodNotAllowed},
	} {
		if res, out := workerPost(t, f, "/machine-request", tc.token, tc.owner, tc.body); res.StatusCode != tc.status {
			t.Fatalf("%s: %d %s, want %d", tc.name, res.StatusCode, out, tc.status)
		}
	}
}

func TestMeshSlotsBoundEachOwnerAndTheRelay(t *testing.T) {
	var q meshSlots
	for range managedMeshPerOwner {
		if !q.take("a") {
			t.Fatal("slot refused under the owner cap")
		}
	}
	if q.take("a") {
		t.Fatal("owner cap not enforced")
	}
	q.release("a")
	if !q.take("a") {
		t.Fatal("released slot not reusable")
	}
	for i := 0; q.total < managedMeshGlobal; i++ {
		_ = q.take("owner-" + string(rune('A'+i%26)) + string(rune('a'+i/26)))
	}
	if q.take("fresh") {
		t.Fatal("global cap not enforced")
	}
}

// A target whose socket is held by another writer cannot keep a Worker
// request, or its slot, past the deadline.
func TestWorkerRequestDeadlineBoundsABlockedWrite(t *testing.T) {
	f, _ := managedMesh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connectMachine(t, ctx, f, "beta", http.NotFoundHandler())
	owner, _ := f.s.tokens.validate(f.key)
	conn := f.s.daemon(owner.UserID, "beta")
	conn.writeOnce.Do(func() { conn.writeSlot = make(chan struct{}, 1) })
	conn.writeSlot <- struct{}{}
	defer func() { <-conn.writeSlot }()

	started := time.Now()
	deadline := started.Add(300 * time.Millisecond).UnixMilli()
	res, out := workerPost(t, f, "/machine-request", controlToken, owner.UserID, `{"machine":"beta","method":"POST","path":"/remote/ask","body":{},"deadline":`+strconv.FormatInt(deadline, 10)+`}`)
	if res.StatusCode != http.StatusGatewayTimeout || time.Since(started) > 2*time.Second || res.Header.Get("X-Repowire-Relay") != "1" {
		t.Fatalf("blocked write %d %s after %v", res.StatusCode, out, time.Since(started))
	}
	f.s.meshSlots.mu.Lock()
	f.s.mu.Lock()
	slots, pending := f.s.meshSlots.total, len(f.s.pending)
	f.s.mu.Unlock()
	f.s.meshSlots.mu.Unlock()
	if slots != 0 || pending != 0 {
		t.Fatalf("slot %d pending %d left behind", slots, pending)
	}
}

// Replacing the target connection fails its in-flight requests at once.
func TestWorkerRequestFailsWhenTheTargetIsReplaced(t *testing.T) {
	f, _ := managedMesh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	arrived := make(chan struct{}, 1)
	connectMachine(t, ctx, f, "beta", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	owner, _ := f.s.tokens.validate(f.key)
	done := make(chan *http.Response, 1)
	go func() {
		res, _ := workerPost(t, f, "/machine-request", controlToken, owner.UserID, `{"machine":"beta","method":"POST","path":"/remote/ask","body":{}}`)
		done <- res
	}()
	<-arrived
	started := time.Now()
	connectMachine(t, ctx, f, "beta", http.NotFoundHandler())
	select {
	case res := <-done:
		if res.StatusCode != http.StatusBadGateway || time.Since(started) > 3*time.Second {
			t.Fatalf("replaced target %d after %v", res.StatusCode, time.Since(started))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request outlived its replaced target")
	}
	f.s.meshSlots.mu.Lock()
	total := f.s.meshSlots.total
	f.s.meshSlots.mu.Unlock()
	if total != 0 {
		t.Fatalf("slot left behind: %d", total)
	}
}
