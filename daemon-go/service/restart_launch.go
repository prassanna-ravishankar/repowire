package service

// restart_launch.go reconstructs a runtime launch line for peer restart from
// what the daemon owns or can observe (backend, cwd, permission mode, resume
// id), never from a recorded argv. Observed argv is parsed for the permission
// flags only; everything else comes from the backend's spawn template.

import (
	"os/exec"
	"regexp"
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

// permissionFlag maps one backend permission mode to its launch flags, and the
// pattern that finds those flags in a command line.
type permissionFlag struct {
	mode    string
	flags   []string
	pattern *regexp.Regexp
}

var claudeModeArg = regexp.MustCompile(`(^|\s)--permission-mode(?:=|\s+)(\S+)`)

var permissionFlags = map[proto.AgentType][]permissionFlag{
	proto.AgentClaudeCode: {
		{mode: "bypassPermissions", flags: []string{"--dangerously-skip-permissions"}, pattern: regexp.MustCompile(`(^|\s)--dangerously-skip-permissions(\s|$)`)},
	},
	proto.AgentCodex: {
		{mode: "bypass", flags: []string{"--dangerously-bypass-approvals-and-sandbox"}, pattern: regexp.MustCompile(`(^|\s)(--dangerously-bypass-approvals-and-sandbox|--yolo)(\s|$)`)},
		{mode: "full-auto", flags: []string{"--full-auto"}, pattern: regexp.MustCompile(`(^|\s)--full-auto(\s|$)`)},
	},
}

var claudeNamedModes = map[string]bool{"acceptEdits": true, "plan": true, "default": true, "dontAsk": true, "bypassPermissions": true}

// PermissionModeFromArgs returns the permission mode a command line selects,
// or "" when it names none.
func PermissionModeFromArgs(backend proto.AgentType, args string) string {
	if backend == proto.AgentClaudeCode {
		if m := claudeModeArg.FindStringSubmatch(args); m != nil && claudeNamedModes[m[2]] {
			return m[2]
		}
	}
	for _, f := range permissionFlags[backend] {
		if f.pattern.MatchString(args) {
			return f.mode
		}
	}
	return ""
}

// ValidPermissionMode reports whether mode has a launch flag for backend.
func ValidPermissionMode(backend proto.AgentType, mode string) bool {
	return modeFlags(backend, mode) != nil
}

func modeFlags(backend proto.AgentType, mode string) []string {
	if backend == proto.AgentClaudeCode && claudeNamedModes[mode] && mode != "bypassPermissions" {
		return []string{"--permission-mode", mode}
	}
	if backend == proto.AgentCodex && mode == "default" {
		return []string{}
	}
	for _, f := range permissionFlags[backend] {
		if f.mode == mode {
			return f.flags
		}
	}
	return nil
}

// WithPermissionMode replaces whatever permission flags base carries with the
// flags for mode. An unknown mode leaves base unchanged.
func WithPermissionMode(base string, backend proto.AgentType, mode string) string {
	flags := modeFlags(backend, mode)
	if flags == nil {
		return base
	}
	out := " " + strings.TrimSpace(base) + " "
	if backend == proto.AgentClaudeCode {
		out = claudeModeArg.ReplaceAllString(out, " ")
	}
	for _, f := range permissionFlags[backend] {
		for f.pattern.MatchString(out) {
			out = f.pattern.ReplaceAllString(out, " ")
		}
	}
	return strings.Join(append(strings.Fields(out), flags...), " ")
}

// ProcessControl is the OS seam restart uses to observe and stop a runtime.
// The real implementation shells to ps and signals; tests inject a fake.
type ProcessControl interface {
	// Args returns the command line of a live pid, "" when unknown.
	Args(pid int) string
	Alive(pid int) bool
	// InTree reports whether pid is root or one of its descendants.
	InTree(pid, root int) bool
	// Terminate sends SIGTERM; restart never escalates to SIGKILL.
	Terminate(pid int) error
}

type realProcessControl struct{}

// NewProcessControl returns the production ProcessControl.
func NewProcessControl() ProcessControl { return realProcessControl{} }

func (realProcessControl) Args(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (realProcessControl) Alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (realProcessControl) InTree(pid, root int) bool {
	for i := 0; pid > 1 && i < 64; i++ {
		if pid == root {
			return true
		}
		out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return false
		}
	}
	return false
}

func (realProcessControl) Terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
