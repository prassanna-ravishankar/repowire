package service

// restart_launch.go reconstructs a runtime launch line for peer restart from
// what the daemon owns or can observe (backend, cwd, permission mode, resume
// id), never from a recorded argv. Observed argv is parsed for the permission
// flags only; everything else comes from the backend's spawn template.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// DefaultSpawnCommands are the built-in launch templates restart falls back to
// when daemon.spawn.commands has no entry for the backend.
var DefaultSpawnCommands = map[proto.AgentType]string{
	proto.AgentClaudeCode: "claude --dangerously-skip-permissions",
	proto.AgentCodex:      "codex --dangerously-bypass-approvals-and-sandbox",
	proto.AgentOpenCode:   "opencode",
	proto.AgentPi:         "pi",
}

// PreservedVerbatim labels a permission flag set restart does not fully
// understand; it is carried over word for word rather than mapped or defaulted.
const PreservedVerbatim = "preserved-verbatim"

// PermissionObservation is what a command line says about permissions: the
// permission-family words it carries (in order) and the mode they mean.
type PermissionObservation struct {
	Mode  string
	Words []string
}

var claudeNamedModes = map[string]bool{"acceptEdits": true, "plan": true, "default": true, "dontAsk": true, "bypassPermissions": true}

var permissionKeywords = []string{"permission", "dangerously", "sandbox", "approval", "yolo", "full-auto", "allowedtools", "disallowedtools", "allowed-tools", "disallowed-tools"}

// permissionWord reports whether value (a flag) belongs to the permission
// family for backend, and whether a following value belongs to it too.
func permissionWord(backend proto.AgentType, value, next string) (family, takesNext bool) {
	if !isFlag(value) {
		return false, false
	}
	name := flagName(value)
	lower := strings.ToLower(name)
	takes := !strings.Contains(value, "=") && next != "" && !isFlag(next) && !booleanFlags[name]
	if backend == proto.AgentCodex {
		switch name {
		case "-s", "-a":
			return true, takes
		case "-c", "--config":
			v := strings.ToLower(value + " " + next)
			for _, k := range []string{"sandbox", "approval", "permission"} {
				if strings.Contains(v, k) {
					return true, takes
				}
			}
			return false, false
		}
	}
	for _, k := range permissionKeywords {
		if strings.Contains(lower, k) {
			return true, takes
		}
	}
	return false, false
}

// ObservePermission finds the permission-family words in argv (argv[0] is
// the program) and names their mode: a canonical mode when every word is
// understood and they agree, "default" when there are none, else
// PreservedVerbatim.
func ObservePermission(backend proto.AgentType, argv []string) PermissionObservation {
	var words []string
	for i := 1; i < len(argv); i++ {
		next := ""
		if i+1 < len(argv) {
			next = argv[i+1]
		}
		family, takes := permissionWord(backend, argv[i], next)
		if !family {
			continue
		}
		words = append(words, argv[i])
		if takes {
			words = append(words, next)
			i++
		}
	}
	return PermissionObservation{Mode: permissionMode(backend, words), Words: words}
}

func permissionMode(backend proto.AgentType, words []string) string {
	if len(words) == 0 {
		return "default"
	}
	mode := ""
	agree := func(m string) bool {
		if mode != "" && mode != m {
			return false
		}
		mode = m
		return true
	}
	for i := 0; i < len(words); i++ {
		w, ok := words[i], false
		switch {
		case backend == proto.AgentClaudeCode && w == "--dangerously-skip-permissions":
			ok = agree("bypassPermissions")
		case backend == proto.AgentClaudeCode && strings.HasPrefix(w, "--permission-mode="):
			m := strings.TrimPrefix(w, "--permission-mode=")
			ok = claudeNamedModes[m] && agree(m)
		case backend == proto.AgentClaudeCode && w == "--permission-mode" && i+1 < len(words):
			i++
			ok = claudeNamedModes[words[i]] && agree(words[i])
		case backend == proto.AgentCodex && (w == "--dangerously-bypass-approvals-and-sandbox" || w == "--yolo"):
			ok = agree("bypass")
		case backend == proto.AgentCodex && w == "--full-auto":
			ok = agree("full-auto")
		}
		if !ok {
			return PreservedVerbatim
		}
	}
	return mode
}

