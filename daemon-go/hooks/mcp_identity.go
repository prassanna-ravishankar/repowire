package hooks

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// MCPIdentity resolves or lazily registers the runtime hosting the stdio shim.
// The returned peer_id is stamped onto every HTTP MCP request.
func MCPIdentity() string {
	claimedPeerID := os.Getenv("REPOWIRE_PEER_ID")
	backend := firstNonempty(os.Getenv("REPOWIRE_BACKEND"), "claude-code")
	cwd := mustGetwd()
	paneID := getPaneID()
	agentPID := os.Getppid()
	if peer := validateCertificateIdentity(backend, cwd, paneID, agentPID); peer != nil {
		return firstNonempty(stringValue(peer, "peer_id"), stringValue(peer, "display_name"))
	}
	hint := consumeSpawnHint(cwd, backend)
	info := getTmuxInfo()
	boundary, err := configuredCircleBoundary()
	if err != nil {
		return filepath.Base(cwd)
	}
	source := "tmux"
	if boundary == proto.CircleBoundaryWindow {
		source = "tmux_window"
	}
	circle := proto.TmuxCircle(boundary, info.SessionName, info.WindowID)
	if circle == "" && paneID != "" && hint != nil {
		circle, source = stringValue(hint, "circle"), "spawn_hint"
	}
	if circle == "" {
		return filepath.Base(cwd)
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
	if claimedPeerID != "" {
		body["peer_id"] = claimedPeerID
	}
	if hint != nil {
		if paneID != "" {
			if value := stringValue(hint, "peer_id"); value != "" {
				body["peer_id"] = value
			}
		}
	}
	status, result := daemonRequest(http.MethodPost, "/peers", body, 2*time.Second)
	if status < 200 || status >= 300 {
		return filepath.Base(cwd)
	}
	if cert, ok := result["birth_certificate"].(map[string]any); ok {
		writeBirthCertificate(backend, agentPID, paneID, cert)
	}
	return firstNonempty(stringValue(result, "peer_id"), stringValue(result, "display_name"), filepath.Base(cwd))
}

// MCPIdentityProof returns the resolved peer identity plus the nonce of a
// daemon-minted runtime certificate that proves the stdio process belongs to
// that peer. The HTTP MCP handler ignores an X-Repowire-Peer claim without
// this proof, preventing direct local clients from spoofing shim identity.
func MCPIdentityProof() (string, string) {
	identity := MCPIdentity()
	backend := firstNonempty(os.Getenv("REPOWIRE_BACKEND"), "claude-code")
	if peer, cert := validateCertificateIdentityWithCert(backend, mustGetwd(), getPaneID(), os.Getppid()); peer != nil && cert != nil {
		resolved := firstNonempty(stringValue(peer, "peer_id"), stringValue(peer, "display_name"))
		if resolved == identity {
			return identity, stringValue(cert, "nonce")
		}
	}
	return identity, ""
}

func validateCertificateIdentity(backend, cwd, paneID string, agentPID int) map[string]any {
	peer, _ := validateCertificateIdentityWithCert(backend, cwd, paneID, agentPID)
	return peer
}

func validateCertificateIdentityWithCert(backend, cwd, paneID string, agentPID int) (map[string]any, map[string]any) {
	var certificates []map[string]any
	if cert, ok := readMetadata(paneID)["birth_certificate"].(map[string]any); ok {
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

func writeBirthCertificate(backend string, agentPID int, paneID string, cert map[string]any) {
	if agentPID <= 0 || cert == nil {
		return
	}
	safeBackend := strings.NewReplacer("/", "-", "\\", "-").Replace(backend)
	raw, _ := json.Marshal(cert)
	_ = os.WriteFile(filepath.Join(paneLogsDir(), "birth-"+safeBackend+"-"+strconv.Itoa(agentPID)+"-"+paneToken(paneID)+".json"), raw, 0o600)
}
