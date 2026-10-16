package mcpstdio

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRunRefreshesIdentityForEveryRequest(t *testing.T) {
	var proofs, peers, problems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proofs = append(proofs, r.Header.Get("X-Repowire-Identity-Proof"))
		peers = append(peers, r.Header.Get("X-Repowire-Peer"))
		problems = append(problems, r.Header.Get("X-Repowire-Identity-Problem"))
		fmt.Fprintln(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()

	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	t.Setenv("REPOWIRE_CONFIG", t.TempDir()+"/missing.yaml")
	t.Setenv("REPOWIRE_DAEMON__HOST", "127.0.0.1")
	t.Setenv("REPOWIRE_DAEMON__PORT", strconv.Itoa(port))

	original := resolveIdentity
	t.Cleanup(func() { resolveIdentity = original })
	calls := 0
	var threadIDs []string
	resolveIdentity = func(threadID string) (string, string, string) {
		calls++
		threadIDs = append(threadIDs, threadID)
		switch calls {
		case 3:
			return "unregistered-folder", "", "" // no mesh identity resolved
		case 4:
			return "repow-test", "", "expired" // identity whose proof failed
		}
		return "repow-test", fmt.Sprintf("proof-%d", calls), ""
	}

	var out bytes.Buffer
	if code := run(strings.NewReader("{}\n{\"params\":{\"_meta\":{\"threadId\":\"thread-live\"}}}\n{}\n{}\n"), &out); code != 0 {
		t.Fatalf("run returned %d", code)
	}
	if got := strings.Join(proofs, ","); got != "proof-1,proof-2,," {
		t.Fatalf("proofs = %q", got)
	}
	// A failed proof is reported, so the daemon fails the call loudly.
	if got := strings.Join(problems, ","); got != ",,,expired" {
		t.Fatalf("problems = %q", got)
	}
	// Without a proof the shim sends no identity claim: the caller is
	// explicitly anonymous rather than an unproven claim the daemon drops.
	if got := strings.Join(peers, ","); got != "repow-test,repow-test,,repow-test" {
		t.Fatalf("peer headers = %q", got)
	}
	if got := strings.Join(threadIDs, ","); got != ",thread-live,," {
		t.Fatalf("thread ids = %q", got)
	}
}
