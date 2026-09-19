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
  // Provenance: where the peer came from and whether the mesh may address it.
  // Flattened on the wire; a peer registered before provenance existed reads
  // as source "unknown" and addressable.
  source?: "hook" | "codex-app-server" | "unknown";
  initiator?: "user" | "agent" | "system";
  parent_runtime_id?: string;
  parent_peer_id?: string | null;
  ephemeral?: boolean;
  addressable?: boolean;
  addressable_reason?: string;
  metadata?: {
    branch?: string;
    agent_nickname?: string;
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

/** False only when the runtime said so; undefined (older daemon) means addressable. */
export function peerAddressable(peer: Peer): boolean {
  return peer.addressable !== false;
}

/** Short origin badge for rosters: the runtime nickname for sub-agent threads, else the source. */
export function peerOriginBadge(peer: Peer): string | null {
  if (!peerAddressable(peer)) return `no input${peer.metadata?.agent_nickname ? ` · ${peer.metadata.agent_nickname}` : ""}`;
  if (peer.initiator === "system") return "system";
  if (peer.metadata?.agent_nickname) return peer.metadata.agent_nickname;
  if (peer.source === "codex-app-server") return "app-server";
  return null;
}

/** The registered parent of a sub-agent thread, if it is on the mesh. */
export function peerParent(peer: Peer, peers: Peer[]): Peer | undefined {
  return peer.parent_peer_id ? peers.find((candidate) => candidate.peer_id === peer.parent_peer_id) : undefined;
}

/**
 * Mirrors the daemon's default-view rule (list_peers, `peer list`): a peer
 * someone can send work to. The dashboard shows the full inventory and dims
 * the rest.
 */
export function peerListed(peer: Peer, peers: Peer[]): boolean {
  if (!peerAddressable(peer) || peer.initiator === "system") return false;
  if (peer.parent_runtime_id) {
    const parent = peerParent(peer, peers);
    return Boolean(parent) && parent!.status !== "offline";
  }
  return true;
}

const LIFECYCLE_EVENT_TYPES: ReadonlySet<Event["type"]> = new Set([
  "peer_online",
  "peer_offline",
  "peer_status",
  "peer_contradiction",
  "peer_reaped",
  "peer_updated",
  "peer_not_addressable",
  "status_change",
]);

/** Registry lifecycle events: they describe one peer rather than a routed message. */
export function isLifecycleEvent(event: Event): boolean {
  return LIFECYCLE_EVENT_TYPES.has(event.type);
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
    | "peer_online"
    | "peer_offline"
    | "peer_status"
    | "peer_contradiction"
    | "peer_reaped"
    | "peer_updated"
    | "peer_not_addressable";
  timestamp: string;
  from?: string;
  to?: string;
  from_peer_id?: string;
  to_peer_id?: string;
  text?: string;
  attachments?: AttachmentRef[];
  status?: "pending" | "success" | "error" | "blocked" | Peer["status"];
  delivered?: boolean;
  has_message?: boolean;
  has_attachments?: boolean;
  peer?: string;
  peer_id?: string;
  peer_name?: string;
  display_name?: string;
  backend?: string;
  path?: string;
  reason?: string;
  // peer_contradiction fields
  code?: string;
  detail?: string;
  severity?: string;
  // peer_updated (provenance) fields
  source?: string;
  addressable?: boolean;
  addressable_reason?: string;
  // peer_not_addressable (demotion) fields
  asks_closed?: number;
  deliveries_dropped?: number;
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
