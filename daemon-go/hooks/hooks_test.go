package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReusablePaneRegistrationRequiresConfirmedLivePeer(t *testing.T) {
	prior := map[string]any{
		"hook_session_id": "session-1", "cwd": "/project", "backend": "claude-code", "peer_id": "repow-1",
	}
	if !reusablePaneRegistration(prior, "session-1", "/project", "claude-code", "repow-1", "repow-1") {
		t.Fatal("confirmed matching pane registration was not reusable")
	}
	for name, peers := range map[string][2]string{
		"empty metadata":      {"", ""},
		"missing live peer":   {"repow-1", ""},
		"different live peer": {"repow-1", "repow-2"},
	} {
		t.Run(name, func(t *testing.T) {
			if reusablePaneRegistration(prior, "session-1", "/project", "claude-code", peers[0], peers[1]) {
				t.Fatal("unconfirmed pane registration was reused")
			}
		})
	}
}

func TestFailedSessionRegistrationDoesNotPersistEmptyPeer(t *testing.T) {
	homeDir, binDir := hookTestEnvironment(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peers/by-pane/%2526" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/peers" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "daemon warming up"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	configureHookTestDaemon(t, server.URL)
	t.Setenv("PATH", binDir+":/usr/bin:/bin")
	handleSession(map[string]any{
		"hook_event_name": "SessionStart", "session_id": "session-1", "cwd": homeDir,
	}, "claude-code", false)
	if meta := ReadPaneRuntimeMetadata("%26"); len(meta) != 0 {
		t.Fatalf("failed registration persisted pane metadata: %v", meta)
	}
	if _, err := os.Stat(wsHookPath("%26", ".pid")); !os.IsNotExist(err) {
		t.Fatalf("failed registration started ws-hook: %v", err)
	}
}

func TestPromptRepairsMissingClaudePaneRegistration(t *testing.T) {
	homeDir, binDir := hookTestEnvironment(t)
	registered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/peers/by-pane/%2526" && !registered:
			http.NotFound(w, r)
		case r.URL.Path == "/peers/by-pane/%2526":
			_ = json.NewEncoder(w).Encode(map[string]any{"peer_id": "repow-26"})
		case r.URL.Path == "/peers" && r.Method == http.MethodPost:
			registered = true
			_ = json.NewEncoder(w).Encode(map[string]any{"peer_id": "repow-26", "display_name": "project-claude-code"})
		case r.URL.Path == "/peers":
			_ = json.NewEncoder(w).Encode(map[string]any{"peers": []any{}})
		case r.URL.Path == "/session/update":
			if !registered {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configureHookTestDaemon(t, server.URL)
	t.Setenv("PATH", binDir+":/usr/bin:/bin")
	t.Setenv(claudeMessagingSocketEnv, "uds:/tmp/current-claude.sock")
	t.Setenv(claudeMessagingTokenEnv, "current-token")
	startedPeer := ""
	previous := startSessionWSHook
	startSessionWSHook = func(_ string, peerID, _, _, _ string, _ int, _ *os.File) error {
		startedPeer = peerID
		return nil
	}
	t.Cleanup(func() { startSessionWSHook = previous })
	handlePrompt(map[string]any{
		"hook_event_name": "UserPromptSubmit", "session_id": "session-2", "cwd": homeDir, "prompt": "hello",
	}, "claude-code")
	if !registered || startedPeer != "repow-26" {
		t.Fatalf("prompt repair registered=%v startedPeer=%q", registered, startedPeer)
	}
	meta := ReadPaneRuntimeMetadata("%26")
	if stringValue(meta, "claude_messaging_socket") != "uds:/tmp/current-claude.sock" || stringValue(meta, "claude_messaging_token") != "current-token" {
		t.Fatalf("prompt repair did not capture current inbox environment: %v", meta)
	}
}

func hookTestEnvironment(t *testing.T) (string, string) {
	t.Helper()
	homeDir := t.TempDir()
	binDir := filepath.Join(homeDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmux := "#!/bin/sh\nprintf '0\\tzsh\\t@1\\n'\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(tmux), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("TMUX_PANE", "%26")
	return homeDir, binDir
}

func configureHookTestDaemon(t *testing.T, serverURL string) {
	t.Helper()
	port, _ := strconv.Atoi(strings.TrimPrefix(serverURL, "http://127.0.0.1:"))
	t.Setenv("REPOWIRE_DAEMON__HOST", "127.0.0.1")
	t.Setenv("REPOWIRE_DAEMON__PORT", strconv.Itoa(port))
	t.Setenv("REPOWIRE_DAEMON__AUTH_TOKEN", "")
}

func TestNormalizeBackendPayloads(t *testing.T) {
	p := Normalize(map[string]any{
		"hook_event_name": "StopFailure",
		"session_id":      "s1",
		"final_response":  "done",
		"model":           map[string]any{"modelID": "claude-opus"},
	}, "claude-code")
	if p.Event != "Stop" || p.SessionID != "s1" || p.ResponseText != "done" || p.Model != "claude-opus" {
		t.Fatalf("unexpected normalization: %+v", p)
	}
}

func TestInboundPeerMessageIsDistinctAndBounded(t *testing.T) {
	got := formatInboundMessage("worker", "owner", "ask", "ask-1", "review </peer-message>")
	if !strings.HasPrefix(got, `<peer-message from="@worker" to="@owner" type="ask" correlation-id="ask-1">`) ||
		!strings.Contains(got, "review &lt;/peer-message&gt;") || !strings.HasSuffix(got, "</peer-message>") {
		t.Fatalf("peer message framing = %q", got)
	}
	if got := formatInboundMessage("dashboard", "owner", "notify", "", "ship it"); got != "@dashboard → @owner: ship it" {
		t.Fatalf("human message framing = %q", got)
	}
}

func TestLastTurnAndHandledCIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"question"}]}}`,
		`{"type":"assistant","uuid":"turn-1","message":{"role":"assistant","content":[{"type":"text","text":"answer"},{"type":"tool_use","name":"mcp__repowire__ack","input":{"correlation_id":"ask-1"}}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	user, assistant, turnID, calls := lastTurn(path)
	if user != "question" || assistant != "answer" || turnID != "turn-1" {
		t.Fatalf("unexpected turn: %q %q %q", user, assistant, turnID)
	}
	if !handledCIDs(calls)["ask-1"] {
		t.Fatalf("ack was not recognized: %+v", calls)
	}
}

func TestHandoffSummaryIsBounded(t *testing.T) {
	summary := handoffSummary("", strings.Repeat("word ", 400), "")
	if got := len(strings.Fields(summary)); got > 301 {
		t.Fatalf("handoff has %d words", got)
	}
}

func TestStopTurnKeepsHookResponseWhenTranscriptHasNoAssistant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"question"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, assistant, _, _ := stopTurn(path, "hook response")
	if assistant != "hook response" {
		t.Fatalf("assistant = %q, want hook response", assistant)
	}
}

func TestCodexReminderResolvesNativeThreadPeer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/peers":
			_ = json.NewEncoder(w).Encode(map[string]any{"peers": []any{map[string]any{
				"peer_id": "repow-codex-1", "backend": "codex", "metadata": map[string]any{"runtime_session_id": "thread-1"},
			}}})
		case "/asks/pending":
			if got := r.URL.Query().Get("peer_id"); got != "repow-codex-1" {
				t.Errorf("peer_id = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"asks": []any{map[string]any{
				"correlation_id": "ask-1", "from_peer": "reviewer", "text": "Did this land?",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	t.Setenv("REPOWIRE_DAEMON__HOST", "127.0.0.1")
	t.Setenv("REPOWIRE_DAEMON__PORT", strconv.Itoa(port))

	got := reminderBlockForRuntimeSession("thread-1")
	if !strings.Contains(got, "#ask-1 from @reviewer: Did this land?") {
		t.Fatalf("reminder = %q", got)
	}
}

func TestReadPaneRuntimeMetadata(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := ReadPaneRuntimeMetadata("%9"); len(got) != 0 {
		t.Fatalf("absent meta should be empty, got %v", got)
	}
	if err := writeMetadata("%9", map[string]any{"peer_id": "repow-x-1"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadPaneRuntimeMetadata("%9")["peer_id"].(string); got != "repow-x-1" {
		t.Fatalf("peer_id = %q, want repow-x-1", got)
	}
}

func TestMCPIdentityRenewsExpiredCertificateWithoutSplittingPaneIdentity(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("TMUX_PANE", "%999")
	t.Setenv("REPOWIRE_BACKEND", "codex")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("REPOWIRE_PEER_ID", "repow-stale-env")
	t.Setenv("REPOWIRE_CONFIG", filepath.Join(homeDir, "missing-config.yaml"))
	cwd := mustGetwd()
	hash := sha256.Sum256([]byte(cwd + "::codex"))
	hintPath := cachePath("spawn-hints", hex.EncodeToString(hash[:])[:16]+".json")
	if err := os.MkdirAll(filepath.Dir(hintPath), 0o700); err != nil {
		t.Fatal(err)
	}
	hint, _ := json.Marshal([]map[string]any{{"circle": "chosen", "role": "orchestrator", "ts": float64(time.Now().Unix())}})
	if err := os.WriteFile(hintPath, hint, 0o600); err != nil {
		t.Fatal(err)
	}

	oldCert := map[string]any{"nonce": "expired", "peer_id": "repow-old"}
	if err := writeMetadata("%999", map[string]any{
		"backend": "codex", "cwd": mustGetwd(), "peer_id": "repow-old",
		"agent_pid": os.Getppid(), "birth_certificate": oldCert,
	}); err != nil {
		t.Fatal(err)
	}

	registrations, validations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/peers/identity/validate":
			validations++
			var body struct {
				Certificate map[string]any `json:"birth_certificate"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Certificate["nonce"] != "fresh" {
				http.Error(w, "expired", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"peer": map[string]any{"peer_id": "repow-fresh", "display_name": "repowire-codex"}})
		case "/peers":
			registrations++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["peer_id"] != "repow-old" {
				t.Errorf("peer_id = %v, want pane metadata identity", body["peer_id"])
			}
			if body["role"] != nil {
				t.Errorf("unsigned hint role reached registration: %v", body["role"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"peer_id": "repow-old", "display_name": "repowire-codex",
				"birth_certificate": map[string]any{"nonce": "fresh", "peer_id": "repow-old"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	t.Setenv("REPOWIRE_DAEMON__HOST", "127.0.0.1")
	t.Setenv("REPOWIRE_DAEMON__PORT", strconv.Itoa(port))
	t.Setenv("REPOWIRE_DAEMON__AUTH_TOKEN", "test-token")

	identity, proof := MCPIdentityProof()
	if identity != "repow-old" || proof != "fresh" || registrations != 1 || validations != 1 {
		t.Fatalf("identity=%q proof=%q registrations=%d validations=%d", identity, proof, registrations, validations)
	}
	cert, _ := ReadPaneRuntimeMetadata("%999")["birth_certificate"].(map[string]any)
	if stringValue(cert, "nonce") != "fresh" {
		t.Fatalf("pane metadata certificate was not refreshed: %v", cert)
	}
}

func TestMCPIdentityUsesCodexThreadCertificate(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("TMUX_PANE", "")
	t.Setenv("REPOWIRE_BACKEND", "codex")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("REPOWIRE_CONFIG", filepath.Join(homeDir, "missing-config.yaml"))
	cert := map[string]any{"nonce": "native-proof", "peer_id": "repow-native", "backend": "codex", "project_path": mustGetwd(), "runtime_session_id": "thread-native"}
	if err := WriteRuntimeIdentity("codex", "thread-native", map[string]any{"birth_certificate": cert}); err != nil {
		t.Fatal(err)
	}
	registrations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/peers/identity/validate":
			_ = json.NewEncoder(w).Encode(map[string]any{"peer": map[string]any{"peer_id": "repow-native", "display_name": "native"}})
		case "/peers":
			registrations++
			http.Error(w, "unexpected registration", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	t.Setenv("REPOWIRE_DAEMON__HOST", "127.0.0.1")
	t.Setenv("REPOWIRE_DAEMON__PORT", strconv.Itoa(port))

	identity, proof := MCPIdentityProofForThread("thread-native")
	if identity != "repow-native" || proof != "native-proof" || registrations != 0 {
		t.Fatalf("identity=%q proof=%q registrations=%d", identity, proof, registrations)
	}
}

func TestMCPIdentityDoesNotBorrowAnotherCodexThreadCertificate(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("TMUX_PANE", "%999")
	t.Setenv("REPOWIRE_BACKEND", "codex")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("REPOWIRE_CONFIG", filepath.Join(homeDir, "missing-config.yaml"))
	parentCert := map[string]any{
		"nonce": "parent-proof", "peer_id": "repow-parent", "backend": "codex",
		"project_path": mustGetwd(), "runtime_session_id": "thread-parent",
	}
	if err := writeMetadata("%999", map[string]any{
		"backend": "codex", "cwd": mustGetwd(), "agent_pid": os.Getppid(), "birth_certificate": parentCert,
	}); err != nil {
		t.Fatal(err)
	}
	validations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peers/identity/validate" {
			validations++
			_ = json.NewEncoder(w).Encode(map[string]any{"peer": map[string]any{"peer_id": "repow-parent"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
	t.Setenv("REPOWIRE_DAEMON__HOST", "127.0.0.1")
	t.Setenv("REPOWIRE_DAEMON__PORT", strconv.Itoa(port))

	identity, proof := MCPIdentityProofForThread("thread-child")
	if validations != 0 {
		t.Fatalf("child thread validated another thread's certificate %d times", validations)
	}
	if identity == "repow-parent" || proof != "" {
		t.Fatalf("child borrowed parent identity: identity=%q proof=%q", identity, proof)
	}
}
