package service

import (
	"fmt"
	"strings"

	"github.com/repowire/repowire/daemon-go/proto"
)

type backendResumeSpec struct {
	supported  bool
	strategy   string
	flag       string
	subcommand string
}

var backendResumeSpecs = map[proto.AgentType]backendResumeSpec{
	proto.AgentClaudeCode:  {supported: true, strategy: "claude_resume", flag: "--resume"},
	proto.AgentCodex:       {supported: true, strategy: "codex_resume", subcommand: "resume"},
	proto.AgentGemini:      {supported: true, strategy: "gemini_resume", flag: "--resume"},
	proto.AgentOpenCode:    {supported: true, strategy: "opencode_session", flag: "--session"},
	proto.AgentAntigravity: {supported: true, strategy: "antigravity_conversation", flag: "--conversation"},
	proto.AgentPi:          {supported: true, strategy: "pi_session", flag: "--session"},
	proto.AgentMCPHTTP:     {supported: false, strategy: "unsupported"},
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

// BuildResumeCommand appends the backend-native resume argument to a base launch command.
func BuildResumeCommand(command string, backend proto.AgentType, runtimeSessionID string) (string, error) {
	spec := backendResumeSpecs[backend]
	if !spec.supported {
		return "", fmt.Errorf("backend-native resume is not available for %s", backend)
	}
	if runtimeSessionID == "" {
		return "", fmt.Errorf("runtime_session_id is required for backend resume")
	}
	arg := shellQuote(runtimeSessionID)
	if spec.subcommand != "" {
		return strings.TrimSpace(command) + " " + spec.subcommand + " " + arg, nil
	}
	return strings.TrimSpace(command) + " " + spec.flag + " " + arg, nil
}
