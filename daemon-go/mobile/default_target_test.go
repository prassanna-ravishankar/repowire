package mobile

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The phone's default target is the peer with the orchestrator role, wherever
// it sits in /peers. A peer that only shares the "orchestrator-" name prefix
// (a Codex thread in the orchestrator's folder) never wins over it, and is
// the target only when no peer has the role.
func TestDefaultTargetPrefersTheOrchestratorRole(t *testing.T) {
	codex := `{"peer_id":"repow-1-codex","display_name":"orchestrator-codex","role":"agent","status":"online"}`
	orch := `{"peer_id":"repow-1-orch","display_name":"orchestrator-d4","role":"orchestrator","status":"online"}`
	for name, tc := range map[string]struct{ peers, wantID, wantName string }{
		"role listed after a prefix match": {`[` + codex + `,` + orch + `]`, "repow-1-orch", "orchestrator-d4"},
		"role listed first":                {`[` + orch + `,` + codex + `]`, "repow-1-orch", "orchestrator-d4"},
		"no role: prefix fallback":         {`[` + codex + `]`, "repow-1-codex", "orchestrator-codex"},
		"neither":                          {`[{"peer_id":"repow-1-x","display_name":"torale-ab","role":"agent","status":"online"}]`, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"peers":` + tc.peers + `}`))
			}))
			defer server.Close()
			peer := NewDaemonPeer(server.URL, "", "telegram", "/telegram", "default")
			id, label := peer.DefaultTargetInfo(context.Background())
			if id != tc.wantID || label != tc.wantName {
				t.Fatalf("DefaultTargetInfo = (%q, %q), want (%q, %q)", id, label, tc.wantID, tc.wantName)
			}
		})
	}
}
