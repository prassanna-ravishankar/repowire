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
  last_seen?: string;
  metadata?: {
    branch?: string;
    [key: string]: unknown;
  };
}

export interface Event {
  id: string;
  type: "query" | "response" | "notification" | "broadcast" | "status_change" | "chat_turn";
  timestamp: string;
  from?: string;
  to?: string;
  text: string;
  status?: "pending" | "success" | "error" | "blocked";
  peer?: string;
  role?: "user" | "assistant";
  new_status?: "online" | "busy" | "offline";
  query_id?: string;
  correlation_id?: string;
}
