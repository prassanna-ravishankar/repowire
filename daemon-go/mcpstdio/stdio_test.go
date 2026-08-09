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
	var proofs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proofs = append(proofs, r.Header.Get("X-Repowire-Identity-Proof"))
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
	resolveIdentity = func(threadID string) (string, string) {
		calls++
		threadIDs = append(threadIDs, threadID)
		return "repow-test", fmt.Sprintf("proof-%d", calls)
	}

	var out bytes.Buffer
	if code := run(strings.NewReader("{}\n{\"params\":{\"_meta\":{\"threadId\":\"thread-live\"}}}\n"), &out); code != 0 {
		t.Fatalf("run returned %d", code)
	}
	if got := strings.Join(proofs, ","); got != "proof-1,proof-2" {
		t.Fatalf("proofs = %q", got)
	}
	if got := strings.Join(threadIDs, ","); got != ",thread-live" {
		t.Fatalf("thread ids = %q", got)
	}
}
