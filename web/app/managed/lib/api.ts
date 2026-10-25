// Typed client for the relay's managed and admin routes. Shapes follow the
// Repowire Managed build contract (sections 1 and 2). Every call is relative
// to the page origin: the relay serves these pages and the API together, and
// the rw_token cookie authenticates both.

export type AgentState = "idle" | "working" | "parked" | "failed" | "retired";

export interface Agent {
  id: string;
  owner?: string;
  workspace: string | null;
  display_name: string | null;
  state: AgentState;
  state_at: string;
  last_op: string | null;
  last_reason: string | null;
  description: string | null;
  home_machine: string | null;
  created_at: string;
  created_by: string;
  retired_at: string | null;
  runs: number;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
}

export interface AgentDetail extends Agent {
  busy: boolean;
  pending: { operationId: string; status: string }[];
}

export interface Run {
  op: string;
  source: string | null;
  started_at: string;
  settled_at: string | null;
  status: "working" | "done" | "unanswered";
  reason: string | null;
  tokens_in: number;
  tokens_out: number;
  cost_usd: number;
}

export interface Workspace {
  id: string;
  agents: string[];
  exec_count: number;
  container_ms: number;
  last_exec_at: string | null;
  snapshot_at: string | null;
  state: "idle" | "running" | "needs_repair";
}

export interface Client {
  kind: "daemon";
  machine: string | null;
  daemon_id: string | null;
  version: string | null;
  connected_at: string;
  connected: boolean;
  disconnected_at?: string;
}

export interface Usage {
  runs: number;
  tokens_in: number;
  tokens_out: number;
  model_usd: number;
  execs: number;
  container_ms: number;
  container_usd: number;
  cost_usd: number;
}

export interface Me {
  user_id: string;
  admin: boolean;
  managed: boolean;
}

export interface RelayDaemon {
  daemon_id: string;
  connected_at: string;
  connected: true;
}

export interface OAuthApp {
  grant_id: string;
  client_id: string;
  client_name?: string;
  scope: string;
  expires: string;
  active: boolean;
}

export interface Clients {
  daemons: RelayDaemon[];
  gateway: Client[];
  apps: OAuthApp[];
  outbox: number;
}

export interface Receipt {
  operationId: string;
  [key: string]: unknown;
}

export type ManagedFrame =
  | { type: "snapshot"; agents: Agent[]; workspaces: Workspace[]; clients: Client[]; outbox: number; at: string }
  | { type: "agent"; agent: Agent }
  | { type: "run"; agent: string; run: Run }
  | { type: "workspace"; workspace: Workspace }
  | { type: "client"; client: Client }
  | { type: "outbox"; depth: number };

export interface AdminSummary {
  owners: number;
  agents_by_state: Record<AgentState, number>;
  workspaces: number;
  running_containers: number;
  connected_daemons: number;
  today: Usage;
  month: Usage;
  relay?: { daemons_connected: number; users_with_daemons: number; oauth_grants_active: number };
}

export interface AdminOwner {
  owner: string;
  agents: number;
  workspaces: number;
  last_active_at: string | null;
  month: Usage;
}

export type AdminSandbox = Workspace & { owner: string };

export interface CostDay {
  day: string;
  model_usd: number;
  container_usd: number;
  do_usd: number;
  total_usd: number;
}

export interface CostSeries {
  days: CostDay[];
  month_usd: number;
}

export interface Costs {
  estimate: CostSeries;
  actual: CostSeries | null;
  alerts: number[];
  as_of: string;
}

/** A failed request: the HTTP status and the server's `error` text, verbatim. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      credentials: "same-origin",
      ...init,
      headers: init?.body ? { "Content-Type": "application/json", ...init.headers } : init?.headers,
    });
  } catch (error) {
    throw new ApiError(0, error instanceof Error ? error.message : "network error");
  }
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    const message =
      data && typeof data === "object" && "error" in data && typeof data.error === "string"
        ? data.error
        : typeof data === "string" && data.trim()
          ? data.trim()
          : `${res.status} ${res.statusText}`.trim();
    throw new ApiError(res.status, message);
  }
  return data as T;
}

const json = (method: string, body?: unknown): RequestInit => ({
  method,
  body: body === undefined ? undefined : JSON.stringify(body),
});

const M = "/api/v1/managed";
const A = "/api/v1/admin";
const seg = encodeURIComponent;

export const managedApi = {
  me: () => request<Me>(`${M}/me`),
  agents: () => request<{ items: Agent[] }>(`${M}/agents`).then((r) => r.items),
  agent: (id: string) => request<AgentDetail>(`${M}/agents/${seg(id)}`),
  createAgent: (body: { id: string; workspace?: string }) =>
    request<{ created: boolean; agent: Agent }>(`${M}/agents`, json("POST", body)),
  retire: (id: string) => request<{ agent: Agent }>(`${M}/agents/${seg(id)}`, json("DELETE")).then((r) => r.agent),
  messages: (id: string) => request<unknown>(`${M}/agents/${seg(id)}/messages`),
  send: (id: string, operationId: string, text: string) =>
    request<Receipt>(`${M}/agents/${seg(id)}/messages`, json("POST", { operationId, text, source: "web" })),
  runs: (id: string) => request<{ items: Run[] }>(`${M}/agents/${seg(id)}/runs`).then((r) => r.items),
  abort: (id: string, operationId?: string) =>
    request<{ aborted: boolean }>(`${M}/agents/${seg(id)}/abort`, json("POST", operationId ? { operationId } : {})),
  workspaces: () => request<{ items: Workspace[] }>(`${M}/workspaces`).then((r) => r.items),
  clients: () => request<Clients>(`${M}/clients`),
  revokeApp: (grantId: string) => request<unknown>(`${M}/apps/${seg(grantId)}`, json("DELETE")),
};

export const adminApi = {
  summary: () => request<AdminSummary>(`${A}/summary`),
  owners: () => request<{ items: AdminOwner[] }>(`${A}/owners`).then((r) => r.items),
  sandboxes: () => request<{ items: AdminSandbox[] }>(`${A}/sandboxes`).then((r) => r.items),
  costs: (refresh = false) => request<Costs>(`${A}/costs${refresh ? "?refresh=1" : ""}`),
};

export const MANAGED_EVENTS_URL = `${M}/events`;
