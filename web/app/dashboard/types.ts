export interface Peer {
  peer_id: string;
  name: string;
  display_name: string;
  status: "online" | "busy" | "offline";
  machine: string;
  path: string;
  tmux_session?: string;
  backend?: string;
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
    | "ask";
  timestamp: string;
  from?: string;
  to?: string;
  from_peer_id?: string;
  to_peer_id?: string;
  text: string;
  status?: "pending" | "success" | "error" | "blocked";
  peer?: string;
  peer_id?: string;
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
}
