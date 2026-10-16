package hooks

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MCPIdentity resolves or lazily registers the runtime hosting the stdio shim.
// The returned peer_id is stamped onto every HTTP MCP request.
func MCPIdentity() string {
	identity, _, _ := resolveMCPIdentity("")
	return identity
}

// resolveMCPIdentity returns the peer identity, the proof nonce, and, when the
// runtime has a certificate that no longer validates and cannot be renewed,
// the problem ("expired" or "invalid") to report instead of acting
// anonymously. An empty proof with no problem means no mesh identity exists.
func resolveMCPIdentity(codexThreadID string) (string, string, string) {
	claimedPeerID := os.Getenv("REPOWIRE_PEER_ID")
	backend := firstNonempty(os.Getenv("REPOWIRE_BACKEND"), "claude-code")
	cwd := mustGetwd()
	paneID := getPaneID()
	agentPID := os.Getppid()
	paneMeta := ReadPaneRuntimeMetadata(paneID)
	if backend == "codex" {
		threadID := firstNonempty(codexThreadID, os.Getenv("CODEX_THREAD_ID"))
		if cert, ok := ReadRuntimeIdentity(backend, threadID)["birth_certificate"].(map[string]any); ok {
			if peer, _ := validateCertificates(backend, cwd, paneID, agentPID, []map[string]any{cert}); peer != nil {
				return firstNonempty(stringValue(peer, "peer_id"), stringValue(peer, "display_name")), stringValue(cert, "nonce"), ""
			}
			// The thread has a mesh identity whose proof failed (normally an
			// expired certificate the bridge has not renewed yet). Renew it now
			// while the thread is connected; if that fails, report the problem
			// so the daemon fails the call instead of running it as mcp-http.
			if fresh := renewThreadCertificate(backend, threadID, cwd, cert); fresh != nil {
				return stringValue(fresh, "peer_id"), stringValue(fresh, "nonce"), ""
			}
			return stringValue(cert, "peer_id"), "", certificateProblem(cert)
		}
		// Codex App Server shares one process and MCP subprocess across many
		// threads. A pane/PID certificate belongs to some other thread and must
		// never be used as fallback identity for this thread.
		if threadID == "" {
			if peer, cert := validateCertificateIdentityWithCert(backend, cwd, paneID, agentPID); peer != nil {
				return firstNonempty(stringValue(peer, "peer_id"), stringValue(peer, "display_name")), stringValue(cert, "nonce"), ""
			}
		}
	} else {
		if peer, cert := validateCertificateIdentityWithCert(backend, cwd, paneID, agentPID); peer != nil {
			return firstNonempty(stringValue(peer, "peer_id"), stringValue(peer, "display_name")), stringValue(cert, "nonce"), ""
		}
	}
	// An expired certificate renews the pane's existing runtime identity. PID,
	// backend, and cwd must all match so stale pane metadata cannot claim a peer.
	panePeerID := ""
	if stringValue(paneMeta, "backend") == backend && stringValue(paneMeta, "cwd") == cwd && intFromAny(paneMeta["agent_pid"]) == agentPID {
		panePeerID = stringValue(paneMeta, "peer_id")
	}
	// Every use of a spawn hint below needs a pane. A paneless shim (the Codex
	// App Server's shared MCP subprocess) must leave the hint queued for the
	// runtime it addresses instead of consuming and discarding it.
	var hint map[string]any
	if paneID != "" {
		hint = consumeSpawnHint(cwd, backend)
	}
	info := getTmuxInfo()
	_, circle, source, err := tmuxPlacement(info)
	if err != nil {
		return filepath.Base(cwd), "", ""
	}
	if circle == "" && paneID != "" && hint != nil {
		circle, source = stringValue(hint, "circle"), "spawn_hint"
	}
	if circle == "" {
		return filepath.Base(cwd), "", ""
	}
	body := map[string]any{
		"name": filepath.Base(cwd), "path": cwd, "circle": circle,
		"circle_source": source, "backend": backend, "agent_pid": agentPID,
	}
	if target := tmuxSession(info); target != "" {
		body["tmux_session"] = target
	}
	if paneID != "" {
		body["pane_id"] = paneID
	}
	if panePeerID != "" {
		body["peer_id"] = panePeerID
	} else if claimedPeerID != "" {
		body["peer_id"] = claimedPeerID
	}
	if hint != nil {
		if paneID != "" && panePeerID == "" {
			if value := stringValue(hint, "peer_id"); value != "" {
				body["peer_id"] = value
			}
		}
	}
	status, result := daemonRequest(http.MethodPost, "/peers", body, 2*time.Second)
	if status < 200 || status >= 300 {
		return filepath.Base(cwd), "", ""
	}
	proof := ""
	if cert, ok := result["birth_certificate"].(map[string]any); ok {
		resultPeerID := stringValue(result, "peer_id")
		if panePeerID != "" && resultPeerID == panePeerID {
			paneMeta["birth_certificate"] = cert
			paneMeta["display_name"] = stringValue(result, "display_name")
			_ = writeMetadata(paneID, paneMeta)
		} else {
			writeBirthCertificate(backend, agentPID, paneID, cert)
		}
		proof = stringValue(cert, "nonce")
	}
	return firstNonempty(stringValue(result, "peer_id"), stringValue(result, "display_name"), filepath.Base(cwd)), proof, ""
}

