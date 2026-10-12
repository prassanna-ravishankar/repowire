package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// workCall issues one request against the work routes and decodes the body.
func workCall(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func workState(t *testing.T, srvURL, workID string) (string, bool) {
	t.Helper()
	_, out := workCall(t, http.MethodGet, srvURL+"/work/"+workID+"/status", "")
	status, _ := out["status"].(map[string]any)
	if status == nil {
		status = out
	}
	st, _ := status["state"].(string)
	cancel, _ := status["cancel_requested"].(bool)
	return st, cancel
}

// A cancel on never-dispatched work that is not queued (here blocked, reachable
// through the public job_update surface) must terminate it, and a later
// job_update must not revive cancelled work as queued. Before the fix the cancel
// left the row blocked with cancel_requested=1, and a job_update to queued then
// produced a row AcquireForDispatch refuses forever while the runner computes a
// deadline of now for it on every iteration.
func TestCancelNeverDispatchedNonQueuedWorkIsTerminal(t *testing.T) {
	store := newWorkTestStore(t)
	h := &Hub{work: &workRoutes{store: store}}
	mux := http.NewServeMux()
	h.registerWorkRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	code, created := workCall(t, http.MethodPost, srv.URL+"/work", `{"title":"B","prompt":"p","path":"/tmp/p","backend":"claude-code"}`)
	if code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	workID, _ := created["work_id"].(string)

	if code, out := workCall(t, http.MethodPatch, srv.URL+"/work/"+workID, `{"state":"blocked","state_reason":"waiting"}`); code != http.StatusOK {
		t.Fatalf("job_update blocked = %d %v", code, out)
	}
	if st, _ := workState(t, srv.URL, workID); st != "blocked" {
		t.Fatalf("state = %q, want blocked", st)
	}

	if code, out := workCall(t, http.MethodPost, srv.URL+"/work/"+workID+"/cancel", `{"reason":"user_cancel"}`); code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if st, _ := workState(t, srv.URL, workID); st != "cancelled" {
		t.Fatalf("after cancel state = %q, want cancelled", st)
	}

	code, _ = workCall(t, http.MethodPatch, srv.URL+"/work/"+workID, `{"state":"queued"}`)
	if code == http.StatusOK {
		if st, cancel := workState(t, srv.URL, workID); st == "queued" && cancel {
			t.Fatalf("job_update revived cancelled work as queued with cancel_requested")
		}
	}
}

// Independent of blocked: no job_update may move cancel-requested work into
// queued, because AcquireForDispatch refuses cancel-requested rows and the row
// would be stranded. This pins the generic route against reintroducing it.
func TestJobUpdateRefusesQueuedForCancelRequestedWork(t *testing.T) {
	store := newWorkTestStore(t)
	h := &Hub{work: &workRoutes{store: store}}
	mux := http.NewServeMux()
	h.registerWorkRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, created := workCall(t, http.MethodPost, srv.URL+"/work", `{"title":"C","prompt":"p","path":"/tmp/p","backend":"claude-code"}`)
	workID, _ := created["work_id"].(string)
	// awaiting_input: non-terminal, never dispatched, not queued or blocked, so
	// cancel takes the cooperative branch and only sets cancel_requested.
	if code, out := workCall(t, http.MethodPatch, srv.URL+"/work/"+workID, `{"state":"awaiting_input"}`); code != http.StatusOK {
		t.Fatalf("job_update awaiting_input = %d %v", code, out)
	}
	workCall(t, http.MethodPost, srv.URL+"/work/"+workID+"/cancel", `{"reason":"user_cancel"}`)
	if _, cancel := workState(t, srv.URL, workID); !cancel {
		t.Fatalf("cancel_requested not set")
	}

	code, _ := workCall(t, http.MethodPatch, srv.URL+"/work/"+workID, `{"state":"queued"}`)
	if code == http.StatusOK {
		t.Fatalf("job_update to queued on cancel-requested work = 200, want refusal")
	}
	if st, _ := workState(t, srv.URL, workID); st == "queued" {
		t.Fatalf("cancel-requested work is queued")
	}
}
