export interface Peer {
  peer_id: string;
  name: string;
  display_name: string;
  status: "online" | "busy" | "offline";
  turn_state?: "idle" | "working" | "awaiting_input" | "pending_first_turn" | null;
  machine: string;
  path: string;
  tmux_session?: string;
  pane_id?: string | null;
  backend?: string;
  model?: string | null;
  circle: string;
  role?: "agent" | "service" | "orchestrator" | "human";
  last_seen?: string;
  description?: string;
  metadata?: {
    branch?: string;
    git_status?: {
      ahead: number;
      behind: number;
      dirty: number;
      staged: number;
    };
    [key: string]: unknown;
  };
}

export interface OrchestratorStatus {
  circle: string;
  present: boolean;
  peer_id?: string | null;
  peer_name?: string | null;
  display_name?: string | null;
  last_seen?: string | null;
  stale_after?: string | null;
}

export interface JobStatus {
  job_id?: string;
  work_id?: string;
  title?: string;
  kind?: string;
  state?: string;
  state_reason?: string | null;
  phase?: string | null;
  progress?: Record<string, unknown>;
  progress_events?: { note?: string; at?: string; timestamp?: string; [key: string]: unknown }[];
  owner_peer_id?: string | null;
  assigned_peer_id?: string | null;
  repowire_session_id?: string | null;
  correlation_id?: string | null;
  circle?: string | null;
  created_by_peer_id?: string | null;
  source_kind?: string | null;
  source_id?: string | null;
  scope?: string | null;
  visibility?: string;
  created_at?: string;
  updated_at?: string;
  deadline_at?: string | null;
  expires_at?: string | null;
  result_summary?: string | null;
  cancel_requested?: boolean;
  cancellation_reason?: string | null;
  request?: Record<string, unknown>;
  execution?: {
    prompt?: { body?: string; source?: string; source_path?: string };
    target?: { path?: string; backend?: string; profile?: string; assigned_peer_id?: string };
    schedule?: { due_at?: string };
    delivery?: { kind?: string; result_surface?: string };
    [key: string]: unknown;
  };
  runner?: Record<string, unknown>;
  due_at?: string | null;
  links?: Record<string, unknown>;
}

export interface RecurringJobStatus {
  calendar_id: string;
  recurring_id?: string;
  title?: string;
  kind?: string;
  state?: string;
  cron?: string;
  next_due_at?: string;
  owner_peer_id?: string | null;
  assigned_peer_id?: string | null;
  circle?: string | null;
  created_by_peer_id?: string | null;
  source_kind?: string | null;
  source_id?: string | null;
  scope?: string | null;
  visibility?: string;
  request?: Record<string, unknown>;
  execution?: JobStatus["execution"];
  last_occurrence_work_id?: string | null;
  last_materialized_at?: string | null;
  created_at?: string;
  updated_at?: string;
}

export interface JobsResponse {
  work: JobStatus[];
  recurring: RecurringJobStatus[];
}

export interface DaemonHealth {
  status: string;
  version: string;
  relay_mode?: boolean;
  channel?: {
    status?: string;
    configured?: boolean;
    runtime_available?: boolean;
    last_error?: string | null;
  };
  acp_broker?: {
    status?: string;
    enabled?: boolean;
    sdk_available?: boolean;
    manager_initialized?: boolean;
    configured_peers?: number;
    active_clients?: number;
    in_flight?: number;
    last_error?: string | null;
    permissions?: {
      pending?: number;
      last_error?: string | null;
    };
  };
}

export interface AttachmentRef {
  id?: string | null;
  path?: string | null;
  filename?: string | null;
  size?: number | null;
  content_type?: string | null;
}

/** Human-readable label: display_name is daemon-assigned and human-friendly. */
export function peerLabel(peer: Peer): string {
  return peer.display_name || peer.name;
}

export interface Event {
  id: string;
  type:
    | "query"
    | "response"
    | "notification"
    | "broadcast"
    | "status_change"
    | "chat_turn"
    | "chat_turn_delta"
    | "ask"
    | "ack"
    | "peer_reaped";
  timestamp: string;
  from?: string;
  to?: string;
  from_peer_id?: string;
  to_peer_id?: string;
  text?: string;
  attachments?: AttachmentRef[];
  status?: "pending" | "success" | "error" | "blocked";
  delivered?: boolean;
  has_message?: boolean;
  has_attachments?: boolean;
  peer?: string;
  peer_id?: string;
  display_name?: string;
  backend?: string;
  path?: string;
  reason?: string;
  role?: "user" | "assistant";
  new_status?: "online" | "busy" | "offline";
  query_id?: string;
  correlation_id?: string;
  session_id?: string;
  tool_calls?: { name: string; input: string }[];
  // chat_turn_delta fields
  turn_id?: string;
  chunk_index?: number;
  kind?: "text" | "tool_use";
  tool_call?: { name: string; input: string };
  is_final?: boolean;
  // structured question carried on an ask event (mesh questions primitive)
  question?: AskQuestion | null;
}

export interface AskQuestionOption {
  id: string;
  title: string;
  description?: string | null;
}

export interface AskQuestion {
  kind: "acknowledge" | "choice" | "text";
  prompt?: string | null;
  options?: AskQuestionOption[];
  blocking?: boolean;
  scope?: string | null;
}
