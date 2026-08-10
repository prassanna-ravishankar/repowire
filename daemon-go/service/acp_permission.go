package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/repowire/repowire/daemon-go/proto"
)

type ACPPermissionHandler func(peerID proto.PeerID, params json.RawMessage) map[string]any

// NewACPPermissionHandler translates ACP permission requests onto the shared
// blocking-question ask primitive. Only an explicit selected option allows.
func NewACPPermissionHandler(asks *AskTracker, emit func(string, map[string]any)) ACPPermissionHandler {
	return func(peerID proto.PeerID, raw json.RawMessage) map[string]any {
		cancelled := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		if asks == nil {
			return cancelled
		}
		var params struct {
			SessionID string           `json:"sessionId"`
			ToolCall  map[string]any   `json:"toolCall"`
			Options   []map[string]any `json:"options"`
		}
		if json.Unmarshal(raw, &params) != nil {
			return cancelled
		}

		options := make([]any, 0, len(params.Options))
		auditOptions := make([]map[string]any, 0, len(params.Options))
		for _, option := range params.Options {
			id := fmt.Sprint(firstPermissionValue(option, "optionId", "option_id", "id"))
			if id == "" {
				continue
			}
			title := fmt.Sprint(firstPermissionValue(option, "name", "title"))
			if title == "" {
				title = id
			}
			options = append(options, map[string]any{"id": id, "title": title})
			auditOptions = append(auditOptions, map[string]any{"option_id": id, "name": title})
		}

		cid := "acpperm-" + uuid.NewString()[:12]
		name := fmt.Sprint(firstPermissionValue(params.ToolCall, "title", "name", "kind"))
		if name == "" {
			name = "a tool"
		}
		prompt := "Allow " + name + "?"
		question := map[string]any{
			"kind": "choice", "prompt": prompt, "options": options,
			"blocking": true, "timeout_seconds": 60.0, "scope": "tool_permission",
			"metadata": map[string]any{
				"acp_peer_id": peerID, "acp_session_id": params.SessionID,
				"tool_call": params.ToolCall, "request_id": cid,
			},
			"default_answer": map[string]any{"outcome": "timed_out", "message": "permission request timed out"},
		}
		_, err := asks.Register(context.Background(), RegisterAskParams{
			FromPeerID: peerID, FromPeerName: proto.DisplayName(peerID),
			ToPeerID: "__repowire_control__", ToPeerName: "__repowire_control__",
			Text: prompt, CorrelationID: cid, Question: question, ReplyDelivery: "pull",
		})
		if err != nil {
			return cancelled
		}
		if emit != nil {
			emit("ask", map[string]any{
				"from": peerID, "to": "__repowire_control__", "from_peer_id": peerID,
				"to_peer_id": "__repowire_control__", "correlation_id": cid,
				"text": prompt, "question": question,
			})
			emit("acp_permission_request", map[string]any{
				"request_id": cid, "peer_id": peerID, "session_id": params.SessionID,
				"tool_call": params.ToolCall, "options": auditOptions,
				"status": "pending", "timeout_seconds": 60.0,
			})
		}

		message := "permission request timed out"
		answer, err := asks.WaitForAnswer(context.Background(), cid, 60*time.Second, &Answer{Outcome: "timed_out", Message: &message})
		if err == nil && answer.Outcome == "answered" && answer.OptionID != nil && *answer.OptionID != "" {
			if emit != nil {
				emit("acp_permission_decision", map[string]any{
					"request_id": cid, "peer_id": peerID, "session_id": params.SessionID,
					"outcome": "allowed", "option_id": *answer.OptionID, "status": "decided",
				})
			}
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": *answer.OptionID}}
		}
		if emit != nil {
			status := "decided"
			if answer.Outcome == "timed_out" {
				status = "timed_out"
			}
			emit("acp_permission_decision", map[string]any{
				"request_id": cid, "peer_id": peerID, "session_id": params.SessionID,
				"outcome": "denied", "status": status,
			})
		}
		return cancelled
	}
}

func firstPermissionValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value := values[key]; value != nil && fmt.Sprint(value) != "" {
			return value
		}
	}
	return ""
}
