package service

import (
	"fmt"
	"slices"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
)

type backendResumeSpec struct {
	supported  bool
	strategy   string
	flag       string
	subcommand string
	// stripValue/stripBool are the session-selecting args a template may
	// already carry; they are removed before the resume argument is appended.
	stripValue []string
	stripBool  []string
}

var backendResumeSpecs = map[proto.AgentType]backendResumeSpec{
	proto.AgentClaudeCode: {supported: true, strategy: "claude_resume", flag: "--resume",
		stripValue: []string{"--resume", "-r", "--session-id"}, stripBool: []string{"--continue", "-c", "--fork-session"}},
	proto.AgentCodex: {supported: true, strategy: "codex_resume", subcommand: "resume"},
	proto.AgentOpenCode: {supported: true, strategy: "opencode_session", flag: "--session",
		stripValue: []string{"--session", "-s"}, stripBool: []string{"--continue", "-c"}},
	proto.AgentPi: {supported: true, strategy: "pi_session", flag: "--session",
		stripValue: []string{"--session"}, stripBool: []string{"--continue", "-c", "--resume", "-r"}},
	proto.AgentMCPHTTP: {supported: false, strategy: "unsupported"},
}

// ResumeCapabilityForRegistration mirrors agent_backends.resume_capability_for_registration.
func ResumeCapabilityForRegistration(backend proto.AgentType, runtimeSessionID string) map[string]any {
	if runtimeSessionID == "" {
		return map[string]any{}
	}
	spec := backendResumeSpecs[backend]
	if !spec.supported {
		return map[string]any{
			"supported": false,
			"strategy":  spec.strategy,
			"reason":    "backend_resume_not_implemented",
		}
	}
	return map[string]any{
		"supported":              true,
		"strategy":               spec.strategy,
		"runtime_session_id_arg": runtimeSessionID,
	}
}

func canResumeBackend(backend proto.AgentType, runtimeSessionID string) bool {
	spec := backendResumeSpecs[backend]
	return spec.supported && runtimeSessionID != ""
}

// BuildResumeCommand replaces any session-selecting args in a base launch
// command with the backend-native resume argument for runtimeSessionID.
func BuildResumeCommand(command string, backend proto.AgentType, runtimeSessionID string) (string, error) {
	spec := backendResumeSpecs[backend]
	if !spec.supported {
		return "", fmt.Errorf("backend-native resume is not available for %s", backend)
	}
	if runtimeSessionID == "" {
		return "", fmt.Errorf("runtime_session_id is required for backend resume")
	}
	base, err := StripResume(command, backend)
	if err != nil {
		return "", err
	}
	arg := ShellQuote(runtimeSessionID)
	if spec.subcommand != "" {
		return base + " " + spec.subcommand + " " + arg, nil
	}
	return base + " " + spec.flag + " " + arg, nil
}

// codexValueFlags are the codex options that take a value (`codex --help`,
// `codex resume --help`, codex-cli 0.162), so a value is never mistaken for
// the subcommand or the session id.
var codexValueFlags = set("-c", "--config", "--enable", "--disable", "--remote", "--remote-auth-token-env",
	"-i", "--image", "-m", "--model", "--local-provider", "-p", "--profile", "-s", "--sandbox",
	"-C", "--cd", "--add-dir", "-a", "--ask-for-approval")

// codexResumeSelectors pick a session inside `codex resume [OPTIONS] [SESSION_ID|--last] [PROMPT]`.
var codexResumeSelectors = set("--last", "--all", "--include-non-interactive")

// StripResume removes the args that pick a session (resume/continue flags,
// codex's `resume` subcommand with its selector, id and prompt) so the result
// starts a fresh session. Options given to `codex resume` are kept; codex
// accepts them before the subcommand too.
func StripResume(command string, backend proto.AgentType) (string, error) {
	words, ok := splitShellWords(command)
	if !ok {
		return "", fmt.Errorf("launch command has unmatched quotes")
	}
	if backend == proto.AgentCodex {
		return joinShellWords(stripCodexResume(words)), nil
	}
	spec := backendResumeSpecs[backend]
	prog := programIndex(words)
	kept := append([]shellWord{}, words[:min(prog+1, len(words))]...)
	for i := prog + 1; i < len(words); i++ {
		w := words[i]
		name := flagName(w.Value)
		switch {
		case slices.Contains(spec.stripValue, name):
			if !strings.Contains(w.Value, "=") && i+1 < len(words) && !isFlag(words[i+1].Value) {
				i++
			}
		case slices.Contains(spec.stripBool, w.Value):
		default:
			kept = append(kept, w)
		}
	}
	return joinShellWords(kept), nil
}

func stripCodexResume(words []shellWord) []shellWord {
	prog := programIndex(words)
	kept := append([]shellWord{}, words[:min(prog+1, len(words))]...)
	inResume, positionals := false, 0
	for i := prog + 1; i < len(words); i++ {
		w := words[i]
		if isFlag(w.Value) {
			if inResume && codexResumeSelectors[w.Value] {
				continue
			}
			kept = append(kept, w)
			if codexValueFlags[flagName(w.Value)] && !strings.Contains(w.Value, "=") && i+1 < len(words) {
				i++
				kept = append(kept, words[i])
			}
			continue
		}
		positionals++
		switch {
		case positionals == 1 && w.Value == "resume":
			inResume = true
		case inResume: // SESSION_ID, then PROMPT
		default:
			kept = append(kept, w)
		}
	}
	return kept
}