// ObservePermissionInCommand is ObservePermission over a shell command line.
func ObservePermissionInCommand(backend proto.AgentType, command string) (PermissionObservation, error) {
	words, ok := splitShellWords(command)
	if !ok {
		return PermissionObservation{}, fmt.Errorf("launch command has unmatched quotes")
	}
	values := []string{}
	for _, w := range words[programIndex(words):] {
		values = append(values, w.Value)
	}
	return ObservePermission(backend, values), nil
}

// PermissionWords are the canonical launch words for a named mode.
func PermissionWords(backend proto.AgentType, mode string) ([]string, bool) {
	switch {
	case backend == proto.AgentClaudeCode && mode == "bypassPermissions":
		return []string{"--dangerously-skip-permissions"}, true
	case backend == proto.AgentClaudeCode && claudeNamedModes[mode]:
		return []string{"--permission-mode", mode}, true
	case backend == proto.AgentCodex && mode == "bypass":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}, true
	case backend == proto.AgentCodex && mode == "full-auto":
		return []string{"--full-auto"}, true
	}
	return nil, false
}

// ApplyPermission removes every permission-family word from command and
// appends words, so the launch carries exactly the observed permissions and
// never more. Other words keep their original quoting.
func ApplyPermission(command string, backend proto.AgentType, words []string) (string, error) {
	parsed, ok := splitShellWords(command)
	if !ok {
		return "", fmt.Errorf("launch command has unmatched quotes")
	}
	prog := programIndex(parsed)
	kept := append([]shellWord{}, parsed[:min(prog+1, len(parsed))]...)
	for i := prog + 1; i < len(parsed); i++ {
		next := ""
		if i+1 < len(parsed) {
			next = parsed[i+1].Value
		}
		family, takes := permissionWord(backend, parsed[i].Value, next)
		if !family {
			kept = append(kept, parsed[i])
			continue
		}
		if takes {
			i++
		}
	}
	return joinShellWords(append(kept, wordsFromValues(words)...)), nil
}

// ProcessControl is the OS seam restart uses to observe and stop a runtime.
// The real implementation shells to ps and signals; tests inject a fake.
type ProcessControl interface {
	// Args returns the argv of a live pid, nil when unknown.
	Args(pid int) []string
	Alive(pid int) bool
	// InTree reports whether pid is root or one of its descendants.
	InTree(pid, root int) bool
	// Terminate sends SIGTERM; restart never escalates to SIGKILL.
	Terminate(pid int) error
}

type realProcessControl struct{}

// NewProcessControl returns the production ProcessControl.
func NewProcessControl() ProcessControl { return realProcessControl{} }

// processQueryTimeout bounds every ps call on the restart path.
const processQueryTimeout = 2 * time.Second

func psOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), processQueryTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// Args reads /proc/<pid>/cmdline where it exists (exact argv); elsewhere ps
// joins argv with spaces, so words are split on whitespace.
func (realProcessControl) Args(pid int) []string {
	if pid <= 0 {
		return nil
	}
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	}
	out, err := psOutput("-o", "args=", "-p", strconv.Itoa(pid))
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

func (realProcessControl) Alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (realProcessControl) InTree(pid, root int) bool {
	for i := 0; pid > 1 && i < 64; i++ {
		if pid == root {
			return true
		}
		out, err := psOutput("-o", "ppid=", "-p", strconv.Itoa(pid))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(out)
		if err != nil {
			return false
		}
	}
	return false
}

func (realProcessControl) Terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
