package hooks

import (
	"bufio"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func streamerPIDPath(paneID string) string {
	return filepath.Join(paneLogsDir(), "chat-delta-streamer-"+paneToken(paneID)+".pid")
}

func streamerLockPath(paneID string) string {
	return filepath.Join(paneLogsDir(), "chat-delta-streamer-"+paneToken(paneID)+".lock")
}

func startChatStreamer(transcript, peer, paneID, sessionID string) {
	path := streamerPIDPath(paneID)
	if raw, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pidAlive(pid) {
			_ = os.Remove(path)
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	if _, err := os.Stat(transcript); err != nil {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		return
	}
	args := []string{"chat-stream", "--transcript", transcript, "--peer", peer, "--pane-id", paneID}
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}

func RunChatStream(args []string) int {
	flags := flag.NewFlagSet("chat-stream", flag.ContinueOnError)
	transcript := flags.String("transcript", "", "transcript JSONL")
	peer := flags.String("peer", "", "peer display name")
	paneID := flags.String("pane-id", "", "tmux pane id")
	sessionID := flags.String("session-id", "", "runtime session id")
	if flags.Parse(args) != nil || *transcript == "" || *peer == "" || *paneID == "" {
		return 2
	}
	lock, err := os.OpenFile(streamerLockPath(*paneID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 1
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return 0
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	pidPath := streamerPIDPath(*paneID)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 1
	}
	defer func() {
		if raw, err := os.ReadFile(pidPath); err == nil && strings.TrimSpace(string(raw)) == strconv.Itoa(os.Getpid()) {
			_ = os.Remove(pidPath)
		}
	}()
	return tailTranscript(*transcript, *peer, *paneID, *sessionID, pidPath)
}

func tailTranscript(path, peer, paneID, sessionID, pidPath string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil {
		_, _ = file.Seek(info.Size(), io.SeekStart)
	}
	reader := bufio.NewReaderSize(file, 64*1024)
	started, lastGrowth := time.Now(), time.Now()
	turnID, chunkIndex := "", 0
	for {
		raw, err := os.ReadFile(pidPath)
		if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) || time.Since(started) > 30*time.Minute || time.Since(lastGrowth) > 90*time.Second {
			return 0
		}
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err != nil {
			return 0
		}
		lastGrowth = time.Now()
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil || stringValue(entry, "type") != "assistant" {
			continue
		}
		message, _ := entry["message"].(map[string]any)
		blocks, _ := message["content"].([]any)
		if turnID == "" {
			turnID = firstNonempty(stringValue(entry, "uuid"), paneID+"-"+strconv.FormatInt(started.Unix(), 10))
		}
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			kind, text := stringValue(block, "type"), ""
			payload := map[string]any{
				"peer": peer, "role": "assistant", "turn_id": turnID,
				"chunk_index": chunkIndex, "kind": kind, "is_final": false, "pane_id": paneID,
			}
			switch kind {
			case "text":
				text = stringValue(block, "text")
				if strings.TrimSpace(text) == "" {
					continue
				}
			case "tool_use":
				name := firstNonempty(stringValue(block, "name"), "unknown")
				input, _ := block["input"].(map[string]any)
				summary := summarizeToolInput(input)
				text = name
				if summary != "" {
					text += ": " + summary
				}
				payload["tool_call"] = map[string]string{"name": name, "input": summary}
			default:
				continue
			}
			payload["text"] = text
			if sessionID != "" {
				payload["session_id"] = sessionID
			}
			if daemonPost("/events/chat_delta", payload) != nil {
				chunkIndex++
			}
		}
	}
}
