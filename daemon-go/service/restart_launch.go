package service

// restart_launch.go reconstructs a runtime launch line for peer restart from
// what the daemon owns or can observe (backend, cwd, permissions, resume id),
// never from a recorded argv. Observed argv is read for its permission
// arguments; only argument shapes listed below (taken from `claude --help`
// and `codex --help`, codex-cli 0.162) are modeled. Anything else in the
// permission family makes restart relaunch the observed argv verbatim.

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

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

const (
	// ModeDefault: no permission arguments; the runtime's own config decides.
	ModeDefault = "default"
	// ModeArgvVerbatim: the observed permission arguments are not fully
	// modeled, so the observed argv is relaunched as-is.
	ModeArgvVerbatim = "argv-verbatim"
)

// ErrUnmodeledPermissions reports permission arguments restart cannot model.
var ErrUnmodeledPermissions = errors.New("permission arguments restart does not model")

var (
	claudePermissionModes = set("acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan")
	codexSandboxModes     = set("read-only", "workspace-write", "danger-full-access")
	codexApprovalPolicies = set("on-request", "never")
)

func set(values ...string) map[string]bool {
	out := map[string]bool{}
	for _, v := range values {
		out[v] = true
	}
	return out
}

// permissionArity is how many values each modeled permission flag takes in a
// launch template (-1: every following non-flag word). A permission-family
// flag missing from this table is unmodeled.
var permissionArity = map[proto.AgentType]map[string]int{
	proto.AgentClaudeCode: {
		"--dangerously-skip-permissions": 0, "--allow-dangerously-skip-permissions": 0,
		"--permission-mode": 1, "--permission-prompts": 1, "--permission-prompt-tool": 1,
		"--allowedTools": -1, "--allowed-tools": -1, "--disallowedTools": -1, "--disallowed-tools": -1, "--tools": -1,
	},
	proto.AgentCodex: {
		"--dangerously-bypass-approvals-and-sandbox": 0, "--dangerously-bypass-hook-trust": 0, "--approve-for-me": 0,
		"-s": 1, "--sandbox": 1, "-a": 1, "--ask-for-approval": 1, "-c": 1, "--config": 1,
	},
}

var permissionKeywords = []string{"permission", "dangerously", "sandbox", "approval", "approve", "yolo", "full-auto", "tools"}