// MCPIdentityProof returns the resolved peer identity plus the nonce of a
// daemon-minted runtime certificate that proves the stdio process belongs to
// that peer. The HTTP MCP handler ignores an X-Repowire-Peer claim without
// this proof, preventing direct local clients from spoofing shim identity.
func MCPIdentityProof() (string, string, string) {
	return resolveMCPIdentity("")
}

// MCPIdentityProofForThread uses Codex's per-call _meta.threadId when App
// Server shares one MCP subprocess across multiple threads.
func MCPIdentityProofForThread(threadID string) (string, string, string) {
	return resolveMCPIdentity(threadID)
}

// renewThreadCertificate exchanges a session-native thread's certificate for a
// fresh one (the daemon allows it only while the thread is connected on the
// same session) and caches it for later calls. nil when renewal is refused.
func renewThreadCertificate(backend, threadID, cwd string, cert map[string]any) map[string]any {
	status, result := daemonRequest(http.MethodPost, "/peers/identity/renew",
		map[string]any{"birth_certificate": cert, "backend": backend, "path": cwd}, 2*time.Second)
	fresh, _ := result["birth_certificate"].(map[string]any)
	if status < 200 || status >= 300 || stringValue(fresh, "nonce") == "" {
		return nil
	}
	_ = WriteRuntimeIdentity(backend, threadID, map[string]any{"birth_certificate": fresh})
	return fresh
}

func certificateProblem(cert map[string]any) string {
	expires, err := time.Parse(time.RFC3339Nano, stringValue(cert, "expires_at"))
	if err == nil && !expires.After(time.Now()) {
		return "expired"
	}
	return "invalid"
}

func validateCertificateIdentity(backend, cwd, paneID string, agentPID int) map[string]any {
	peer, _ := validateCertificateIdentityWithCert(backend, cwd, paneID, agentPID)
	return peer
}

func validateCertificateIdentityWithCert(backend, cwd, paneID string, agentPID int) (map[string]any, map[string]any) {
	var certificates []map[string]any
	if cert, ok := ReadPaneRuntimeMetadata(paneID)["birth_certificate"].(map[string]any); ok {
		certificates = append(certificates, cert)
	}
	safeBackend := strings.NewReplacer("/", "-", "\\", "-").Replace(backend)
	paths, _ := filepath.Glob(filepath.Join(paneLogsDir(), "birth-"+safeBackend+"-"+strconv.Itoa(agentPID)+"-*.json"))
	for _, path := range paths {
		var cert map[string]any
		if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &cert) == nil {
			certificates = append(certificates, cert)
		}
	}
	return validateCertificates(backend, cwd, paneID, agentPID, certificates)
}

func validateCertificates(backend, cwd, paneID string, agentPID int, certificates []map[string]any) (map[string]any, map[string]any) {
	seen := map[string]bool{}
	for _, cert := range certificates {
		nonce := stringValue(cert, "nonce")
		if nonce == "" || seen[nonce] {
			continue
		}
		seen[nonce] = true
		body := map[string]any{"birth_certificate": cert, "backend": backend, "path": cwd, "agent_pid": agentPID}
		if paneID != "" {
			body["pane_id"] = paneID
		}
		status, result := daemonRequest(http.MethodPost, "/peers/identity/validate", body, 2*time.Second)
		if status >= 200 && status < 300 {
			if peer, ok := result["peer"].(map[string]any); ok {
				return peer, cert
			}
			return result, cert
		}
	}
	return nil, nil
}
