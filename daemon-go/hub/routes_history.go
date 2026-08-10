package hub

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

type historyTurn struct {
	Role       string              `json:"role"`
	Text       string              `json:"text"`
	Timestamp  string              `json:"timestamp"`
	SessionID  string              `json:"session_id"`
	TurnID     string              `json:"turn_id"`
	ToolCalls  []map[string]string `json:"tool_calls"`
	LineOffset int                 `json:"-"`
}
type historyResult struct {
	Turns                    []historyTurn
	Status, Backend, Message string
}

func (h *Hub) registerHistoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /peers/{name}/timeline", h.requireAuth(h.handlePeerTimeline))
	mux.HandleFunc("GET /peers/{name}/timeline/search", h.requireAuth(h.handleSearchPeerTimeline))
	mux.HandleFunc("GET /peers/{name}/transcript", h.requireAuth(h.handlePeerTranscript))
}

func (h *Hub) historyPeer(w http.ResponseWriter, r *http.Request) (*proto.Peer, *state.SessionBinding, bool) {
	var circle *string
	if value := r.URL.Query().Get("circle"); value != "" {
		circle = &value
	}
	peer, err := h.reg.GetPeerByName(r.PathValue("name"), circle)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return nil, nil, false
	}
	if peer == nil {
		writeJSONError(w, http.StatusNotFound, "Peer not found: "+r.PathValue("name"))
		return nil, nil, false
	}
	var binding *state.SessionBinding
	if h.store != nil {
		sessionID := r.URL.Query().Get("session_id")
		if sessionID != "" {
			backend, path := string(peer.Backend), peer.Path
			binding, _ = h.store.GetByRuntimeSession(r.Context(), sessionID, &backend, &path)
			if binding == nil {
				binding, _ = h.store.GetByRuntimeSession(r.Context(), sessionID, &backend, nil)
			}
		} else {
			bindings, _ := h.store.ListBindingsByPeer(r.Context(), string(peer.PeerID))
			usable := []*state.SessionBinding{}
			for _, item := range bindings {
				if item.Backend == string(peer.Backend) && item.ProjectPath == peer.Path {
					usable = append(usable, item)
				}
			}
			if len(usable) == 1 {
				binding = usable[0]
			} else if hookID, _ := peer.Metadata["hook_session_id"].(string); hookID != "" {
				for _, item := range usable {
					if item.RuntimeSessionID != nil && *item.RuntimeSessionID == hookID {
						binding = item
						break
					}
				}
			}
		}
	}
	return peer, binding, true
}

func loadHistory(peer *proto.Peer, binding *state.SessionBinding, sessionID string) historyResult {
	backend := string(peer.Backend)
	historyBackend := backend
	if backend == "codex" && peer.Metadata != nil && peer.Metadata["acp"] != nil {
		historyBackend = "codex-acp"
	}
	if backend != "claude-code" && backend != "codex" {
		return historyResult{Turns: []historyTurn{}, Status: "unsupported", Backend: historyBackend, Message: historyBackend + " local history is not supported."}
	}
	paths := []string{}
	if binding != nil && binding.RuntimeSourceURI != nil {
		if path := historySourcePath(*binding.RuntimeSourceURI, backend); path != "" {
			paths = append(paths, path)
		}
		if sessionID == "" && binding.RuntimeSessionID != nil {
			sessionID = *binding.RuntimeSessionID
		}
	}
	if backend == "claude-code" {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(peer.Path, "/", "-"))
		if sessionID != "" {
			paths = append(paths, filepath.Join(dir, sessionID+".jsonl"))
		}
		found, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		paths = append(paths, found...)
	} else {
		home, _ := os.UserHomeDir()
		found, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
		for _, path := range found {
			if codexPathMatches(path, peer.Path) {
				paths = append(paths, path)
			}
		}
	}
	seen := map[string]bool{}
	turns := []historyTurn{}
	for _, path := range paths {
		absolute, _ := filepath.Abs(path)
		if seen[absolute] {
			continue
		}
		seen[absolute] = true
		if backend == "claude-code" {
			turns = append(turns, parseClaudeHistory(path)...)
		} else {
			turns = append(turns, parseCodexHistory(path)...)
		}
	}
	if sessionID != "" {
		filtered := turns[:0]
		for _, turn := range turns {
			if turn.SessionID == sessionID {
				filtered = append(filtered, turn)
			}
		}
		turns = filtered
	}
	sort.Slice(turns, func(i, j int) bool { return historyKey(turns[i]) > historyKey(turns[j]) })
	status, message := "unavailable", "No "+historyBackend+" history found for this peer path."
	if len(turns) > 0 {
		status, message = "available", historyBackend+" history loaded."
	}
	return historyResult{Turns: turns, Status: status, Backend: historyBackend, Message: message}
}