// isPermissionFlag reports whether word belongs to the permission family.
// next is the following word, used for codex `-c key=value` overrides.
func isPermissionFlag(backend proto.AgentType, word, next string) bool {
	if !isFlag(word) {
		return false
	}
	name := flagName(word)
	if backend == proto.AgentCodex {
		switch name {
		case "-s", "-a":
			return true
		case "-c", "--config":
			value := strings.ToLower(word + " " + next)
			return strings.Contains(value, "sandbox") || strings.Contains(value, "approval") || strings.Contains(value, "permission")
		}
	}
	lower := strings.ToLower(name)
	for _, k := range permissionKeywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// PermissionObservation is what an argv says about permissions: a modeled
// mode with the exact words that select it, ModeDefault with no words, or
// ModeArgvVerbatim when any permission argument is outside the model.
type PermissionObservation struct {
	Mode  string
	Words []string
}

// ObservePermission reads argv (argv[0] is the program).
func ObservePermission(backend proto.AgentType, argv []string) PermissionObservation {
	var words, parts []string
	verbatim := PermissionObservation{Mode: ModeArgvVerbatim}
	claudeMode := ""
	for i := 1; i < len(argv); i++ {
		next := ""
		if i+1 < len(argv) {
			next = argv[i+1]
		}
		if !isPermissionFlag(backend, argv[i], next) {
			continue
		}
		name, inline, hasInline := strings.Cut(argv[i], "=")
		value, consumed := inline, 0
		if !hasInline {
			value, consumed = next, 1
		}
		switch {
		case backend == proto.AgentClaudeCode && argv[i] == "--dangerously-skip-permissions":
			if claudeMode != "" {
				return verbatim
			}
			claudeMode, words = "bypassPermissions", append(words, argv[i])
		case backend == proto.AgentClaudeCode && name == "--permission-mode" && claudePermissionModes[value]:
			if claudeMode != "" {
				return verbatim
			}
			claudeMode, words = value, append(words, argv[i:i+1+consumed]...)
			i += consumed
		case backend == proto.AgentCodex && argv[i] == "--dangerously-bypass-approvals-and-sandbox":
			parts, words = append(parts, "bypass"), append(words, argv[i])
		case backend == proto.AgentCodex && (name == "-s" || name == "--sandbox") && codexSandboxModes[value]:
			parts, words = append(parts, "sandbox="+value), append(words, argv[i:i+1+consumed]...)
			i += consumed
		case backend == proto.AgentCodex && (name == "-a" || name == "--ask-for-approval") && codexApprovalPolicies[value]:
			parts, words = append(parts, "approval="+value), append(words, argv[i:i+1+consumed]...)
			i += consumed
		default:
			return verbatim
		}
	}
	switch {
	case len(words) == 0:
		return PermissionObservation{Mode: ModeDefault}
	case claudeMode != "":
		return PermissionObservation{Mode: claudeMode, Words: words}
	case len(parts) > 1 && slices.Contains(parts, "bypass"):
		return verbatim // bypass plus an explicit sandbox/approval is not modeled
	}
	return PermissionObservation{Mode: strings.Join(parts, ","), Words: words}
}

// PermissionWords are the launch words for a mode a hook recorded.
func PermissionWords(backend proto.AgentType, mode string) ([]string, bool) {
	switch {
	case mode == ModeDefault && (backend == proto.AgentClaudeCode || backend == proto.AgentCodex):
		return []string{}, true
	case backend == proto.AgentClaudeCode && mode == "bypassPermissions":
		return []string{"--dangerously-skip-permissions"}, true
	case backend == proto.AgentClaudeCode && claudePermissionModes[mode]:
		return []string{"--permission-mode", mode}, true
	}
	return nil, false
}

// ApplyPermission removes the permission arguments from a launch template and
// appends words. It returns ErrUnmodeledPermissions when the template carries
// a permission argument whose arity is not modeled, since stripping it could
// leave part of it behind. Other words keep their original quoting.
func ApplyPermission(command string, backend proto.AgentType, words []string) (string, error) {
	parsed, ok := splitShellWords(command)
	if !ok {
		return "", errors.New("launch command has unmatched quotes")
	}
	prog := programIndex(parsed)
	kept := append([]shellWord{}, parsed[:min(prog+1, len(parsed))]...)
	for i := prog + 1; i < len(parsed); i++ {
		next := ""
		if i+1 < len(parsed) {
			next = parsed[i+1].Value
		}
		if !isPermissionFlag(backend, parsed[i].Value, next) {
			kept = append(kept, parsed[i])
			continue
		}
		arity, modeled := permissionArity[backend][flagName(parsed[i].Value)]
		if !modeled {
			return "", ErrUnmodeledPermissions
		}
		if strings.Contains(parsed[i].Value, "=") {
			continue
		}
		for n := 0; (arity < 0 || n < arity) && i+1 < len(parsed) && !isFlag(parsed[i+1].Value); n++ {
			i++
		}
	}
	return joinShellWords(append(kept, wordsFromValues(words)...)), nil
}

// ArgvCommand renders an observed argv as a shell command line.
func ArgvCommand(argv []string) string {
	return joinShellWords(wordsFromValues(argv))
}

// ProcessControl is the OS seam restart uses to observe and stop a runtime.
// The real implementation reads /proc or shells to ps and signals; tests
// inject a fake.
type ProcessControl interface {
	// Args returns the argv of a live pid (nil when unknown) and whether it is
	// exact: ps joins argv with spaces, so words containing spaces split.
	Args(pid int) (argv []string, exact bool)
	Alive(pid int) bool
	// InTree reports whether pid is root or one of its descendants.
	InTree(pid, root int) bool
	// Terminate sends SIGTERM; restart never escalates to SIGKILL.
	Terminate(pid int) error
}

type realProcessControl struct{}

// NewProcessControl returns the production ProcessControl.
func NewProcessControl() ProcessControl { return realProcessControl{} }

func (realProcessControl) Args(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), true
	}
	out, err := PSOutput("-o", "args=", "-p", strconv.Itoa(pid))
	if err != nil || out == "" {
		return nil, false
	}
	return strings.Fields(out), false
}

func (realProcessControl) Alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (realProcessControl) InTree(pid, root int) bool {
	for i := 0; pid > 1 && i < 64; i++ {
		if pid == root {
			return true
		}
		out, err := PSOutput("-o", "ppid=", "-p", strconv.Itoa(pid))
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
