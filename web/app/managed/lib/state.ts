import type { Agent, AgentState, Client, ManagedFrame, Run, Workspace } from "./api";

// Attention first: a silent failure is what this view exists to surface.
const STATE_RANK: Record<AgentState, number> = { failed: 0, working: 1, parked: 2, idle: 3, retired: 4 };

export interface StateMeta {
  label: string;
  dot: string;
  text: string;
  hint: string;
}

export const STATE_META: Record<AgentState, StateMeta> = {
  failed: { label: "FAILED", dot: "bg-error", text: "text-error", hint: "The last run did not answer." },
  working: { label: "WORKING", dot: "bg-tertiary-fixed-dim glow-busy animate-pulse", text: "text-tertiary-fixed-dim", hint: "A run is in progress." },
  parked: { label: "PARKED", dot: "border border-primary bg-transparent", text: "text-primary", hint: "Waiting on something outside the run, such as a peer reply." },
  idle: { label: "IDLE", dot: "bg-secondary", text: "text-secondary", hint: "Ready. The next message starts a run." },
  retired: { label: "RETIRED", dot: "border border-outline bg-transparent", text: "text-outline line-through", hint: "Refuses new input. The transcript stays readable." },
};

/** Sorted for the list: failed, working, parked, idle, retired; most recent change first within a state. */
export function sortAgents(agents: Agent[]): Agent[] {
  return [...agents].sort(
    (a, b) => STATE_RANK[a.state] - STATE_RANK[b.state] || b.state_at.localeCompare(a.state_at) || a.id.localeCompare(b.id),
  );
}

export function visibleAgents(agents: Agent[], showRetired: boolean): Agent[] {
  return sortAgents(showRetired ? agents : agents.filter((agent) => agent.state !== "retired"));
}

export function agentLabel(agent: Pick<Agent, "id" | "display_name">): string {
  return agent.display_name || agent.id;
}

/** The muted second line under an agent's name, relative to `now`. */
export function agentSubline(agent: Agent, now: number): string {
  switch (agent.state) {
    case "working":
      return `run ${formatDuration(now - Date.parse(agent.state_at))}`;
    case "failed":
      return [agent.last_reason || "failed", formatAgo(agent.state_at, now)].join(" · ");
    case "retired":
      return `retired ${formatDate(agent.retired_at ?? agent.state_at)}`;
    case "parked":
      return [agent.last_reason, `since ${formatAgo(agent.state_at, now)}`].filter(Boolean).join(" · ");
    default:
      return `last active ${formatAgo(agent.state_at, now)}`;
  }
}

/** How a run reads in the Runs tab. An abort is a stop the user asked for, not a failure. */
export function runStatus(run: Run): { label: string; cls: string; failure: boolean } {
  if (run.status === "working") return { label: "WORKING", cls: "text-tertiary-fixed-dim", failure: false };
  if (run.status === "done") return { label: "ANSWERED", cls: "text-secondary", failure: false };
  if (run.reason === "aborted") return { label: "ABORTED", cls: "text-outline", failure: false };
  return { label: "UNANSWERED", cls: "text-error", failure: true };
}

export function stateCounts(agents: Agent[]): Record<AgentState, number> {
  const counts: Record<AgentState, number> = { failed: 0, working: 0, parked: 0, idle: 0, retired: 0 };
  for (const agent of agents) counts[agent.state] += 1;
  return counts;
}

/** The connected gateway daemon, if any. Mesh tools fail without one. */
export function connectedDaemon(clients: Client[]): Client | null {
  return clients.find((client) => client.kind === "daemon" && client.connected) ?? null;
}

export interface LiveState {
  agents: Agent[];
  workspaces: Workspace[];
  clients: Client[];
  outbox: number;
  runs: Record<string, Run[]>;
  at: string | null;
}

export const EMPTY_LIVE: LiveState = { agents: [], workspaces: [], clients: [], outbox: 0, runs: {}, at: null };

function upsert<T>(items: T[], next: T, same: (item: T) => boolean): T[] {
  const index = items.findIndex(same);
  if (index === -1) return [next, ...items];
  const copy = items.slice();
  copy[index] = next;
  return copy;
}

const clientKey = (client: Client) => client.daemon_id ?? client.machine ?? client.connected_at;

/** A live frame, or runs fetched over HTTP for one agent's Runs tab. */
export type LiveAction = ManagedFrame | { type: "runs"; agent: string; runs: Run[] };

/**
 * Fold one live frame into page state. A snapshot replaces everything it
 * carries (there is no event cursor to recover), deltas upsert by id.
 */
export function applyFrame(state: LiveState, frame: LiveAction): LiveState {
  switch (frame.type) {
    case "runs":
      return { ...state, runs: { ...state.runs, [frame.agent]: frame.runs } };
    case "snapshot":
      return { ...state, agents: frame.agents, workspaces: frame.workspaces, clients: frame.clients, outbox: frame.outbox, at: frame.at };
    case "agent":
      return { ...state, agents: upsert(state.agents, frame.agent, (a) => a.id === frame.agent.id) };
    case "run": {
      const runs = state.runs[frame.agent];
      // Runs are only kept for agents whose Runs tab has loaded; otherwise the
      // next load fetches them fresh.
      if (!runs) return state;
      return { ...state, runs: { ...state.runs, [frame.agent]: upsert(runs, frame.run, (r) => r.op === frame.run.op) } };
    }
    case "workspace":
      return { ...state, workspaces: upsert(state.workspaces, frame.workspace, (w) => w.id === frame.workspace.id) };
    case "client":
      return { ...state, clients: upsert(state.clients, frame.client, (c) => clientKey(c) === clientKey(frame.client)) };
    case "outbox":
      return { ...state, outbox: frame.depth };
    default:
      return state;
  }
}

export function isFrame(value: unknown): value is ManagedFrame {
  if (!value || typeof value !== "object") return false;
  const type = (value as { type?: unknown }).type;
  return type === "snapshot" || type === "agent" || type === "run" || type === "workspace" || type === "client" || type === "outbox";
}

// Formatting. Times are ISO 8601 UTC strings from the server and are shown in
// the viewer's local time.

export function formatDuration(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

export function formatAgo(iso: string | null | undefined, now: number): string {
  if (!iso) return "never";
  const secs = Math.max(0, Math.floor((now - Date.parse(iso)) / 1000));
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

export function formatClock(iso: string | null | undefined): string {
  if (!iso) return "-";
  return new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return "-";
  return new Date(iso).toLocaleDateString([], { month: "short", day: "numeric" });
}

/** Clock time today, else month and day. */
export function formatWhen(iso: string | null | undefined, now: number): string {
  if (!iso) return "-";
  const date = new Date(iso);
  return date.toDateString() === new Date(now).toDateString() ? formatClock(iso) : formatDate(iso);
}

export function formatUsd(value: number): string {
  if (value > 0 && value < 0.01) return "<$0.01";
  return `$${value.toFixed(2)}`;
}

/** `token-99209d…08e6`: long ids keep a recognisable prefix and suffix. */
export function shortId(id: string, head = 6, tail = 4): string {
  const dash = id.indexOf("-") + 1;
  const body = id.slice(dash);
  if (body.length <= head + tail + 1) return id;
  return `${id.slice(0, dash)}${body.slice(0, head)}…${body.slice(-tail)}`;
}

export function formatTokens(value: number): string {
  return value.toLocaleString("en-US");
}
