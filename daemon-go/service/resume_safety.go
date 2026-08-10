package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
)

// ResolveLocalResume returns a plan only after proving the backend-owned session
// file exists locally. Unknown or stale ids fall back to fresh spawn.
func ResolveLocalResume(backend proto.AgentType, path, runtimeSessionID string, repowireSessionID *string, capability map[string]any) (map[string]any, bool) {
	if runtimeSessionID == "" {
		return nil, false
	}
	if supported, ok := capability["supported"].(bool); ok && !supported {
		return nil, false
	}
	if !canResumeBackend(backend, runtimeSessionID) {
		return nil, false
	}
	if runtimeSessionValidationStatus(path, backend, runtimeSessionID) != "resumable" {
		return nil, false
	}
	plan := map[string]any{
		"backend":            string(backend),
		"runtime_session_id": runtimeSessionID,
		"capability":         capability,
	}
	if repowireSessionID != nil && *repowireSessionID != "" {
		plan["repowire_session_id"] = *repowireSessionID
	}
	return plan, true
}

func runtimeSessionValidationStatus(peerPath string, backend proto.AgentType, runtimeSessionID string) string {
	if runtimeSessionID == "" {
		return "missing_id"
	}
	if !canResumeBackend(backend, runtimeSessionID) {
		return "unsupported"
	}
	switch backend {
	case proto.AgentClaudeCode:
		if claudeResumable(peerPath, runtimeSessionID) {
			return "resumable"
		}
	case proto.AgentCodex:
		if codexResumable(peerPath, runtimeSessionID) {
			return "resumable"
		}
	case proto.AgentOpenCode:
		if opencodeResumable(peerPath, runtimeSessionID) {
			return "resumable"
		}
	case proto.AgentPi:
		if piResumable(peerPath, runtimeSessionID) {
			return "resumable"
		}
	case proto.AgentAntigravity:
		if antigravityResumable(peerPath, runtimeSessionID) {
			return "resumable"
		}
	case proto.AgentGemini:
		if geminiResumable(peerPath, runtimeSessionID) {
			return "resumable"
		}
	default:
		return "unvalidated_backend"
	}
	return "stale_missing_file"
}

func claudeResumable(peerPath, runtimeSessionID string) bool {
	if peerPath == "" {
		return false
	}
	direct := filepath.Join(homeDir(), ".claude", "projects", encodeClaudeCWD(peerPath), runtimeSessionID+".jsonl")
	if fileExists(direct) {
		return true
	}
	return false
}

func codexResumable(peerPath, runtimeSessionID string) bool {
	root := filepath.Join(homeDir(), ".codex", "sessions")
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(filepath.Base(path), "rollout-") || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if !strings.Contains(filepath.Base(path), runtimeSessionID) {
			return nil
		}
		if peerPath == "" || codexFileMatchesPeer(path, peerPath) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func opencodeResumable(peerPath, runtimeSessionID string) bool {
	root := filepath.Join(homeDir(), ".local", "share", "opencode", "storage", "session")
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(filepath.Base(path), "ses_") || !strings.HasSuffix(path, ".json") {
			return nil
		}
		var data map[string]any
		if readJSONFile(path, &data) != nil || data["id"] != runtimeSessionID {
			return nil
		}
		if peerPath == "" || pathsMatch(strAny(data["directory"]), peerPath) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func piResumable(peerPath, runtimeSessionID string) bool {
	var data map[string]any
	if readJSONFile(filepath.Join(homeDir(), ".pi", "pi-acp", "session-map.json"), &data) != nil {
		return false
	}
	sessions := mapAtAny(data, "sessions")
	entry := mapAtAny(sessions, runtimeSessionID)
	if len(entry) == 0 {
		return false
	}
	if peerPath != "" && !pathsMatch(strAny(entry["cwd"]), peerPath) {
		return false
	}
	return fileExists(strAny(entry["sessionFile"]))
}

func antigravityResumable(peerPath, runtimeSessionID string) bool {
	root := filepath.Join(homeDir(), ".gemini", "antigravity-cli")
	var last map[string]any
	if readJSONFile(filepath.Join(root, "cache", "last_conversations.json"), &last) != nil {
		return false
	}
	if peerPath != "" {
		mapped := ""
		for cwd, id := range last {
			if pathsMatch(cwd, peerPath) {
				mapped = strAny(id)
				break
			}
		}
		if mapped != runtimeSessionID {
			return false
		}
	} else {
		seen := false
		for _, id := range last {
			if strAny(id) == runtimeSessionID {
				seen = true
				break
			}
		}
		if !seen {
			return false
		}
	}
	return fileExists(filepath.Join(root, "conversations", runtimeSessionID+".pb"))
}

func geminiResumable(peerPath, runtimeSessionID string) bool {
	root := filepath.Join(homeDir(), ".gemini", "tmp")
	expectedHash := ""
	if peerPath != "" {
		expectedHash = fmt.Sprintf("%x", sha256.Sum256([]byte(NormPath(peerPath))))
	}
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(filepath.Base(path), "session-") || !strings.HasSuffix(path, ".json") {
			return nil
		}
		var data map[string]any
		if readJSONFile(path, &data) != nil || data["sessionId"] != runtimeSessionID {
			return nil
		}
		if peerPath == "" || pathsMatch(strAny(data["directory"]), peerPath) || pathsMatch(strAny(data["cwd"]), peerPath) || strAny(data["projectHash"]) == expectedHash {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func codexFileMatchesPeer(path, peerPath string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		typ := strAny(entry["type"])
		if typ != "session_meta" && typ != "turn_context" {
			continue
		}
		payload := mapAtAny(entry, "payload")
		if pathsMatch(strAny(payload["cwd"]), peerPath) {
			return true
		}
	}
	return false
}

func encodeClaudeCWD(path string) string { return strings.ReplaceAll(path, "/", "-") }

func pathsMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return NormPath(a) == NormPath(b)
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func readJSONFile(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}
