package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/peer"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

func newRetentionServer(t *testing.T, withRetention bool) (*httptest.Server, *state.Store, *peer.Registry) {
	t.Helper()
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	transport := service.NewWebSocketTransport()
	reg, err := peer.NewRegistry(context.Background(), memStore{}, deadLive{}, transport)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubWithTransport(reg, transport, "tok")
	if withRetention {
		r := service.NewRetention(store, config.Defaults().Retention, "").WithJournal(reg.AddEvent).WithSpawn(reg.Track)
		h.WithRetention(r)
		reg.AddRepairPass(r.MaybeSweep)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, reg
}

func retentionCall(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func seedOldTranscript(t *testing.T, store *state.Store) {
	t.Helper()
	if err := store.AppendEvent(context.Background(), peer.Event{Type: "chat_turn", Timestamp: time.Now().Add(-8 * 24 * time.Hour),
		Payload: map[string]any{"text": "old transcript"}}); err != nil {
		t.Fatal(err)
	}
}

func transcriptCount(t *testing.T, store *state.Store) int {
	t.Helper()
	rows, err := store.EventsOfTypes(context.Background(), "chat_turn")
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestRetentionRoutesDryRunThenSweep(t *testing.T) {
	srv, store, _ := newRetentionServer(t, true)
	seedOldTranscript(t, store)

	code, body := retentionCall(t, srv, http.MethodGet, "/retention", "")
	if code != http.StatusOK {
		t.Fatalf("GET /retention = %d %v", code, body)
	}
	policy, _ := body["policy"].(map[string]any)
	next, _ := body["next_sweep"].(map[string]any)
	if policy["content_days"] != 7.0 || policy["records_days"] != 180.0 || next["events.delete_transcripts"] != 1.0 {
		t.Fatalf("GET /retention body = %v", body)
	}

	code, body = retentionCall(t, srv, http.MethodPost, "/retention/sweep", `{"dry_run":true}`)
	if code != http.StatusOK || transcriptCount(t, store) != 1 {
		t.Fatalf("dry run = %d %v, transcripts %d", code, body, transcriptCount(t, store))
	}
	code, body = retentionCall(t, srv, http.MethodPost, "/retention/sweep", "")
	counts, _ := body["counts"].(map[string]any)
	if code != http.StatusOK || counts["events.delete_transcripts"] != 1.0 || transcriptCount(t, store) != 0 {
		t.Fatalf("sweep = %d %v", code, body)
	}
	if code, _ := retentionCall(t, srv, http.MethodPost, "/retention/sweep", `{"vacuum":true}`); code != http.StatusOK {
		t.Fatalf("vacuum = %d", code)
	}
	code, body = retentionCall(t, srv, http.MethodGet, "/retention", "")
	if policy, _ := body["policy"].(map[string]any); code != http.StatusOK || policy["last_sweep_at"] == nil {
		t.Fatalf("last sweep not reported: %v", body)
	}
}

func TestRetentionRoutesNeedAuthAndASweeper(t *testing.T) {
	srv, _, _ := newRetentionServer(t, false)
	resp, err := http.Post(srv.URL+"/retention/sweep", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sweep = %d", resp.StatusCode)
	}
	if code, _ := retentionCall(t, srv, http.MethodGet, "/retention", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("no sweeper = %d", code)
	}
}

func TestLazyRepairStartsADueSweep(t *testing.T) {
	_, store, reg := newRetentionServer(t, true)
	seedOldTranscript(t, store)
	reg.LazyRepair(context.Background())
	reg.Close() // joins the tracked sweep
	if n := transcriptCount(t, store); n != 0 {
		t.Fatalf("transcripts after lazy repair = %d", n)
	}
}