func historySourcePath(raw, backend string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	path, _ := url.PathUnescape(parsed.Path)
	switch parsed.Scheme {
	case "claude-jsonl":
		return filepath.Join(home, ".claude", "projects", strings.TrimPrefix(path, "/"))
	case "codex-rollout":
		return filepath.Join(home, ".codex", "sessions", strings.TrimPrefix(path, "/"))
	case "file":
		return expandHome(path, home)
	case "":
		return expandHome(raw, home)
	}
	// Compatibility with backend-specific locator schemes whose path payload is
	// still relative to the runtime's standard history root.
	if backend == "claude-code" && strings.HasPrefix(parsed.Scheme, "claude") {
		return filepath.Join(home, ".claude", "projects", strings.TrimPrefix(path, "/"))
	}
	if backend == "codex" && strings.HasPrefix(parsed.Scheme, "codex") {
		return filepath.Join(home, ".codex", "sessions", strings.TrimPrefix(path, "/"))
	}
	return ""
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}
func codexPathMatches(path, cwd string) bool {
	for _, entry := range readJSONLines(path) {
		if typ, _ := entry["type"].(string); typ != "session_meta" && typ != "turn_context" {
			continue
		}
		payload, _ := entry["payload"].(map[string]any)
		if value, _ := payload["cwd"].(string); samePath(value, cwd) {
			return true
		}
	}
	return false
}
func samePath(left, right string) bool {
	a, e1 := filepath.Abs(left)
	b, e2 := filepath.Abs(right)
	return e1 == nil && e2 == nil && filepath.Clean(a) == filepath.Clean(b)
}
func readJSONLines(path string) []map[string]any {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var out []map[string]any
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) == nil {
			out = append(out, item)
		}
	}
	return out
}
func contentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if item, ok := value.(map[string]any); ok {
		text, _ := item["text"].(string)
		return text
	}
	values, _ := value.([]any)
	parts := []string{}
	for _, raw := range values {
		if text, ok := raw.(string); ok {
			parts = append(parts, text)
			continue
		}
		item, _ := raw.(map[string]any)
		typ, _ := item["type"].(string)
		if typ == "text" || typ == "input_text" || typ == "output_text" {
			if text, _ := item["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
func toolSummary(value any) string {
	raw, _ := json.Marshal(value)
	text := string(raw)
	if len(text) > 300 {
		text = text[:297] + "..."
	}
	return text
}

func parseClaudeHistory(path string) []historyTurn {
	entries := readJSONLines(path)
	sessionFallback := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	turns := []historyTurn{}
	var assistant *historyTurn
	flush := func() {
		if assistant != nil && assistant.Text != "" {
			turns = append(turns, *assistant)
		}
		assistant = nil
	}
	for offset, entry := range entries {
		typ, _ := entry["type"].(string)
		if typ != "user" && typ != "assistant" {
			continue
		}
		message, _ := entry["message"].(map[string]any)
		content := message["content"]
		if typ == "user" {
			if values, ok := content.([]any); ok && len(values) > 0 {
				allTools := true
				for _, raw := range values {
					item, _ := raw.(map[string]any)
					if item["type"] != "tool_result" {
						allTools = false
					}
				}
				if allTools {
					continue
				}
			}
			flush()
			text := contentText(content)
			if text == "" {
				continue
			}
			session, _ := entry["sessionId"].(string)
			if session == "" {
				session = sessionFallback
			}
			timestamp, _ := entry["timestamp"].(string)
			turns = append(turns, historyTurn{Role: "user", Text: text, Timestamp: timestamp, SessionID: session, TurnID: fmt.Sprintf("history:%s:%d", session, offset), ToolCalls: []map[string]string{}, LineOffset: offset})
			continue
		}
		session, _ := entry["sessionId"].(string)
		if session == "" {
			session = sessionFallback
		}
		if assistant == nil {
			id, _ := entry["uuid"].(string)
			if id == "" {
				id, _ = entry["id"].(string)
			}
			if id == "" {
				id, _ = message["id"].(string)
			}
			if id == "" {
				id = fmt.Sprintf("history:%s:%d", session, offset)
			}
			assistant = &historyTurn{Role: "assistant", SessionID: session, TurnID: id, ToolCalls: []map[string]string{}}
		}
		if values, ok := content.([]any); ok {
			for _, raw := range values {
				item, _ := raw.(map[string]any)
				if item["type"] == "tool_use" {
					name, _ := item["name"].(string)
					if name == "" {
						name = "unknown"
					}
					assistant.ToolCalls = append(assistant.ToolCalls, map[string]string{"name": name, "input": toolSummary(item["input"])})
				}
			}
		}
		if text := contentText(content); text != "" {
			assistant.Text = text
			assistant.Timestamp, _ = entry["timestamp"].(string)
			assistant.LineOffset = offset
		}
	}
	flush()
	return turns
}
func parseCodexHistory(path string) []historyTurn {
	entries := readJSONLines(path)
	session := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), "rollout-")
	turnID := ""
	var pending *historyTurn
	tools := []map[string]string{}
	out := []historyTurn{}
	for offset, entry := range entries {
		typ, _ := entry["type"].(string)
		payload, _ := entry["payload"].(map[string]any)
		if typ == "session_meta" {
			if id, _ := payload["id"].(string); id != "" {
				session = id
			}
			continue
		}
		if typ == "turn_context" {
			if id, _ := payload["turn_id"].(string); id != "" {
				turnID = id
			}
			if pending != nil {
				out = append(out, *pending)
				pending = nil
			}
			continue
		}
		if typ != "response_item" {
			continue
		}
		payloadType, _ := payload["type"].(string)
		if payloadType == "function_call" {
			name, _ := payload["name"].(string)
			if name == "" {
				name = "unknown"
			}
			args := payload["arguments"]
			if text, ok := args.(string); ok {
				var parsed any
				if json.Unmarshal([]byte(text), &parsed) == nil {
					args = parsed
				}
			}
			tools = append(tools, map[string]string{"name": name, "input": toolSummary(args)})
			continue
		}
		if payloadType != "message" {
			continue
		}
		role, _ := payload["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		text := contentText(payload["content"])
		if text == "" {
			continue
		}
		timestamp, _ := entry["timestamp"].(string)
		if role == "user" {
			pending = &historyTurn{Role: "user", Text: text, Timestamp: timestamp, SessionID: session, TurnID: fmt.Sprintf("history:%s:%d", session, offset), ToolCalls: []map[string]string{}, LineOffset: offset}
			continue
		}
		id := turnID
		if id == "" {
			id = fmt.Sprintf("history:%s:%d", session, offset)
		}
		out = append(out, historyTurn{Role: "assistant", Text: text, Timestamp: timestamp, SessionID: session, TurnID: id, ToolCalls: tools, LineOffset: offset})
		tools = []map[string]string{}
	}
	if pending != nil {
		out = append(out, *pending)
	}
	return out
}
func historyKey(turn historyTurn) string {
	return turn.Timestamp + "|" + turn.SessionID + "|" + fmt.Sprintf("%012d", turn.LineOffset)
}

func (h *Hub) handlePeerTranscript(w http.ResponseWriter, r *http.Request) {
	peer, binding, ok := h.historyPeer(w, r)
	if !ok {
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	history := loadHistory(peer, binding, sessionID)
	limit := queryInt(r, "limit", 50, 1, 500)
	turns, next := pageHistory(history.Turns, limit, r.URL.Query().Get("before"))
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns, "next_before": next, "history_status": history.Status, "history_backend": history.Backend, "history_message": history.Message, "history_source": historySource(binding), "repowire_session_id": bindingField(binding, func(b *state.SessionBinding) any { return b.RepowireSessionID }), "binding_status": bindingField(binding, func(b *state.SessionBinding) any { return b.Status }), "runtime_session_id": firstHistorySession(binding, sessionID)})
}
func pageHistory(turns []historyTurn, limit int, before string) ([]historyTurn, any) {
	filtered := turns
	if before != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(before); err == nil {
			cursor := string(raw)
			filtered = []historyTurn{}
			for _, turn := range turns {
				if historyKey(turn) < cursor {
					filtered = append(filtered, turn)
				}
			}
		}
	}
	more := len(filtered) > limit
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if more && len(filtered) > 0 {
		return filtered, base64.RawURLEncoding.EncodeToString([]byte(historyKey(filtered[len(filtered)-1])))
	}
	return filtered, nil
}

