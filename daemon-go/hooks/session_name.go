package hooks

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// nativeFields is the /session/update body for a runtime's session name (it
// renames the peer) and its generated title (it seeds an empty description).
// Empty values are left out: a report without a name changes nothing.
func nativeFields(name, title string) map[string]any {
	fields := map[string]any{}
	if name != "" {
		fields["native_name"] = name
	}
	if title != "" {
		fields["native_title"] = title
	}
	return fields
}

// sessionNativeName is the name a session registers under: its own name when
// set, else, for Claude Code, the default name it shows in agent view.
func sessionNativeName(backend string, payload Payload) string {
	if payload.SessionTitle != "" || backend != "claude-code" {
		return payload.SessionTitle
	}
	return claudeDefaultName(payload.SessionID)
}

// claudeDefaultName looks up the default name Claude Code gives a session
// (folder plus a short suffix, as in `claude agents`). Hooks do not receive it.
// Any failure leaves the peer on its derived {folder}-{backend} name.
var claudeDefaultName = func(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "agents", "--json").Output()
	if err != nil {
		return ""
	}
	return agentName(out, sessionID)
}

func agentName(listing []byte, sessionID string) string {
	var agents []struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	if json.Unmarshal(listing, &agents) != nil {
		return ""
	}
	for _, agent := range agents {
		if agent.SessionID == sessionID {
			return agent.Name
		}
	}
	return ""
}
