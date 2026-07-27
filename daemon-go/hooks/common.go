package hooks

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

const (
	hookVersion           = 1
	paneUnsafeStrikeLimit = 3
	hintTTL               = 5 * time.Minute
)

var shellCommands = map[string]bool{
	"bash": true, "dash": true, "fish": true, "ksh": true, "sh": true,
	"tcsh": true, "zsh": true,
}

type Payload struct {
	Event          string
	SessionID      string
	CWD            string
	TranscriptPath string
	ResponseText   string
	Model          string
	Backend        string
}

func Normalize(raw map[string]any, backend string) Payload {
	event, _ := raw["hook_event_name"].(string)
	switch event {
	case "AfterAgent", "StopFailure":
		event = "Stop"
	case "BeforeAgent":
		event = "UserPromptSubmit"
	}
	response := ""
	for _, key := range []string{"prompt_response", "last_assistant_message", "final_response"} {
		if value, ok := raw[key]; ok && value != nil {
			response, _ = value.(string)
			break
		}
	}
	stringField := func(key string) string { value, _ := raw[key].(string); return value }
	return Payload{
		Event: event, SessionID: stringField("session_id"), CWD: stringField("cwd"),
		TranscriptPath: stringField("transcript_path"), ResponseText: response,
		Model: extractModel(raw["model"]), Backend: backend,
	}
}