type timelineItem struct {
	ID        string              `json:"id"`
	Kind      string              `json:"kind"`
	Source    string              `json:"source"`
	Timestamp string              `json:"timestamp"`
	SessionID string              `json:"session_id"`
	TurnID    string              `json:"turn_id"`
	Role      string              `json:"role"`
	Text      string              `json:"text"`
	ToolCalls []map[string]string `json:"tool_calls"`
	PeerID    any                 `json:"peer_id"`
	Peer      any                 `json:"peer"`
	EventIDs  []string            `json:"event_ids"`
}

func (h *Hub) timeline(peer *proto.Peer, history historyResult, sessionID string) []timelineItem {
	items := []timelineItem{}
	final := map[string]bool{}
	for _, event := range h.reg.GetEvents() {
		typ, _ := event["type"].(string)
		if typ != "chat_turn" {
			continue
		}
		if !eventForPeer(event, peer) {
			continue
		}
		sid, _ := event["session_id"].(string)
		if sid == "" {
			sid = "legacy"
		}
		if sessionID != "" && sid != sessionID {
			continue
		}
		turnID, _ := event["turn_id"].(string)
		if turnID == "" {
			continue
		}
		key := sid + ":" + turnID
		final[key] = true
		items = append(items, timelineEvent(event, sid, turnID))
	}
	for _, turn := range history.Turns {
		if sessionID != "" && turn.SessionID != sessionID {
			continue
		}
		if final[turn.SessionID+":"+turn.TurnID] {
			continue
		}
		items = append(items, timelineItem{ID: "history:" + turn.SessionID + ":" + turn.TurnID, Kind: "turn", Source: "history", Timestamp: turn.Timestamp, SessionID: turn.SessionID, TurnID: turn.TurnID, Role: turn.Role, Text: turn.Text, ToolCalls: turn.ToolCalls, EventIDs: []string{}})
	}
	items = append(items, h.deltaGroups(peer, sessionID, final)...)
	sort.Slice(items, func(i, j int) bool {
		return items[i].Timestamp+items[i].SessionID+items[i].TurnID < items[j].Timestamp+items[j].SessionID+items[j].TurnID
	})
	return items
}
func eventForPeer(event map[string]any, peer *proto.Peer) bool {
	if id, _ := event["peer_id"].(string); id != "" {
		return id == string(peer.PeerID)
	}
	name, _ := event["peer"].(string)
	return name == string(peer.DisplayName) || name == string(peer.PeerID)
}
func timelineEvent(event map[string]any, sid, turnID string) timelineItem {
	tools := []map[string]string{}
	if values, ok := event["tool_calls"].([]any); ok {
		for _, raw := range values {
			item, _ := raw.(map[string]any)
			tools = append(tools, map[string]string{"name": fmt.Sprint(item["name"]), "input": fmt.Sprint(item["input"])})
		}
	}
	id, _ := event["id"].(string)
	timestamp, _ := event["timestamp"].(string)
	role, _ := event["role"].(string)
	text, _ := event["text"].(string)
	return timelineItem{ID: id, Kind: "turn", Source: "realtime", Timestamp: timestamp, SessionID: sid, TurnID: turnID, Role: role, Text: text, ToolCalls: tools, PeerID: event["peer_id"], Peer: event["peer"], EventIDs: []string{id}}
}
func (h *Hub) deltaGroups(peer *proto.Peer, sessionID string, hidden map[string]bool) []timelineItem {
	groups := map[string]*timelineItem{}
	for _, event := range h.reg.GetEvents() {
		if event["type"] != "chat_turn_delta" || !eventForPeer(event, peer) {
			continue
		}
		sid, _ := event["session_id"].(string)
		if sid == "" {
			sid = "legacy"
		}
		if sessionID != "" && sid != sessionID {
			continue
		}
		turnID, _ := event["turn_id"].(string)
		key := sid + ":" + turnID
		if turnID == "" || hidden[key] {
			continue
		}
		item := groups[key]
		if item == nil {
			item = &timelineItem{ID: "delta:" + key, Kind: "delta_group", Source: "realtime", SessionID: sid, TurnID: turnID, Role: "assistant", ToolCalls: []map[string]string{}, EventIDs: []string{}, PeerID: event["peer_id"], Peer: event["peer"]}
			groups[key] = item
		}
		if timestamp, _ := event["timestamp"].(string); timestamp > item.Timestamp {
			item.Timestamp = timestamp
		}
		if id, _ := event["id"].(string); id != "" {
			item.EventIDs = append(item.EventIDs, id)
		}
		if kind, _ := event["kind"].(string); kind == "tool_use" {
			if call, ok := event["tool_call"].(map[string]any); ok {
				item.ToolCalls = append(item.ToolCalls, map[string]string{"name": fmt.Sprint(call["name"]), "input": fmt.Sprint(call["input"])})
			}
		} else if text, _ := event["text"].(string); text != "" {
			if item.Text != "" {
				item.Text += "\n\n"
			}
			item.Text += text
		}
	}
	out := []timelineItem{}
	for _, item := range groups {
		if item.Text != "" || len(item.ToolCalls) > 0 {
			out = append(out, *item)
		}
	}
	return out
}
func (h *Hub) handlePeerTimeline(w http.ResponseWriter, r *http.Request) {
	peer, binding, ok := h.historyPeer(w, r)
	if !ok {
		return
	}
	sid := r.URL.Query().Get("session_id")
	history := loadHistory(peer, binding, sid)
	items := h.timeline(peer, history, sid)
	limit := queryInt(r, "limit", 100, 1, 500)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer_id": peer.PeerID, "peer_name": peer.DisplayName, "session_id": nilIfEmpty(sid), "history_status": history.Status, "history_backend": history.Backend, "history_message": history.Message, "history_source": historySource(binding), "repowire_session_id": bindingField(binding, func(b *state.SessionBinding) any { return b.RepowireSessionID }), "binding_status": bindingField(binding, func(b *state.SessionBinding) any { return b.Status }), "runtime_session_id": firstHistorySession(binding, sid), "items": items})
}
func (h *Hub) handleSearchPeerTimeline(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSONError(w, http.StatusUnprocessableEntity, "q is required")
		return
	}
	peer, binding, ok := h.historyPeer(w, r)
	if !ok {
		return
	}
	sid := r.URL.Query().Get("session_id")
	history := loadHistory(peer, binding, sid)
	items := h.timeline(peer, history, sid)
	limit := queryInt(r, "limit", 20, 1, 100)
	results := []map[string]any{}
	needle := strings.ToLower(query)
	for i := len(items) - 1; i >= 0 && len(results) < limit; i-- {
		lower := strings.ToLower(items[i].Text)
		index := strings.Index(lower, needle)
		if index < 0 {
			continue
		}
		start := index - 80
		if start < 0 {
			start = 0
		}
		end := index + len(query) + 80
		if end > len(items[i].Text) {
			end = len(items[i].Text)
		}
		snippet := items[i].Text[start:end]
		if start > 0 {
			snippet = "..." + snippet
		}
		if end < len(items[i].Text) {
			snippet += "..."
		}
		results = append(results, map[string]any{"cursor": items[i].SessionID + ":" + items[i].TurnID, "target_id": "turn-" + items[i].SessionID + "-" + items[i].TurnID, "item": items[i], "match": map[string]any{"start": index, "end": index + len(query), "snippet": snippet}})
	}
	degraded := history.Status != "available"
	message := ""
	if degraded {
		message = history.Message
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer_id": peer.PeerID, "peer_name": peer.DisplayName, "query": query, "session_id": nilIfEmpty(sid), "history_status": history.Status, "history_backend": history.Backend, "history_message": history.Message, "history_source": historySource(binding), "degraded": degraded, "degradation_message": message, "repowire_session_id": bindingField(binding, func(b *state.SessionBinding) any { return b.RepowireSessionID }), "binding_status": bindingField(binding, func(b *state.SessionBinding) any { return b.Status }), "runtime_session_id": firstHistorySession(binding, sid), "results": results})
}
func queryInt(r *http.Request, key string, fallback, min, max int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
func historySource(binding *state.SessionBinding) string {
	if binding != nil {
		return "session_binding"
	}
	return "peer_path"
}
func bindingField(binding *state.SessionBinding, extract func(*state.SessionBinding) any) any {
	if binding == nil {
		return nil
	}
	return extract(binding)
}
func firstHistorySession(binding *state.SessionBinding, fallback string) any {
	if binding != nil && binding.RuntimeSessionID != nil {
		return *binding.RuntimeSessionID
	}
	return nilIfEmpty(fallback)
}
