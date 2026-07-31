package hooks

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/repowire/repowire/daemon-go/proto"
)

func startWSHook(paneID, peerID, displayName, backend, cwd string, agentPID int, lock *os.File) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(wsHookPath(paneID, ".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	env := append(os.Environ(),
		"REPOWIRE_DISPLAY_NAME="+displayName,
		"REPOWIRE_PEER_ID="+peerID,
		"REPOWIRE_AGENT_PID="+strconv.Itoa(agentPID),
		"REPOWIRE_BACKEND="+backend,
		"REPOWIRE_HOOK_LOCK_FD=3",
		"TMUX_PANE="+paneID,
	)
	cmd := exec.Command(executable, "ws-hook")
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.ExtraFiles = []*os.File{lock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	path := wsHookPath(paneID, ".pid")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return os.Rename(tmp, path)
}

func killPIDFile(path string, signal syscall.Signal) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err == nil && pid > 0 {
		_ = syscall.Kill(pid, signal)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errorsIsPermission(err)
}

func errorsIsPermission(err error) bool {
	return err == syscall.EPERM
}

func maybeRespawn(paneID, backend, cwd string) bool {
	if paneID == "" {
		return false
	}
	raw, err := os.ReadFile(wsHookPath(paneID, ".pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pidAlive(pid) {
		return false
	}
	lock, err := os.OpenFile(wsHookPath(paneID, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false
	}
	meta := ReadPaneRuntimeMetadata(paneID)
	metaCWD := stringValue(meta, "cwd")
	metaBackend := firstNonempty(stringValue(meta, "backend"), "claude-code")
	displayName := stringValue(meta, "display_name")
	if metaCWD == "" || displayName == "" || backend == "" || cwd == "" || metaCWD != cwd || metaBackend != backend {
		return false
	}
	agentPID := intFromAny(meta["agent_pid"])
	return startWSHook(paneID, stringValue(meta, "peer_id"), displayName, metaBackend, metaCWD, agentPID, lock) == nil
}

// ReconcileWSHook replaces a disconnected pane hook after the daemon has
// independently proven pane ownership. It is the apply half of peer rehook.
func ReconcileWSHook(paneID, peerID, displayName, backend, cwd string, agentPID int) (bool, error) {
	if paneID == "" || peerID == "" || cwd == "" {
		return false, fmt.Errorf("incomplete hook identity")
	}
	lock, err := os.OpenFile(wsHookPath(paneID, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		killPIDFile(wsHookPath(paneID, ".pid"), syscall.SIGTERM)
		acquired := false
		for i := 0; i < 20; i++ {
			if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				acquired = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !acquired {
			return false, fmt.Errorf("ws-hook lock remained contested")
		}
	}
	meta := ReadPaneRuntimeMetadata(paneID)
	delete(meta, "birth_certificate")
	meta["backend"], meta["cwd"], meta["display_name"], meta["peer_id"], meta["agent_pid"] = backend, cwd, displayName, peerID, agentPID
	if err := writeMetadata(paneID, meta); err != nil {
		return false, err
	}
	if err := startWSHook(paneID, peerID, displayName, backend, cwd, agentPID, lock); err != nil {
		return false, err
	}
	return true, nil
}

func intFromAny(value any) int {
	switch value := value.(type) {
	case float64:
		return int(value)
	case int:
		return value
	case string:
		n, _ := strconv.Atoi(value)
		return n
	default:
		return 0
	}
}

func RunWS() int {
	if fd, err := strconv.Atoi(os.Getenv("REPOWIRE_HOOK_LOCK_FD")); err == nil && fd >= 0 {
		syscall.CloseOnExec(fd)
	}
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		errf("ws-hook: TMUX_PANE not set")
		return 1
	}
	info := getTmuxInfo()
	boundary, circle, source, err := tmuxPlacement(info)
	if err != nil {
		errf("ws-hook: load circle boundary: %v", err)
		return 1
	}
	if circle == "" {
		errf("ws-hook: no tmux circle; spawn the peer with --circle")
		return 1
	}
	displayName := getDisplayName()
	backend := firstNonempty(os.Getenv("REPOWIRE_BACKEND"), "claude-code")
	agentPID, _ := strconv.Atoi(os.Getenv("REPOWIRE_AGENT_PID"))
	cwd, _ := os.Getwd()
	peerID := os.Getenv("REPOWIRE_PEER_ID")
	lastPeerID := peerID
	expectedCommand, replacementPID := capturePaneBaseline(paneID)
	if replacementPID > 0 {
		agentPID = replacementPID
	}
	if expectedCommand == "" {
		if command, ok := tmuxValue(paneID, "#{pane_current_command}"); ok {
			command = strings.TrimPrefix(strings.ToLower(command), "-")
			if !shellCommands[command] {
				expectedCommand = command
			}
		}
	}
	unsafeStrikes, failures := 0, 0
	for {
		if agentPID > 0 && !pidAlive(agentPID) {
			if replacement := findExpectedAgentPID(paneID, expectedCommand); replacement > 0 && replacement != agentPID {
				agentPID = replacement
			} else if safe := paneSafe(paneID, expectedCommand); safe != nil && !*safe {
				markOffline(lastPeerID, "agent_exited", "ws_hook", fmt.Sprintf("agent pid %d for pane %s exited", agentPID, paneID))
				ClearPaneRuntimeState(paneID)
				return 0
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		baseURL, authToken := daemonConnection()
		wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/ws"
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			cancel()
			failures++
			time.Sleep(backoff(failures))
			continue
		}
		connect := map[string]any{
			"type": "connect", "display_name": displayName, "circle": circle,
			"backend": backend, "path": cwd, "pane_id": paneID, "circle_source": source,
			"hook_version": hookVersion, "capabilities": []string{"delivery_receipts"},
		}
		if target := tmuxSession(info); target != "" {
			connect["tmux_session"] = target
		}
		if peerID != "" {
			connect["peer_id"] = peerID
		}
		if agentPID > 0 {
			connect["agent_pid"] = agentPID
		}
		if model := os.Getenv("REPOWIRE_MODEL"); model != "" {
			connect["model"] = model
		}
		if authToken != "" {
			connect["auth_token"] = authToken
		}
		if err := wsjson.Write(ctx, conn, connect); err != nil {
			_ = conn.CloseNow()
			cancel()
			failures++
			time.Sleep(backoff(failures))
			continue
		}
		var response map[string]any
		if err := wsjson.Read(ctx, conn, &response); err != nil {
			_ = conn.CloseNow()
			cancel()
			failures++
			time.Sleep(backoff(failures))
			continue
		}
		if stringValue(response, "type") == "error" && stringValue(response, "code") == "peer_retired" {
			_ = conn.Close(websocket.StatusNormalClosure, "retired")
			cancel()
			ClearPaneRuntimeState(paneID)
			return 0
		}
		if stringValue(response, "type") != "connected" {
			_ = conn.CloseNow()
			cancel()
			failures++
			time.Sleep(backoff(failures))
			continue
		}
		failures = 0
		lastPeerID = stringValue(response, "session_id")
		meta := ReadPaneRuntimeMetadata(paneID)
		meta["backend"], meta["cwd"], meta["display_name"], meta["peer_id"] = backend, cwd, firstNonempty(stringValue(response, "display_name"), displayName), lastPeerID
		if agentPID > 0 {
			meta["agent_pid"] = agentPID
		}
		_ = writeMetadata(paneID, meta)

		exited := make(chan bool, 1)
		if agentPID > 0 {
			go watchAgent(ctx, conn, paneID, agentPID, expectedCommand, exited)
		}
		stop := false
		for !stop {
			var message map[string]any
			if err := wsjson.Read(ctx, conn, &message); err != nil {
				select {
				case gone := <-exited:
					if gone {
						markOffline(lastPeerID, "agent_exited", "ws_hook", fmt.Sprintf("agent pid %d for pane %s exited", agentPID, paneID))
						ClearPaneRuntimeState(paneID)
						_ = conn.CloseNow()
						cancel()
						return 0
					}
				default:
				}
				break
			}
			stop, unsafeStrikes = handleMessage(ctx, conn, message, paneID, expectedCommand, boundary, unsafeStrikes)
		}
		_ = conn.CloseNow()
		cancel()
		if stop {
			ClearPaneRuntimeState(paneID)
			return 0
		}
		time.Sleep(2 * time.Second)
	}
}

func watchAgent(ctx context.Context, conn *websocket.Conn, paneID string, agentPID int, expectedCommand string, exited chan<- bool) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	watched := agentPID
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if pidAlive(watched) {
				continue
			}
			if replacement := findExpectedAgentPID(paneID, expectedCommand); replacement > 0 && replacement != watched {
				watched = replacement
				continue
			}
			if safe := paneSafe(paneID, expectedCommand); safe != nil && !*safe {
				select {
				case exited <- true:
				default:
				}
				_ = conn.Close(websocket.StatusNormalClosure, "agent exited")
				return
			}
		}
	}
}

func handleMessage(ctx context.Context, conn *websocket.Conn, data map[string]any, paneID, expectedCommand string, boundary proto.CircleBoundary, unsafeStrikes int) (bool, int) {
	typ := stringValue(data, "type")
	if typ == "ping" {
		safe := paneSafe(paneID, expectedCommand)
		info := getTmuxInfo()
		pong := map[string]any{"type": "pong", "circle": proto.TmuxCircle(boundary, info.SessionName, info.WindowID)}
		if safe != nil {
			pong["pane_alive"] = *safe
		}
		_ = wsjson.Write(ctx, conn, pong)
		if safe == nil {
			return false, unsafeStrikes
		}
		if *safe {
			return false, 0
		}
		unsafeStrikes++
		return unsafeStrikes >= paneUnsafeStrikeLimit, unsafeStrikes
	}
	if typ != "ask" && typ != "notify" && typ != "broadcast" {
		return false, unsafeStrikes
	}
	safe := paneSafe(paneID, expectedCommand)
	if safe == nil || !*safe {
		status, detail := "rejected", "Pane "+paneID+" not safe for injection"
		if safe == nil {
			status, detail = "failed", "Pane "+paneID+" safety inconclusive; delivery not injected"
		}
		sendDeliveryAck(ctx, conn, data, status, detail)
		if typ == "ask" {
			sendFrameError(ctx, conn, stringValue(data, "correlation_id"), detail)
		}
		return safe != nil && !*safe, unsafeStrikes
	}
	from, to, text := firstNonempty(stringValue(data, "from_peer"), "unknown"), stringValue(data, "to_peer"), stringValue(data, "text")+formatAttachments(data["attachments"])
	injected := formatInboundMessage(from, to, typ, stringValue(data, "correlation_id"), text)
	if injectText(paneID, injected) {
		sendDeliveryAck(ctx, conn, data, "injected", "")
	} else {
		detail := "Failed to send keys to pane " + paneID
		sendDeliveryAck(ctx, conn, data, "failed", detail)
	}
	return false, unsafeStrikes
}

func formatInboundMessage(from, to, typ, correlationID, text string) string {
	from = strings.TrimPrefix(from, "@")
	to = strings.TrimPrefix(to, "@")
	toLabel := ""
	if to != "" {
		toLabel = " → @" + to
	}
	if isHumanSender(from) {
		return "@" + from + toLabel + ": " + text
	}
	attrs := ` from="@` + html.EscapeString(from) + `"`
	if to != "" {
		attrs += ` to="@` + html.EscapeString(to) + `"`
	}
	attrs += ` type="` + html.EscapeString(typ) + `"`
	if correlationID != "" {
		attrs += ` correlation-id="` + html.EscapeString(correlationID) + `"`
	}
	return "<peer-message" + attrs + ">\n" + html.EscapeString(text) + "\n</peer-message>"
}

func isHumanSender(from string) bool {
	switch strings.TrimPrefix(strings.ToLower(from), "@") {
	case "dashboard", "telegram", "slack", "human":
		return true
	default:
		return false
	}
}

func sendDeliveryAck(ctx context.Context, conn *websocket.Conn, data map[string]any, status, detail string) {
	typ, deliveryID := stringValue(data, "type"), stringValue(data, "delivery_id")
	if (typ != "ask" && typ != "notify") || deliveryID == "" {
		return
	}
	frame := map[string]any{"type": "delivery_ack", "delivery_id": deliveryID, "message_type": typ, "status": status}
	if detail != "" {
		frame["detail"] = detail
	}
	_ = wsjson.Write(ctx, conn, frame)
}

func sendFrameError(ctx context.Context, conn *websocket.Conn, cid, detail string) {
	_ = wsjson.Write(ctx, conn, map[string]any{"type": "error", "correlation_id": cid, "error": detail})
}

func formatAttachments(value any) string {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	lines := []string{"", "Attachments:"}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		id, path := stringValue(item, "id"), stringValue(item, "path")
		label := firstNonempty(stringValue(item, "filename"), path, id, "attachment")
		target := firstNonempty(path, func() string {
			if id != "" {
				return "/attachments/" + url.PathEscape(id)
			}
			return ""
		}())
		if target != "" {
			lines = append(lines, "- "+label+": "+target)
		} else {
			lines = append(lines, "- "+label)
		}
	}
	if len(lines) == 2 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func backoff(failures int) time.Duration {
	seconds := 1
	for i := 1; i < failures && seconds < 30; i++ {
		seconds *= 2
	}
	if seconds > 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}