func extractModel(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if m, ok := value.(map[string]any); ok {
		for _, key := range []string{"modelID", "model_id", "id", "name"} {
			if s, ok := m[key].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func readInput() (map[string]any, error) {
	var in map[string]any
	err := json.NewDecoder(os.Stdin).Decode(&in)
	return in, err
}

func printJSON(value any) {
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func hookOutput(backend string) {
	if backend == "gemini" || backend == "antigravity" {
		printJSON(map[string]string{"decision": "allow"})
	}
}

func homePath(parts ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func cachePath(parts ...string) string {
	return filepath.Join(append([]string{homePath(".cache", "repowire")}, parts...)...)
}

func paneToken(paneID string) string {
	value := strings.NewReplacer("%", "", "/", "", "\\", "").Replace(paneID)
	if value == "" {
		return "unknown"
	}
	return value
}

func paneLogsDir() string {
	dir := cachePath("logs")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func wsHookPath(paneID, suffix string) string {
	return filepath.Join(paneLogsDir(), "ws-hook-"+paneToken(paneID)+suffix)
}

func readMetadata(paneID string) map[string]any {
	var out map[string]any
	raw, err := os.ReadFile(wsHookPath(paneID, ".meta.json"))
	if err != nil || json.Unmarshal(raw, &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func writeMetadata(paneID string, data map[string]any) error {
	if paneID == "" {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	path := wsHookPath(paneID, ".meta.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if cwd := stringValue(data, "cwd"); cwd != "" {
		_ = os.WriteFile(wsHookPath(paneID, ".cwd"), []byte(cwd), 0o600)
	}
	cert, certOK := data["birth_certificate"].(map[string]any)
	backend, agentPID := stringValue(data, "backend"), intFromAny(data["agent_pid"])
	if certOK && backend != "" && agentPID > 0 {
		safeBackend := strings.NewReplacer("/", "-", "\\", "-").Replace(backend)
		raw, _ := json.Marshal(cert)
		_ = os.WriteFile(filepath.Join(paneLogsDir(), fmt.Sprintf("birth-%s-%d-%s.json", safeBackend, agentPID, paneToken(paneID))), raw, 0o600)
	}
	return nil
}

func clearRuntime(paneID string) {
	for _, suffix := range []string{".pid", ".meta.json", ".cwd"} {
		_ = os.Remove(wsHookPath(paneID, suffix))
	}
}

func daemonConnection() (string, string) {
	if cfg, err := config.Load(); err == nil {
		return fmt.Sprintf("http://%s:%d", cfg.Daemon.Host, cfg.Daemon.Port), cfg.Daemon.AuthToken
	}
	host := firstNonempty(os.Getenv("REPOWIRE_DAEMON_HOST"), "127.0.0.1")
	port := firstNonempty(os.Getenv("REPOWIRE_DAEMON_PORT"), "8377")
	return "http://" + host + ":" + port, os.Getenv("REPOWIRE_AUTH_TOKEN")
}

func daemonRequest(method, path string, payload any, timeout time.Duration) (int, map[string]any) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil
		}
		body = bytes.NewReader(raw)
	}
	baseURL, authToken := daemonConnection()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		return 0, nil
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	var result map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result)
	return resp.StatusCode, result
}

func daemonGet(path string) map[string]any {
	status, result := daemonRequest(http.MethodGet, path, nil, 2*time.Second)
	if status < 200 || status >= 300 {
		return nil
	}
	return result
}

func daemonPost(path string, payload any) map[string]any {
	status, result := daemonRequest(http.MethodPost, path, payload, 2*time.Second)
	if status < 200 || status >= 300 {
		return nil
	}
	return result
}

func updateStatus(identifier, status, turnState, model string, byPane bool) bool {
	if identifier == "" {
		return false
	}
	payload := map[string]any{"name": identifier, "status": status}
	if byPane {
		payload["pane_id"] = identifier
		delete(payload, "name")
	}
	if turnState != "" {
		payload["turn_state"] = turnState
	}
	if model != "" {
		payload["model"] = model
		source := map[string]string{"idle": "hook_stop", "working": "hook_user_prompt_submit"}[turnState]
		if source == "" {
			source = "hook_session_update"
		}
		payload["metadata"] = map[string]any{
			"model_source":      source,
			"model_observed_at": time.Now().UTC().Format(time.RFC3339Nano),
		}
	}
	return daemonPost("/session/update", payload) != nil
}

type tmuxInfo struct {
	PaneID, SessionName, WindowName, WindowID string
}

func configuredCircleBoundary() (proto.CircleBoundary, error) {
	cfg, err := config.Load()
	return cfg.Daemon.CircleBoundary, err
}

func tmuxSession(info tmuxInfo) string {
	if info.SessionName == "" || info.WindowName == "" {
		return ""
	}
	return info.SessionName + ":" + info.WindowName
}

func getPaneID() string {
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		return pane
	}
	parents := processParents(os.Getppid())
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			pid, _ := strconv.Atoi(fields[1])
			if parents[pid] {
				return fields[0]
			}
		}
	}
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err = exec.Command("tmux", "display-message", "-p", "#{pane_id}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func getTmuxInfo() tmuxInfo {
	pane := getPaneID()
	if pane == "" {
		return tmuxInfo{}
	}
	out, err := exec.Command("tmux", "display-message", "-t", pane, "-p", "#{session_name}\t#{window_name}\t#{window_id}").Output()
	if err != nil {
		return tmuxInfo{PaneID: pane}
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "\t")
	info := tmuxInfo{PaneID: pane}
	if len(parts) == 3 {
		info.SessionName, info.WindowName, info.WindowID = parts[0], parts[1], parts[2]
	}
	return info
}

func processSnapshot() (map[int][]int, map[int]string, map[int]int, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, nil, nil, err
	}
	children := map[int][]int{}
	commands := map[int]string{}
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parents[pid] = ppid
		children[ppid] = append(children[ppid], pid)
		commands[pid] = strings.ToLower(filepath.Base(strings.Join(fields[2:], " ")))
	}
	return children, commands, parents, nil
}

func processParents(start int) map[int]bool {
	_, _, parents, err := processSnapshot()
	if err != nil {
		return nil
	}
	out := map[int]bool{}
	for pid, n := start, 0; pid > 1 && n < 128; n++ {
		if out[pid] {
			break
		}
		out[pid] = true
		pid = parents[pid]
	}
	return out
}

func tmuxValue(paneID, format string) (string, bool) {
	out, err := exec.Command("tmux", "display-message", "-t", paneID, "-p", format).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", true
		}
		return "", false
	}
	value := strings.TrimSpace(string(out))
	return value, value != ""
}

func panePID(paneID string) (int, bool) {
	value, conclusive := tmuxValue(paneID, "#{pane_pid}")
	if !conclusive || value == "" {
		return 0, conclusive
	}
	pid, err := strconv.Atoi(value)
	return pid, err == nil
}

func commandIsShell(pid int) (bool, bool) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false, false
	}
	return shellCommands[strings.TrimPrefix(strings.ToLower(filepath.Base(strings.TrimSpace(string(out)))), "-")], true
}

func findAgentPID(paneID string) int {
	_, pid := capturePaneBaseline(paneID)
	return pid
}

