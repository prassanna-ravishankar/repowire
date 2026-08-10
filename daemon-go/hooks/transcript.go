package hooks

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type transcriptTurn struct {
	Role, Text string
}

type toolCall struct {
	Name  string
	Input map[string]any
}

func readTranscript(path string) []map[string]any {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var entries []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		var entry map[string]any
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

func textContent(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, item := range content {
			switch block := item.(type) {
			case string:
				parts = append(parts, block)
			case map[string]any:
				switch stringValue(block, "type") {
				case "text":
					parts = append(parts, stringValue(block, "text"))
				case "tool_use":
					parts = append(parts, "[tool: "+firstNonempty(stringValue(block, "name"), "tool")+"]")
				case "tool_result":
					parts = append(parts, "[tool result]")
				}
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		if stringValue(content, "type") == "text" {
			return stringValue(content, "text")
		}
	}
	return ""
}

func entryTurn(entry map[string]any) transcriptTurn {
	role := firstNonempty(stringValue(entry, "type"), stringValue(entry, "role"))
	content := entry["content"]
	if message, ok := entry["message"].(map[string]any); ok {
		role = firstNonempty(stringValue(message, "role"), role)
		content = message["content"]
	}
	if role != "user" && role != "assistant" {
		return transcriptTurn{}
	}
	return transcriptTurn{Role: role, Text: cleanText(textContent(content))}
}

func lastTurn(path string) (user, assistant, turnID string, calls []toolCall) {
	entries := readTranscript(path)
	assistantHadText := false
	boundary := -1
	for i, entry := range entries {
		role := stringValue(entry, "type")
		var content any
		if message, ok := entry["message"].(map[string]any); ok {
			role = firstNonempty(stringValue(message, "role"), role)
			content = message["content"]
		}
		turn := transcriptTurn{Role: role, Text: cleanText(textOnlyContent(content))}
		if turn.Role == "user" && turn.Text != "" {
			user, boundary = turn.Text, i
		}
		if turn.Role == "assistant" {
			assistantHadText = turn.Text != ""
			if turn.Text != "" {
				assistant = turn.Text
			}
		}
	}
	if !assistantHadText {
		assistant = ""
	}
	for _, entry := range entries[boundary+1:] {
		if stringValue(entry, "type") == "assistant" && turnID == "" {
			turnID = stringValue(entry, "uuid")
		}
		message, _ := entry["message"].(map[string]any)
		content, _ := message["content"].([]any)
		for _, raw := range content {
			item, _ := raw.(map[string]any)
			if stringValue(item, "type") != "tool_use" {
				continue
			}
			input, _ := item["input"].(map[string]any)
			calls = append(calls, toolCall{Name: stringValue(item, "name"), Input: input})
		}
	}
	if len(calls) == 0 {
		for i := len(entries) - 1; i >= 0; i-- {
			payload, _ := entries[i]["payload"].(map[string]any)
			if stringValue(payload, "type") == "message" && stringValue(payload, "role") == "user" {
				break
			}
			if stringValue(payload, "type") != "function_call" {
				continue
			}
			var input map[string]any
			switch args := payload["arguments"].(type) {
			case string:
				_ = json.Unmarshal([]byte(args), &input)
			case map[string]any:
				input = args
			}
			calls = append([]toolCall{{Name: stringValue(payload, "name"), Input: input}}, calls...)
		}
	}
	return
}

func textOnlyContent(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	items, _ := value.([]any)
	var parts []string
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if stringValue(item, "type") == "text" {
			parts = append(parts, stringValue(item, "text"))
		}
	}
	return strings.Join(parts, " ")
}

func cleanText(value string) string {
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(value, " "))
}

func handledCIDs(calls []toolCall) map[string]bool {
	out := map[string]bool{}
	for _, call := range calls {
		name := call.Name
		if i := strings.LastIndex(name, "__"); i >= 0 {
			name = name[i+2:]
		}
		if name == "ack" {
			cid := firstNonempty(stringValue(call.Input, "correlation_id"), stringValue(call.Input, "corr_id"))
			out[cid] = cid != ""
		} else if name == "ask" || name == "ask_peer" {
			cid := stringValue(call.Input, "reply_to")
			out[cid] = cid != ""
		}
	}
	return out
}

func handoffPath(cwd, backend, sessionID string) string {
	if cwd == "" || sessionID == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	raw, _ := json.Marshal(map[string]string{"backend": backend, "cwd": abs, "session_id": sessionID})
	hash := sha256.Sum256(raw)
	return cachePath("handoffs", hex.EncodeToString(hash[:])+".json")
}

func handoffSummary(path, user, assistant string) string {
	var turns []transcriptTurn
	for _, entry := range readTranscript(path) {
		if turn := entryTurn(entry); turn.Text != "" {
			turns = append(turns, turn)
		}
	}
	if len(turns) == 0 {
		if user != "" {
			turns = append(turns, transcriptTurn{Role: "user", Text: cleanText(user)})
		}
		if assistant != "" {
			turns = append(turns, transcriptTurn{Role: "assistant", Text: cleanText(assistant)})
		}
	}
	if len(turns) > 12 {
		turns = turns[len(turns)-12:]
	}
	var parts []string
	for _, turn := range turns {
		parts = append(parts, turn.Role+": "+turn.Text)
	}
	if len(parts) == 0 {
		return ""
	}
	words := strings.Fields("Recent session context: " + strings.Join(parts, " "))
	if len(words) > 300 {
		words = append(words[:300], "...")
	}
	return strings.Join(words, " ")
}

func writeHandoff(cwd, backend, sessionID, transcript, user, assistant string) {
	path := handoffPath(cwd, backend, sessionID)
	summary := handoffSummary(transcript, user, assistant)
	if path == "" || summary == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	raw, _ := json.MarshalIndent(map[string]any{
		"backend": backend, "cwd": cwd, "session_id": sessionID, "summary": summary,
		"word_limit": 300, "updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	_ = os.WriteFile(path, append(raw, '\n'), 0o600)
}

func WriteHandoff(cwd, backend, sessionID, user, assistant string) {
	writeHandoff(cwd, backend, sessionID, "", user, assistant)
}

func loadHandoff(cwd, backend, sessionID string) string {
	path := handoffPath(cwd, backend, sessionID)
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return ""
	}
	summary := cleanText(stringValue(data, "summary"))
	if summary == "" {
		return ""
	}
	return "[Repowire Session Handoff]\nSummary from the previous turn of this same cwd/session identity:\n" + summary
}

func LoadHandoff(cwd, backend, sessionID string) string {
	return loadHandoff(cwd, backend, sessionID)
}
