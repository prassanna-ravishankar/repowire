package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

func addMCPTool[T any](srv *mcp.Server, name, description string, fn func(context.Context, string, T) (string, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description}, func(ctx context.Context, req *mcp.CallToolRequest, args T) (*mcp.CallToolResult, any, error) {
		text, err := fn(ctx, callerIdentity(req), args)
		if err != nil {
			return nil, nil, err
		}
		return textResult(text), nil, nil
	})
}

func jsonResult(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func requireMCPAdmin(h *Hub, cfg config.MCPHTTPConfig, caller, tool string) error {
	if caller != mcpDefaultIdentity {
		if peer, _ := h.reg.GetPeerByName(caller, nil); peer != nil {
			return nil
		}
	}
	if cfg.AllowDangerousTools {
		return nil
	}
	return fmt.Errorf("%s is disabled for anonymous HTTP MCP; enable daemon.mcp_http.allow_dangerous_tools or use a registered peer identity", tool)
}

// mcpKillCircle keeps destructive MCP control inside an ordinary caller's
// circle. Anonymous HTTP MCP remains the explicit CLI/admin surface, while
// bypass-capable roles may target another circle.
func (h *Hub) mcpKillCircle(caller, requested string) (*string, error) {
	if caller == mcpDefaultIdentity {
		return strPtr(requested), nil
	}
	peer, err := h.reg.GetPeerByName(caller, nil)
	if err != nil || peer == nil {
		return nil, fmt.Errorf("kill_peer caller is not registered")
	}
	if peer.Role.BypassesCircles() {
		return strPtr(requested), nil
	}
	if requested != "" && requested != peer.Circle {
		return nil, fmt.Errorf("kill_peer agents may only target their own circle (%s)", peer.Circle)
	}
	return &peer.Circle, nil
}

// mcpSpawnPlacement keeps agent spawns in the caller's circle and, when
// available, carries its pane so window-boundary placement stays server-derived.
// Direct /spawn callers (the CLI) intentionally bypass this MCP-specific policy.
func (h *Hub) mcpSpawnPlacement(caller, requested string) (circle, sourcePane string, err error) {
	if caller == mcpDefaultIdentity {
		if requested == "" {
			return "", "", fmt.Errorf("spawn_peer requires circle for anonymous HTTP MCP")
		}
		return requested, "", nil
	}
	peer, err := h.reg.GetPeerByName(caller, nil)
	if err != nil || peer == nil {
		return "", "", fmt.Errorf("spawn_peer caller is not registered")
	}
	if !peer.Role.BypassesCircles() && requested != "" && requested != peer.Circle {
		return "", "", fmt.Errorf("spawn_peer agents may only spawn in their own circle (%s)", peer.Circle)
	}
	circle = firstNonempty(requested, peer.Circle)
	if circle == peer.Circle {
		sourcePane = derefString(peer.PaneID)
	}
	return circle, sourcePane, nil
}

type mcpAskArgs struct {
	PeerName    string           `json:"peer_name"`
	Query       string           `json:"query"`
	ReplyTo     string           `json:"reply_to,omitempty"`
	Circle      string           `json:"circle,omitempty"`
	Attachments []map[string]any `json:"attachments,omitempty"`
}
type mcpWaitArgs struct {
	CorrelationID  string `json:"correlation_id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}
type mcpAskManyArgs struct {
	PeerNames      []string `json:"peer_names"`
	Query          string   `json:"query"`
	Circle         string   `json:"circle,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}
type mcpParentIDArgs struct {
	ParentID string `json:"parent_id"`
}
type mcpJobIDArgs struct {
	JobID string `json:"job_id"`
}
type mcpScheduleIDArgs struct {
	ScheduleID string `json:"schedule_id"`
}
type mcpShareIDArgs struct {
	ShareID string `json:"share_id"`
}
type mcpAckArgs struct {
	CorrelationID string           `json:"correlation_id"`
	Message       *string          `json:"message,omitempty"`
	Attachments   []map[string]any `json:"attachments,omitempty"`
}
type mcpAnswerArgs struct {
	CorrelationID string  `json:"correlation_id"`
	OptionID      *string `json:"option_id,omitempty"`
	Text          *string `json:"text,omitempty"`
}

type mcpJobCreateArgs struct {
	Title             string         `json:"title,omitempty"`
	Kind              string         `json:"kind,omitempty"`
	AssignedPeerID    string         `json:"assigned_peer_id,omitempty"`
	OwnerPeerID       string         `json:"owner_peer_id,omitempty"`
	RepowireSessionID string         `json:"repowire_session_id,omitempty"`
	CorrelationID     string         `json:"correlation_id,omitempty"`
	Circle            string         `json:"circle,omitempty"`
	SourceKind        string         `json:"source_kind,omitempty"`
	SourceID          string         `json:"source_id,omitempty"`
	Scope             string         `json:"scope,omitempty"`
	Visibility        string         `json:"visibility,omitempty"`
	DeadlineAt        string         `json:"deadline_at,omitempty"`
	ExpiresAt         string         `json:"expires_at,omitempty"`
	Prompt            string         `json:"prompt,omitempty"`
	PromptFile        string         `json:"prompt_file,omitempty"`
	Path              string         `json:"path,omitempty"`
	Backend           string         `json:"backend,omitempty"`
	Profile           string         `json:"profile,omitempty"`
	DueAt             string         `json:"due_at,omitempty"`
	Cron              string         `json:"cron,omitempty"`
	ResultSurface     string         `json:"result_surface,omitempty"`
	ProcessScope      string         `json:"process_scope,omitempty"`
	Continuity        string         `json:"continuity,omitempty"`
	Request           map[string]any `json:"request,omitempty"`
	Provenance        map[string]any `json:"provenance,omitempty"`
}
type mcpJobListArgs struct {
	State             string `json:"state,omitempty"`
	OwnerPeerID       string `json:"owner_peer_id,omitempty"`
	CreatedByPeerID   string `json:"created_by_peer_id,omitempty"`
	RepowireSessionID string `json:"repowire_session_id,omitempty"`
	Circle            string `json:"circle,omitempty"`
}
type mcpJobUpdateArgs struct {
	JobID         string         `json:"job_id"`
	State         string         `json:"state"`
	StateReason   string         `json:"state_reason,omitempty"`
	Phase         string         `json:"phase,omitempty"`
	ProgressNote  string         `json:"progress_note,omitempty"`
	ResultSummary string         `json:"result_summary,omitempty"`
	AttemptID     string         `json:"attempt_id,omitempty"`
	Progress      map[string]any `json:"progress,omitempty"`
	ResultData    map[string]any `json:"result_data,omitempty"`
	Error         map[string]any `json:"error,omitempty"`
	Provenance    map[string]any `json:"provenance,omitempty"`
	Artifacts     []any          `json:"artifacts,omitempty"`
}
type mcpJobCancelArgs struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason,omitempty"`
}

type mcpDescriptionArgs struct {
	Description string `json:"description"`
}
type mcpSpawnArgs struct {
	Path    string `json:"path"`
	Backend string `json:"backend,omitempty"`
	Profile string `json:"profile,omitempty"`
	Command string `json:"command,omitempty"`
	Circle  string `json:"circle,omitempty"`
	Message string `json:"message,omitempty"`
}
type mcpCircleArgs struct {
	Circle string `json:"circle,omitempty"`
}
type mcpKillArgs struct {
	PeerIdentifier string `json:"peer_identifier"`
	Circle         string `json:"circle,omitempty"`
}
type mcpMarkReviewArgs struct {
	PRURL           string  `json:"pr_url"`
	LastReviewedSHA *string `json:"last_reviewed_sha,omitempty"`
}
type mcpReviewArgs struct {
	PeerName string `json:"peer_name,omitempty"`
}
type mcpScheduleArgs struct {
	ToPeer string `json:"to_peer"`
	Text   string `json:"text"`
	FireAt string `json:"fire_at,omitempty"`
	Cron   string `json:"cron,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Circle string `json:"circle,omitempty"`
}
type mcpScheduleSelfArgs struct {
	Text   string `json:"text"`
	FireAt string `json:"fire_at,omitempty"`
	Cron   string `json:"cron,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Circle string `json:"circle,omitempty"`
}
type mcpScheduleListArgs struct {
	MineOnly    *bool `json:"mine_only,omitempty"`
	IncludeCron bool  `json:"include_cron,omitempty"`
}
type mcpShareArgs struct {
	PeerName    string `json:"peer_name,omitempty"`
	Permissions string `json:"permissions,omitempty"`
	TTLSeconds  *int   `json:"ttl_secs,omitempty"`
}

func registerMCPParityTools(srv *mcp.Server, h *Hub, cfg config.MCPHTTPConfig) {
	addMCPTool(srv, "ask", "Open a tracked thread only when another peer's context or ownership materially helps and explicit closure is needed; peers may be occupied.", func(ctx context.Context, caller string, a mcpAskArgs) (string, error) {
		if err := requireFields("peer_name", a.PeerName, "query", a.Query); err != nil {
			return "", err
		}
		result, err := h.openAsk(ctx, AskRequest{FromPeer: caller, ToPeer: a.PeerName, Text: a.Query, ReplyTo: strPtr(a.ReplyTo), Circle: h.mcpSendCircle(caller, a.Circle), Attachments: a.Attachments})
		return result.CorrelationID, err
	})
	addMCPTool(srv, "wait_on_ack", "Wait for a tracked ask to resolve.", func(ctx context.Context, caller string, a mcpWaitArgs) (string, error) {
		if err := required("correlation_id", a.CorrelationID); err != nil {
			return "", err
		}
		timeout := a.TimeoutSeconds
		if timeout <= 0 {
			timeout = 600
		}
		wait := float64(timeout)
		result, err := h.waitOnAck(ctx, a.CorrelationID, AskWaitRequest{PeerID: caller, TimeoutSeconds: &wait})
		return jsonResult(result), err
	})
	addMCPTool(srv, "ask_many", "Open tracked asks only when every named peer's input is materially needed; peers may be occupied.", func(ctx context.Context, caller string, a mcpAskManyArgs) (string, error) {
		if len(a.PeerNames) == 0 {
			return "", fmt.Errorf("peer_names is required")
		}
		if err := required("query", a.Query); err != nil {
			return "", err
		}
		result, err := h.openAskMany(ctx, AskManyRequest{FromPeer: caller, ToPeers: a.PeerNames, Text: a.Query, Circle: strPtr(a.Circle), TimeoutSeconds: defaultInt(a.TimeoutSeconds, 300)})
		if err != nil {
			return "", err
		}
		return result.ParentID, nil
	})
	addMCPTool(srv, "ask_many_result", "Return the current result of an ask-many fanout.", func(ctx context.Context, _ string, a mcpParentIDArgs) (string, error) {
		if err := required("parent_id", a.ParentID); err != nil {
			return "", err
		}
		result, err := h.askManyResult(a.ParentID)
		return jsonResult(result), err
	})
	addMCPTool(srv, "ack", "Close an ask, optionally replying to its asker.", func(ctx context.Context, caller string, a mcpAckArgs) (string, error) {
		if err := required("correlation_id", a.CorrelationID); err != nil {
			return "", err
		}
		_, err := h.ackDirect(ctx, AckRequest{CorrelationID: a.CorrelationID, FromPeer: &caller, Message: a.Message, Attachments: a.Attachments})
		if err != nil {
			return "", err
		}
		suffix := ""
		if a.Message != nil || len(a.Attachments) > 0 {
			suffix = " with reply"
		}
		return "acked #" + a.CorrelationID + suffix, nil
	})
	addMCPTool(srv, "answer", "Answer a structured question.", func(ctx context.Context, caller string, a mcpAnswerArgs) (string, error) {
		if err := required("correlation_id", a.CorrelationID); err != nil {
			return "", err
		}
		if a.OptionID == nil && a.Text == nil {
			return "", fmt.Errorf("option_id or text is required")
		}
		_, err := h.answerDirect(ctx, AnswerRequest{CorrelationID: a.CorrelationID, OptionID: a.OptionID, Text: a.Text})
		return "answered #" + a.CorrelationID, err
	})

	addMCPTool(srv, "job_create", "Create a durable tracked work job.", func(ctx context.Context, caller string, a mcpJobCreateArgs) (string, error) {
		if err := required("title", a.Title); err != nil {
			return "", err
		}
		result, err := h.jobCreate(ctx, workCreateRequest{
			Title: a.Title, Kind: firstNonempty(a.Kind, "general"), CreatedByPeerID: &caller,
			AssignedPeerID: strPtr(a.AssignedPeerID), OwnerPeerID: strPtr(a.OwnerPeerID), RepowireSessionID: strPtr(a.RepowireSessionID), CorrelationID: strPtr(a.CorrelationID), Circle: strPtr(a.Circle),
			SourceKind: strPtr(a.SourceKind), SourceID: strPtr(a.SourceID), Scope: strPtr(a.Scope), Visibility: firstNonempty(a.Visibility, "circle"), DeadlineAt: strPtr(a.DeadlineAt), ExpiresAt: strPtr(a.ExpiresAt),
			Prompt: strPtr(a.Prompt), PromptFile: strPtr(a.PromptFile), Path: strPtr(a.Path), Backend: strPtr(a.Backend), Profile: strPtr(a.Profile), DueAt: strPtr(a.DueAt), Cron: strPtr(a.Cron), ResultSurface: strPtr(a.ResultSurface), ProcessScope: strPtr(a.ProcessScope), Continuity: strPtr(a.Continuity),
			Request: firstNonNilMap(a.Request), Provenance: a.Provenance,
		})
		return jsonResult(result), err
	})
	addMCPTool(srv, "job_list", "List durable jobs as JSON.", func(ctx context.Context, _ string, a mcpJobListArgs) (string, error) {
		result, err := h.jobList(ctx, workListRequest{State: strPtr(a.State), OwnerPeerID: strPtr(a.OwnerPeerID), CreatedByPeerID: strPtr(a.CreatedByPeerID), RepowireSessionID: strPtr(a.RepowireSessionID), Circle: strPtr(a.Circle)})
		return jsonResult(result), err
	})
	jobStatus := func(ctx context.Context, _ string, a mcpJobIDArgs) (string, error) {
		if err := required("job_id", a.JobID); err != nil {
			return "", err
		}
		result, err := h.jobStatus(ctx, a.JobID)
		return jsonResult(result), err
	}
	addMCPTool(srv, "job_status", "Return one job status as JSON.", jobStatus)
	addMCPTool(srv, "job_show", "Alias for job_status.", jobStatus)
	addMCPTool(srv, "job_update", "Update a tracked job lifecycle state.", func(ctx context.Context, _ string, a mcpJobUpdateArgs) (string, error) {
		if err := requireFields("job_id", a.JobID, "state", a.State); err != nil {
			return "", err
		}
		result, err := h.jobUpdate(ctx, a.JobID, workUpdateRequest{State: a.State, StateReason: strPtr(a.StateReason), Phase: strPtr(a.Phase), ProgressNote: strPtr(a.ProgressNote), ResultSummary: strPtr(a.ResultSummary), AttemptID: strPtr(a.AttemptID), Progress: a.Progress, ResultData: a.ResultData, Error: a.Error, Provenance: a.Provenance, Artifacts: a.Artifacts})
		return jsonResult(result), err
	})
	addMCPTool(srv, "job_result", "Return a tracked job result as JSON.", func(ctx context.Context, _ string, a mcpJobIDArgs) (string, error) {
		if err := required("job_id", a.JobID); err != nil {
			return "", err
		}
		result, err := h.jobResult(ctx, a.JobID)
		return jsonResult(result), err
	})
	addMCPTool(srv, "job_cancel", "Request cancellation for a tracked job.", func(ctx context.Context, caller string, a mcpJobCancelArgs) (string, error) {
		if err := required("job_id", a.JobID); err != nil {
			return "", err
		}
		result, err := h.jobCancel(ctx, a.JobID, workCancelRequest{RequestedByPeerID: &caller, Reason: firstNonempty(a.Reason, "cancel_requested")})
		return jsonResult(result), err
	})

	addMCPTool(srv, "set_description", "Update the caller's dashboard task description.", func(ctx context.Context, caller string, a mcpDescriptionArgs) (string, error) {
		found, err := h.reg.UpdateDescription(ctx, caller, a.Description, nil)
		if err == nil && !found {
			err = fmt.Errorf("peer not found: %s", caller)
		}
		return "description updated: " + a.Description, err
	})
	addMCPTool(srv, "spawn_peer", "Spawn a local tmux-backed coding peer.", func(ctx context.Context, caller string, a mcpSpawnArgs) (string, error) {
		if err := requireMCPAdmin(h, cfg, caller, "spawn_peer"); err != nil {
			return "", err
		}
		if err := required("path", a.Path); err != nil {
			return "", err
		}
		circle, sourcePane, err := h.mcpSpawnPlacement(caller, a.Circle)
		if err != nil {
			return "", err
		}
		var backend *proto.AgentType
		if a.Backend != "" {
			value := proto.AgentType(a.Backend)
			backend = &value
		}
		result, err := h.spawnPeer(ctx, SpawnRequest{Path: a.Path, Backend: backend, Profile: strPtr(a.Profile), Command: strPtr(a.Command), Circle: circle, Message: strPtr(a.Message), SourcePane: sourcePane})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Spawned %s (tmux: %s) peer_id=%s registration_state=%s", result.DisplayName, result.TmuxSession, derefString(result.PeerID), result.RegistrationState), nil
	})
	addMCPTool(srv, "orchestrator_status", "Check whether a live orchestrator is present in a circle.", func(ctx context.Context, caller string, a mcpCircleArgs) (string, error) {
		circle := a.Circle
		if circle == "" {
			if peer, _ := h.reg.GetPeerByName(caller, nil); peer != nil {
				circle = peer.Circle
			} else {
				circle = "global"
			}
		}
		resp := OrchestratorStatusResponse{Circle: circle, StaleAfterSeconds: int(h.reg.HeartbeatTolerance().Seconds())}
		if orch, ok := h.reg.GetOrchestrator(circle); ok {
			peerID, peerName := string(orch.PeerID), string(orch.DisplayName)
			resp.Present, resp.PeerID, resp.PeerName, resp.LastSeen = true, &peerID, &peerName, isoOrNil(orch.LastSeen)
		}
		return jsonResult(resp), nil
	})
	addMCPTool(srv, "kill_peer", "Deregister a peer and kill its pane only with destructive proof.", func(ctx context.Context, caller string, a mcpKillArgs) (string, error) {
		if err := requireMCPAdmin(h, cfg, caller, "kill_peer"); err != nil {
			return "", err
		}
		if err := required("peer_identifier", a.PeerIdentifier); err != nil {
			return "", err
		}
		circle, err := h.mcpKillCircle(caller, a.Circle)
		if err != nil {
			return "", err
		}
		result, err := h.killPeer(ctx, KillPeerRequest{PeerIdentifier: a.PeerIdentifier, Circle: circle, FromPeer: &caller})
		return jsonResult(result), err
	})
	addMCPTool(srv, "mark_reviewed", "Record that the caller reviewed a pull request.", func(ctx context.Context, caller string, a mcpMarkReviewArgs) (string, error) {
		if err := required("pr_url", a.PRURL); err != nil {
			return "", err
		}
		err := h.markReviewedDirect(markReviewedRequest{Reviewer: caller, PRURL: a.PRURL, LastReviewedSHA: a.LastReviewedSHA})
		return "marked reviewed: " + a.PRURL, err
	})
	addMCPTool(srv, "review_queue", "List pull requests awaiting review.", func(ctx context.Context, caller string, a mcpReviewArgs) (string, error) {
		items, err := h.listReviewsDirect(firstNonempty(a.PeerName, caller))
		if err != nil {
			return "", err
		}
		lines := []string{"pr_url\tlast_reviewed_sha\tcurrent_head_sha\tstate\tmy_action"}
		for _, item := range items {
			lines = append(lines, strings.Join([]string{item.PRURL, derefString(item.LastReviewedSHA), derefString(item.CurrentHeadSHA), item.State, item.MyAction}, "\t"))
		}
		return strings.Join(lines, "\n"), nil
	})

	addMCPTool(srv, "schedule_create", "Schedule a one-shot future peer message.", scheduleTool(h, cfg, false))
	addMCPTool(srv, "schedule_cron", "Schedule a recurring peer message.", scheduleTool(h, cfg, true))
	addMCPTool(srv, "schedule_self", "Schedule a future message to the caller.", func(ctx context.Context, caller string, a mcpScheduleSelfArgs) (string, error) {
		if err := requireMCPAdmin(h, cfg, caller, "schedule_self"); err != nil {
			return "", err
		}
		if err := required("text", a.Text); err != nil {
			return "", err
		}
		if (a.FireAt == "") == (a.Cron == "") {
			return "", fmt.Errorf("provide exactly one of fire_at or cron")
		}
		result, err := h.createSchedule(ctx, scheduleCreateRequest{FromPeer: caller, ToPeer: caller, Text: a.Text, FireAt: strPtr(a.FireAt), Cron: strPtr(a.Cron), Kind: firstNonempty(a.Kind, "notify"), Circle: strPtr(a.Circle)})
		return result.ScheduleID, err
	})
	addMCPTool(srv, "schedule_list", "List pending scheduled messages.", func(ctx context.Context, caller string, a mcpScheduleListArgs) (string, error) {
		var fromPeer *string
		if a.MineOnly == nil || *a.MineOnly {
			fromPeer = &caller
		}
		result, err := h.listSchedules(ctx, fromPeer)
		if err != nil {
			return "", err
		}
		header := "schedule_id\tfrom_peer\tto_peer\tkind\tfire_at\ttext"
		if a.IncludeCron {
			header += "\tcron"
		}
		lines := []string{header}
		for _, item := range result.Schedules {
			fields := []string{item.ScheduleID, item.FromPeer, item.ToPeer, item.Kind, item.FireAt, strings.NewReplacer("\t", " ", "\n", " ").Replace(item.Text)}
			if a.IncludeCron {
				fields = append(fields, derefString(item.Cron))
			}
			lines = append(lines, strings.Join(fields, "\t"))
		}
		return strings.Join(lines, "\n"), nil
	})
	addMCPTool(srv, "schedule_delete", "Cancel a pending schedule.", func(ctx context.Context, caller string, a mcpScheduleIDArgs) (string, error) {
		if err := requireMCPAdmin(h, cfg, caller, "schedule_delete"); err != nil {
			return "", err
		}
		if err := required("schedule_id", a.ScheduleID); err != nil {
			return "", err
		}
		err := h.deleteSchedule(ctx, a.ScheduleID)
		return "deleted schedule " + a.ScheduleID, err
	})
	addMCPTool(srv, "share_session", "Generate a relay share link for a peer.", func(ctx context.Context, caller string, a mcpShareArgs) (string, error) {
		target := firstNonempty(a.PeerName, caller)
		result, err := h.createShareDirect(ctx, shareRequest{PeerName: target, Permissions: firstNonempty(a.Permissions, "ro"), TTLSecs: a.TTLSeconds})
		if err != nil {
			return "", err
		}
		expires := "never"
		if value, ok := result["expires_at"].(string); ok && value != "" {
			expires = value
		}
		return fmt.Sprintf("share link for %s [%s]: %s\nshare_id: %s\nexpires: %s", target, stringValue(result, "permissions"), stringValue(result, "url"), stringValue(result, "share_id"), expires), nil
	})
	addMCPTool(srv, "revoke_share", "Revoke a relay share link.", func(ctx context.Context, _ string, a mcpShareIDArgs) (string, error) {
		if err := required("share_id", a.ShareID); err != nil {
			return "", err
		}
		_, err := h.revokeShareDirect(ctx, a.ShareID)
		return "revoked share " + a.ShareID, err
	})
}

func scheduleTool(h *Hub, cfg config.MCPHTTPConfig, cron bool) func(context.Context, string, mcpScheduleArgs) (string, error) {
	return func(ctx context.Context, caller string, a mcpScheduleArgs) (string, error) {
		name := "schedule_create"
		if cron {
			name = "schedule_cron"
		}
		if err := requireMCPAdmin(h, cfg, caller, name); err != nil {
			return "", err
		}
		if err := requireFields("to_peer", a.ToPeer, "text", a.Text); err != nil {
			return "", err
		}
		if cron && a.Cron == "" {
			return "", fmt.Errorf("cron is required")
		}
		if !cron && a.FireAt == "" {
			return "", fmt.Errorf("fire_at is required")
		}
		result, err := h.createSchedule(ctx, scheduleCreateRequest{FromPeer: caller, ToPeer: a.ToPeer, Text: a.Text, FireAt: strPtr(a.FireAt), Cron: strPtr(a.Cron), Kind: firstNonempty(a.Kind, "notify"), Circle: strPtr(a.Circle)})
		return result.ScheduleID, err
	}
}

func firstNonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func required(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func requireFields(fields ...string) error {
	for index := 0; index+1 < len(fields); index += 2 {
		if err := required(fields[index], fields[index+1]); err != nil {
			return err
		}
	}
	return nil
}
func defaultInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
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