func capturePaneBaseline(paneID string) (string, int) {
	root, ok := panePID(paneID)
	if !ok {
		return "", 0
	}
	children, commands, _, err := processSnapshot()
	if err != nil {
		return "", 0
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		cmd := strings.TrimPrefix(commands[pid], "-")
		if cmd != "" && !shellCommands[cmd] {
			return cmd, pid
		}
		queue = append(queue, children[pid]...)
	}
	return "", 0
}

// paneSafe returns nil when the probe itself was inconclusive.
func paneSafe(paneID, expectedCommand string) *bool {
	root, ok := panePID(paneID)
	if !ok {
		return nil
	}
	if root <= 0 {
		v := false
		return &v
	}
	children, commands, _, err := processSnapshot()
	if err != nil {
		return nil
	}
	if expectedCommand == "" {
		command, conclusive := tmuxValue(paneID, "#{pane_current_command}")
		if !conclusive {
			return nil
		}
		command = strings.TrimPrefix(strings.ToLower(filepath.Base(command)), "-")
		v := command != "" && !shellCommands[command]
		return &v
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		cmd := strings.TrimPrefix(commands[pid], "-")
		if cmd == expectedCommand {
			v := true
			return &v
		}
		queue = append(queue, children[pid]...)
	}
	v := false
	return &v
}

func findExpectedAgentPID(paneID, expectedCommand string) int {
	if expectedCommand == "" {
		return findAgentPID(paneID)
	}
	root, ok := panePID(paneID)
	if !ok {
		return 0
	}
	children, commands, _, err := processSnapshot()
	if err != nil {
		return 0
	}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if strings.TrimPrefix(commands[pid], "-") == expectedCommand {
			return pid
		}
		queue = append(queue, children[pid]...)
	}
	return 0
}

func injectText(paneID, text string) bool {
	copyMode, _ := tmuxValue(paneID, "#{pane_in_mode}")
	if copyMode == "1" {
		_ = exec.Command("tmux", "send-keys", "-t", paneID, "-X", "cancel").Run()
	}
	if exec.Command("tmux", "send-keys", "-t", paneID, "-l", text).Run() != nil {
		return false
	}
	time.Sleep(min(500*time.Millisecond+time.Duration(len(text))*time.Second/4000, 1500*time.Millisecond))
	if exec.Command("tmux", "send-keys", "-t", paneID, "-H", "1b", "5b", "32", "30", "31", "7e").Run() != nil {
		return false
	}
	time.Sleep(100 * time.Millisecond)
	return exec.Command("tmux", "send-keys", "-t", paneID, "Enter").Run() == nil
}

func consumeSpawnHint(path, backend string) map[string]any {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	hash := sha256.Sum256([]byte(abs + "::" + backend))
	key := hex.EncodeToString(hash[:])[:16]
	target := cachePath("spawn-hints", key+".json")
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil
	}
	var queue []map[string]any
	if json.Unmarshal(raw, &queue) != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) == nil {
			queue = []map[string]any{one}
		}
	}
	var selected map[string]any
	var fresh []map[string]any
	now := time.Now()
	for _, item := range queue {
		ts, ok := item["ts"].(float64)
		circle, valid := item["circle"].(string)
		if !ok || !valid || circle == "" || now.Sub(time.Unix(int64(ts), 0)) > hintTTL {
			continue
		}
		if selected == nil {
			selected = item
		} else {
			fresh = append(fresh, item)
		}
	}
	if len(fresh) == 0 {
		_ = os.Remove(target)
	} else if raw, err := json.Marshal(fresh); err == nil {
		_ = os.WriteFile(target, raw, 0o600)
	}
	return selected
}

func markOffline(peerID, reason, source, detail string) {
	if peerID == "" {
		return
	}
	daemonPost("/peers/"+url.PathEscape(peerID)+"/offline", map[string]any{
		"reason": reason, "source": source, "detail": detail, "terminal": true,
	})
}

func stringValue(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func boolValue(m map[string]any, key string) bool {
	value, _ := m[key].(bool)
	return value
}

func getDisplayName() string {
	if value := os.Getenv("REPOWIRE_DISPLAY_NAME"); value != "" {
		return value
	}
	cwd, _ := os.Getwd()
	return filepath.Base(cwd)
}

func parentPID(pid int) int {
	_, _, parents, err := processSnapshot()
	if err != nil {
		return 0
	}
	return parents[pid]
}

func gitOutput(cwd string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func compactJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func errf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "repowire: "+format+"\n", args...)
}

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)[:n]
	}
	return hex.EncodeToString(buf)[:n]
}
