import { DurableObject } from 'cloudflare:workers';
import { admission, deriveState, meshWindow, utcDay, type AgentState } from './ledger';
import { execCost, modelCost, type TokenUsage } from './prices';
import { followUp, managedTool, type MeshEnv } from './mesh';
import { MANAGED, remoteResult } from './protocol';
import type { ExecOutcome } from './workspace';

// How long an agent's tool call waits for the daemon before failing.
const CALL_TIMEOUT_MS = 60_000;
// Keep an outbox entry well inside the storage value limit.
export const REPORT_TEXT_LIMIT = 64_000;
const RECENT_CLIENTS = 20;
const MESH_TOOLS = new Set(['notify_peer', 'ask']);

export interface GatewayCall { id: string; agent: string; tool: string; args: Record<string, unknown> }

// An agent on the managed machine: created through the relay and claimed by
// no daemon. Rows a daemon created, including lazily repaired ones without a
// home, stay with the daemon.
export const onManaged = (row: { created_by: unknown; home_machine: unknown }) => row.created_by !== 'daemon' && !row.home_machine;
export interface GatewayReport {
  agent: string; operationId: string; status: string; reason?: string; text?: string; usage?: TokenUsage;
  // The run's admission, so a report that arrives first can create its row.
  source?: string | null; startedAt?: string;
  // Who submitted it. Only the daemon's runs go to its outbox; the relay's
  // are answered from the run log.
  via?: Via;
}
export type Via = 'daemon' | 'control';
export interface RelayRequest {
  id: string; agent: string; kind: 'ask' | 'notify'; requester: string; text: string;
  created_at: string; state: 'submitting' | 'accepted' | 'not_delivered';
}
// A finished command as the agent reports it: the workspace's own outcome, or
// when the workspace could not answer, the agent's elapsed time and no state.
export type ExecReport = Pick<ExecOutcome, 'wall_ms' | 'snapshot_at'> & { dirty: boolean | null };
export interface AgentInit { id: string; workspace: string | null; display_name?: string | null; created_by: string; home_machine?: string | null; circle?: string }
interface Pending { socket: WebSocket; resolve(text: string): void; reject(error: Error): void }
// A cross-machine request as the source sent it; immutable once recorded.
export interface RemoteEnvelope {
  id: string; kind: 'ask' | 'notify'; from_machine: string; from_peer: string; from_name: string; from_circle: string;
  to_agent: string; text: string; created_at: number;
}
export interface RemoteInbound extends RemoteEnvelope { state: string; close_reason: string | null; reply: string | null }
export interface RemoteOutboundInit {
  id: string; machine: string; store_epoch: string; to_peer: string; to_name: string; from_agent: string; text: string; created_at: number;
}
export interface RemoteOutbound extends RemoteOutboundInit {
  state: string; close_reason: string | null; reply: string | null; delivered: number; disposition: string | null; checked_at: number;
}

export interface Client {
  kind: 'daemon'; machine: string | null; daemon_id: string | null; version: string | null;
  connected_at: string; connected: boolean; disconnected_at?: string;
}

const now = () => new Date().toISOString();

const SCHEMA = `
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, workspace TEXT, display_name TEXT, description TEXT, home_machine TEXT, circle TEXT NOT NULL DEFAULT 'managed',
  created_at TEXT NOT NULL, created_by TEXT NOT NULL, retired_at TEXT,
  state TEXT NOT NULL DEFAULT 'idle', state_at TEXT NOT NULL, last_op TEXT, last_reason TEXT);
CREATE TABLE IF NOT EXISTS runs (
  agent TEXT NOT NULL, op TEXT NOT NULL, source TEXT, started_at TEXT NOT NULL, settled_at TEXT,
  status TEXT NOT NULL, reason TEXT, tokens_in INTEGER NOT NULL DEFAULT 0, tokens_out INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0, PRIMARY KEY (agent, op));
CREATE TABLE IF NOT EXISTS usage_event (
  id TEXT PRIMARY KEY, at TEXT NOT NULL, day TEXT NOT NULL, agent TEXT NOT NULL, kind TEXT NOT NULL,
  tokens_in INTEGER NOT NULL DEFAULT 0, tokens_out INTEGER NOT NULL DEFAULT 0, wall_ms INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL, detail TEXT);
CREATE TABLE IF NOT EXISTS usage_day (
  day TEXT NOT NULL, agent TEXT NOT NULL, kind TEXT NOT NULL, events INTEGER NOT NULL DEFAULT 0,
  tokens_in INTEGER NOT NULL DEFAULT 0, tokens_out INTEGER NOT NULL DEFAULT 0, wall_ms INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0, PRIMARY KEY (day, agent, kind));
CREATE TABLE IF NOT EXISTS requests (
  id TEXT PRIMARY KEY, agent TEXT NOT NULL, kind TEXT NOT NULL, requester TEXT NOT NULL, text TEXT NOT NULL,
  created_at TEXT NOT NULL, state TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS requests_requester ON requests (requester, created_at);
CREATE TABLE IF NOT EXISTS remote_inbound (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, from_machine TEXT NOT NULL, from_peer TEXT NOT NULL, from_name TEXT NOT NULL,
  from_circle TEXT NOT NULL, to_agent TEXT NOT NULL, text TEXT NOT NULL, created_at INTEGER NOT NULL, state TEXT NOT NULL,
  close_reason TEXT, reply TEXT, settled_at INTEGER);
CREATE INDEX IF NOT EXISTS remote_inbound_state ON remote_inbound (state, created_at);
CREATE TABLE IF NOT EXISTS remote_fences (
  id TEXT NOT NULL, from_machine TEXT NOT NULL, from_peer TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (id, from_machine));
CREATE TABLE IF NOT EXISTS remote_outbound (
  id TEXT PRIMARY KEY, machine TEXT NOT NULL, store_epoch TEXT NOT NULL, to_peer TEXT NOT NULL, to_name TEXT NOT NULL,
  from_agent TEXT NOT NULL, text TEXT NOT NULL, created_at INTEGER NOT NULL, state TEXT NOT NULL,
  close_reason TEXT, reply TEXT, delivered INTEGER NOT NULL DEFAULT 0, disposition TEXT, checked_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS remote_outbound_open ON remote_outbound (state, delivered, checked_at);
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY, exec_count INTEGER NOT NULL DEFAULT 0, container_ms INTEGER NOT NULL DEFAULT 0,
  last_exec_at TEXT, running INTEGER NOT NULL DEFAULT 0, snapshot_at TEXT, dirty INTEGER NOT NULL DEFAULT 0);
`;
// Columns added after the first deploy, for directories created before them.
const ADDED_COLUMNS = [
  'ALTER TABLE workspaces ADD COLUMN snapshot_at TEXT',
  'ALTER TABLE workspaces ADD COLUMN dirty INTEGER NOT NULL DEFAULT 0',
  'ALTER TABLE runs ADD COLUMN answer TEXT',
  "ALTER TABLE agents ADD COLUMN circle TEXT NOT NULL DEFAULT 'managed'",
];

// A run as listed and pushed; its answer is fetched one run at a time.
const RUN = 'agent, op, source, started_at, settled_at, status, reason, tokens_in, tokens_out, cost_usd';

type Row = Record<string, SqlStorageValue>;
type Usage = ReturnType<typeof emptyUsage>;

const emptyUsage = () => ({ runs: 0, tokens_in: 0, tokens_out: 0, model_usd: 0, execs: 0, container_ms: 0, container_usd: 0, cost_usd: 0 });

function addUsage(total: Usage, row: Row) {
  const cost = Number(row.cost_usd);
  if (row.kind === 'model') {
    total.runs += Number(row.events); total.tokens_in += Number(row.tokens_in); total.tokens_out += Number(row.tokens_out); total.model_usd += cost;
  } else if (row.kind === 'exec') {
    total.execs += Number(row.events); total.container_ms += Number(row.wall_ms); total.container_usd += cost;
  }
  total.cost_usd += cost;
}

/**
 * One owner's Durable Object: their directory of agents, runs and workspaces,
 * their usage ledger, and their link to the Repowire daemon.
 *
 * The daemon dials in and holds a hibernating WebSocket (tag `daemon`);
 * agents' mesh tool calls go out over it and the daemon runs them as the
 * calling hosted peer. Settled runs are reported through a durable outbox,
 * resent on every connect until the daemon acknowledges them. The relay
 * watches the owner over a second socket kind (tag `ui`) that gets a snapshot
 * and then every change. Agents and workspaces push their state here, so
 * listing never wakes an agent. Protocol pings are answered by the runtime,
 * so idle links never wake this object.
 */
export class Gateway extends DurableObject {
  private pending = new Map<string, Pending>();
  private sql = this.ctx.storage.sql;

  constructor(ctx: DurableObjectState, env: unknown) {
    super(ctx, env as never);
    this.sql.exec(SCHEMA);
    for (const statement of ADDED_COLUMNS) {
      try { this.sql.exec(statement); } catch (error) {
        if (!/duplicate column/i.test(String(error))) throw error;
      }
    }
  }

  async fetch(request: Request): Promise<Response> {
    if (request.headers.get('Upgrade') !== 'websocket') return new Response('Expected WebSocket', { status: 426 });
    const [client, server] = Object.values(new WebSocketPair());
    if (new URL(request.url).pathname === '/events') {
      this.ctx.acceptWebSocket(server, ['ui']);
      server.send(JSON.stringify(await this.snapshot()));
      return new Response(null, { status: 101, webSocket: client });
    }
    // One daemon per owner: a reconnect replaces a socket that may be half open.
    for (const old of this.ctx.getWebSockets('daemon')) {
      await this.webSocketClose(old);
      old.close(1012, 'Replaced by a newer daemon connection');
    }
    const header = (name: string) => request.headers.get(name)?.slice(0, 200) || null;
    const info: Client = {
      kind: 'daemon', machine: header('X-Repowire-Machine'), daemon_id: header('X-Repowire-Daemon-Id'),
      version: header('X-Repowire-Version'), connected_at: now(), connected: true,
    };
    server.serializeAttachment(info);
    this.ctx.acceptWebSocket(server, ['daemon']);
    for (const [id, report] of await this.ctx.storage.list<GatewayReport>({ prefix: 'outbox:' })) {
      server.send(JSON.stringify({ type: 'settled', id, ...report }));
    }
    this.ui({ type: 'client', client: info });
    return new Response(null, { status: 101, webSocket: client });
  }

  // ---- directory ----------------------------------------------------------

  // The owner this object belongs to, recorded by the first caller that names it.
  async owner(name?: string): Promise<string | undefined> {
    if (name && !(await this.ctx.storage.get('owner'))) await this.ctx.storage.put('owner', name);
    return this.ctx.storage.get<string>('owner');
  }

  // Ownership never changes until phase 4 (rfcs/managed-in-the-mesh.md): a
  // create that would move an agent between managed and a daemon, or
  // between daemons, is refused, so each runtime keeps one ask authority. A
  // daemon may fill the home of its own row created before homes existed.
  addAgent(init: AgentInit) {
    const existing = this.sql.exec('SELECT created_by, home_machine FROM agents WHERE id = ?', init.id).toArray()[0];
    const home = init.home_machine ?? null;
    if (existing) {
      const daemonOwned = (by: unknown) => by === 'daemon';
      if (daemonOwned(existing.created_by) !== daemonOwned(init.created_by)) {
        return { conflict: `Agent ${init.id} belongs to ${daemonOwned(existing.created_by) ? 'a daemon' : 'managed'}` };
      }
      if (existing.home_machine && home && existing.home_machine !== home) {
        return { conflict: `Agent ${init.id} belongs to machine ${existing.home_machine}` };
      }
    }
    const at = now();
    this.sql.exec(
      `INSERT INTO agents (id, workspace, display_name, home_machine, circle, created_at, created_by, state, state_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, 'idle', ?)
       ON CONFLICT (id) DO UPDATE SET display_name = COALESCE(excluded.display_name, agents.display_name),
         home_machine = COALESCE(agents.home_machine, excluded.home_machine)`,
      init.id, init.workspace, init.display_name ?? null, home, init.circle ?? 'managed', at, init.created_by, at);
    if (init.workspace) this.sql.exec('INSERT OR IGNORE INTO workspaces (id) VALUES (?)', init.workspace);
    return this.publish(init.id);
  }

  agents() { return this.sql.exec('SELECT * FROM agents ORDER BY created_at').toArray().map((row) => this.agentRow(row)); }

  agent(id: string) {
    const row = this.sql.exec('SELECT * FROM agents WHERE id = ?', id).toArray()[0];
    return row ? this.agentRow(row) : null;
  }

  setCircle(id: string, circle: string) {
    this.sql.exec('UPDATE agents SET circle = ? WHERE id = ?', circle, id);
    return this.publish(id);
  }

  describe(id: string, description: string) {
    this.sql.exec('UPDATE agents SET description = ? WHERE id = ?', description.slice(0, 200), id);
    return this.publish(id);
  }

  retire(id: string) {
    this.sql.exec('UPDATE agents SET retired_at = COALESCE(retired_at, ?) WHERE id = ?', now(), id);
    return this.refresh(id);
  }

  runs(agent: string) {
    return this.sql.exec(`SELECT ${RUN} FROM runs WHERE agent = ? ORDER BY started_at DESC LIMIT 100`, agent).toArray();
  }

  run(agent: string, op: string, answer = false) {
    return this.sql.exec(`SELECT ${RUN}${answer ? ', answer' : ''} FROM runs WHERE agent = ? AND op = ?`, agent, op).toArray()[0] ?? null;
  }

  // ---- requests -----------------------------------------------------------
  // The relay's asks and notifies. A request's id is its run's operation id,
  // so its result is that run.

  request(id: string): RelayRequest | null {
    const row = this.sql.exec('SELECT * FROM requests WHERE id = ?', id).toArray()[0];
    return row ? (row as unknown as RelayRequest) : null;
  }

  // Records a request before it is submitted. Returns the existing one for a
  // known id, so a retry never resubmits.
  openRequest(init: Omit<RelayRequest, 'state' | 'created_at'>): { request: RelayRequest; created: boolean } {
    const created = this.sql.exec(
      `INSERT OR IGNORE INTO requests (id, agent, kind, requester, text, created_at, state) VALUES (?, ?, ?, ?, ?, ?, 'submitting')`,
      init.id, init.agent, init.kind, init.requester, init.text, now()).rowsWritten > 0;
    return { request: this.request(init.id)!, created };
  }

  acceptRequest(id: string) { this.sql.exec("UPDATE requests SET state = 'accepted' WHERE id = ? AND state = 'submitting'", id); }

  // The agent confirmed it never took the request and will refuse it.
  requestUndelivered(id: string) { this.sql.exec("UPDATE requests SET state = 'not_delivered' WHERE id = ? AND state = 'submitting'", id); }

  // A request the agent refused as retired before recording it: forgotten.
  dropRequest(id: string) { this.sql.exec("DELETE FROM requests WHERE id = ? AND state = 'submitting'", id); }

  // A requester's asks that have not settled, newest last.
  openRequests(requester: string) {
    return this.sql.exec(
      `SELECT q.id, q.agent, q.text, q.created_at FROM requests q LEFT JOIN runs r ON r.agent = q.agent AND r.op = q.id
       WHERE q.requester = ? AND q.kind = 'ask' AND q.state != 'not_delivered' AND r.settled_at IS NULL
       ORDER BY q.created_at LIMIT 100`, requester).toArray();
  }

  workspaces() {
    const users = new Map<string, string[]>();
    for (const row of this.sql.exec('SELECT id, workspace FROM agents WHERE workspace IS NOT NULL').toArray()) {
      const workspace = row.workspace as string;
      users.set(workspace, [...(users.get(workspace) ?? []), row.id as string]);
    }
    return this.sql.exec('SELECT * FROM workspaces ORDER BY id').toArray().map((row) => ({
      id: row.id as string, agents: users.get(row.id as string) ?? [], exec_count: Number(row.exec_count),
      container_ms: Number(row.container_ms), last_exec_at: row.last_exec_at, snapshot_at: row.snapshot_at,
      // A failed checkpoint outranks activity: the files cannot be restored.
      state: row.dirty ? 'needs_repair' : row.running ? 'running' : 'idle',
    }));
  }

  async clients(): Promise<{ items: Client[]; outbox: number }> {
    const live = this.ctx.getWebSockets('daemon').map((socket) => socket.deserializeAttachment() as Client);
    const recent = (await this.ctx.storage.get<Client[]>('clients:recent')) ?? [];
    return { items: [...live, ...recent], outbox: (await this.ctx.storage.list({ prefix: 'outbox:' })).size };
  }

  // ---- runs and metering --------------------------------------------------

  // The refusal for a new submission to agent, or null. Checked before the
  // runtime admits it, so nothing over budget is queued.
  admit(agent: string): string | null {
    const day = utcDay(Date.now());
    const sum = (where: string, ...args: SqlStorageValue[]) =>
      Number(this.sql.exec(`SELECT COALESCE(SUM(cost_usd), 0) AS usd FROM usage_day WHERE ${where}`, ...args).one().usd);
    const reason = admission({
      ownerDay: sum('day = ?', day), ownerMonth: sum('day >= ?', `${day.slice(0, 7)}-01`), agentDay: sum('day = ? AND agent = ?', day, agent),
    });
    if (reason) this.meter(`limit:${agent}:${Date.now()}`, agent, 'limit', { detail: reason });
    return reason;
  }

  // A run's start and its report may arrive in either order: neither regresses
  // the other. A start never touches an existing row, and a report creates
  // the row when the start was lost.
  runStarted(agent: string, op: string, source: string | null, startedAt = now()) {
    this.sql.exec(
      `INSERT INTO runs (agent, op, source, started_at, status) VALUES (?, ?, ?, ?, 'working')
       ON CONFLICT (agent, op) DO NOTHING`, agent, op, source, startedAt);
    const run = this.run(agent, op);
    if (run) this.ui({ type: 'run', agent, run });
    return this.refresh(agent, op);
  }

  // A pending start the agent could not admit, so the run never ran.
  runWithdrawn(agent: string, op: string) {
    this.sql.exec('DELETE FROM runs WHERE agent = ? AND op = ? AND settled_at IS NULL', agent, op);
    this.sql.exec("UPDATE requests SET state = 'not_delivered' WHERE id = ? AND agent = ? AND state = 'submitting'", op, agent);
    return this.refresh(agent);
  }

  async report(report: GatewayReport) {
    const id = `outbox:${report.agent}:${report.operationId}`;
    const { source, startedAt, via, ...settled } = report;
    const stored = { ...settled, text: report.text?.slice(0, REPORT_TEXT_LIMIT) };
    const usage = report.usage;
    const cost = usage ? modelCost(usage) : 0;
    const tokensIn = usage ? usage.input + usage.cacheRead : 0;
    const at = now();
    // The run, its metering and a remote ask's settlement commit together,
    // before the report is acknowledged; a replayed report repairs a part
    // that did not.
    const remote = this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        `INSERT INTO runs (agent, op, source, started_at, settled_at, status, reason, tokens_in, tokens_out, cost_usd, answer)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT (agent, op) DO UPDATE SET settled_at = excluded.settled_at, status = excluded.status,
           reason = excluded.reason, tokens_in = excluded.tokens_in, tokens_out = excluded.tokens_out,
           cost_usd = excluded.cost_usd, answer = excluded.answer
         WHERE runs.settled_at IS NULL`,
        report.agent, report.operationId, source ?? null, startedAt ?? at, at, report.status, report.reason ?? null,
        tokensIn, usage?.output ?? 0, cost, stored.text ?? null);
      if (usage) this.meter(`run:${report.agent}:${report.operationId}`, report.agent, 'model', { tokensIn, tokensOut: usage.output, cost });
      return this.settleRemoteRun(report.agent, report.operationId, report.status, report.reason ?? null, stored.text ?? null);
    });
    if (remote) this.pushSettled(remote);
    // The asker's run completed: a reply it is owed may be deliverable now.
    const owner = await this.owner();
    if (owner) this.ctx.waitUntil(followUp(this.env as MeshEnv, owner));
    // The relay's runs were never queued by the daemon; their answer is in the
    // run log, so the outbox does not carry them.
    if ((via ?? 'daemon') === 'daemon') {
      await this.ctx.storage.put(id, stored);
      this.daemon()?.send(JSON.stringify({ type: 'settled', id, ...stored }));
    }
    const run = this.run(report.agent, report.operationId);
    if (run) this.ui({ type: 'run', agent: report.agent, run });
    this.refresh(report.agent, report.operationId, report.reason ?? null);
    await this.publishOutbox();
  }

  // ---- remote (rfcs/managed-in-the-mesh.md) ---------------------------------
  // This owner's side of the cross-machine protocol when managed is the
  // target. Every multi-statement change is one synchronous transaction, and
  // every terminal write is a compare-and-set, so a racing settle, read or
  // replayed report changes nothing once a result is recorded.

  storeEpoch(): string {
    let epoch = this.ctx.storage.kv.get<string>('store_epoch');
    if (!epoch) {
      epoch = crypto.randomUUID();
      this.ctx.storage.kv.put('store_epoch', epoch);
    }
    return epoch;
  }

  // Records an inbound request before dispatch. A fence for the id answers
  // not_delivered; a known id answers its record; a different envelope is a
  // conflict.
  createInbound(record: RemoteEnvelope): { conflict: true } | { record: RemoteInbound; created: boolean } {
    return this.ctx.storage.transactionSync(() => {
      const fence = this.sql.exec('SELECT 1 FROM remote_fences WHERE id = ? AND from_machine = ?', record.id, record.from_machine).toArray()[0];
      if (fence) return { record: { ...record, state: 'settled', close_reason: 'not_delivered', reply: null }, created: false };
      const known = this.inbound(record.id);
      if (known) {
        const same = known.kind === record.kind && known.from_machine === record.from_machine && known.from_peer === record.from_peer
          && known.from_name === record.from_name && known.from_circle === record.from_circle && known.to_agent === record.to_agent && known.text === record.text;
        return same ? { record: known, created: false } : { conflict: true as const };
      }
      this.sql.exec(
        `INSERT INTO remote_inbound (id, kind, from_machine, from_peer, from_name, from_circle, to_agent, text, created_at, state)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'dispatching')`,
        record.id, record.kind, record.from_machine, record.from_peer, record.from_name, record.from_circle, record.to_agent, record.text, record.created_at);
      return { record: this.inbound(record.id)!, created: true };
    });
  }

  // The recorded origin reading its request. An unknown id inside the create
  // horizon is fenced, so a late create can no longer dispatch it; one past
  // the horizon may have existed and been pruned, so it is expired.
  readInbound(id: string, fromMachine: string, fromPeer: string, createdAt: number, horizon: number): RemoteInbound | 'not_found' | 'expired' {
    return this.ctx.storage.transactionSync(() => {
      const known = this.inbound(id);
      if (known) return known.from_machine === fromMachine && known.from_peer === fromPeer ? known : 'not_found';
      const fence = this.sql.exec('SELECT from_peer FROM remote_fences WHERE id = ? AND from_machine = ?', id, fromMachine).toArray()[0];
      if (!fence) {
        if (createdAt < horizon) return 'expired';
        this.sql.exec('INSERT INTO remote_fences (id, from_machine, from_peer, created_at) VALUES (?, ?, ?, ?)', id, fromMachine, fromPeer, createdAt);
      } else if (fence.from_peer !== fromPeer) return 'not_found';
      return { id, kind: id.startsWith('notif-') ? 'notify' : 'ask', from_machine: fromMachine, from_peer: fromPeer, from_name: '', from_circle: '',
        to_agent: '', text: '', created_at: createdAt, state: 'settled', close_reason: 'not_delivered', reply: null };
    });
  }

  inboundRecord(id: string) { return this.inbound(id) ?? null; }

  advanceInbound(id: string, from: string, to: string) {
    return this.sql.exec('UPDATE remote_inbound SET state = ? WHERE id = ? AND state = ?', to, id, from).rowsWritten > 0;
  }

  // Settles a request once. The first terminal write wins.
  settleInbound(id: string, reason: string, reply: string | null): RemoteInbound | null {
    const won = this.ctx.storage.transactionSync(() => this.settleRow(id, reason, reply));
    if (won) this.pushSettled(won);
    return won;
  }

  // Requests caught mid-dispatch, for lazy recovery.
  dispatchingInbound(limit: number, before: number) {
    return this.sql.exec("SELECT * FROM remote_inbound WHERE state = 'dispatching' AND created_at < ? ORDER BY created_at LIMIT ?", before, limit).toArray() as unknown as RemoteInbound[];
  }

  // ---- outbound: managed agents as askers. The row is authoritative; its
  // envelope is immutable and every transition a compare-and-set.

  insertOutbound(row: RemoteOutboundInit) {
    this.sql.exec(
      `INSERT INTO remote_outbound (id, machine, store_epoch, to_peer, to_name, from_agent, text, created_at, state)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'sent')`,
      row.id, row.machine, row.store_epoch, row.to_peer, row.to_name, row.from_agent, row.text, row.created_at);
  }

  outboundRecord(id: string) {
    return (this.sql.exec('SELECT * FROM remote_outbound WHERE id = ?', id).toArray()[0] as unknown as RemoteOutbound | undefined) ?? null;
  }

  markOutboundUncertain(id: string) {
    return this.sql.exec("UPDATE remote_outbound SET state = 'uncertain' WHERE id = ? AND state = 'sent'", id).rowsWritten > 0;
  }

  // One terminal transition for a push, a read and a create response alike.
  settleOutbound(id: string, reason: string, reply: string | null) {
    return this.sql.exec("UPDATE remote_outbound SET state = 'settled', close_reason = ?, reply = ? WHERE id = ? AND state IN ('sent', 'uncertain')",
      reason, reply, id).rowsWritten > 0;
  }

  markOutboundDelivered(id: string, disposition: string | null = null) {
    this.sql.exec('UPDATE remote_outbound SET delivered = 1, disposition = ? WHERE id = ? AND delivered = 0', disposition, id);
  }

  // Unsettled rows not read in the last cutoff, oldest check first; each is
  // stamped as it is claimed so every row gets its turn.
  claimOutboundChecks(checkedBefore: number, now: number, limit: number) {
    return this.ctx.storage.transactionSync(() => {
      const rows = this.sql.exec("SELECT * FROM remote_outbound WHERE state != 'settled' AND checked_at < ? ORDER BY checked_at, created_at LIMIT ?",
        checkedBefore, limit).toArray() as unknown as RemoteOutbound[];
      for (const row of rows) this.sql.exec('UPDATE remote_outbound SET checked_at = ? WHERE id = ?', now, row.id);
      return rows;
    });
  }

  undeliveredOutbound(limit: number) {
    return this.sql.exec("SELECT * FROM remote_outbound WHERE state = 'settled' AND delivered = 0 ORDER BY created_at LIMIT ?", limit)
      .toArray() as unknown as RemoteOutbound[];
  }

  // An agent's open cross-machine asks, for /managed.
  openOutbound(agent: string) {
    return this.sql.exec("SELECT * FROM remote_outbound WHERE from_agent = ? AND (state != 'settled' OR delivered = 0) ORDER BY created_at", agent)
      .toArray() as unknown as RemoteOutbound[];
  }

  pruneRemote(before: number) {
    this.ctx.storage.transactionSync(() => {
      this.sql.exec('DELETE FROM remote_outbound WHERE created_at < ?', before);
      this.sql.exec('DELETE FROM remote_inbound WHERE created_at < ?', before);
      this.sql.exec('DELETE FROM remote_fences WHERE created_at < ?', before);
    });
  }

  private inbound(id: string) {
    return this.sql.exec('SELECT * FROM remote_inbound WHERE id = ?', id).toArray()[0] as unknown as RemoteInbound | undefined;
  }

  private settleRow(id: string, reason: string, reply: string | null) {
    const changed = this.sql.exec("UPDATE remote_inbound SET state = 'settled', close_reason = ?, reply = ?, settled_at = ? WHERE id = ? AND state != 'settled'",
      reason, reply, Math.floor(Date.now() / 1000), id).rowsWritten;
    return changed ? this.inbound(id)! : null;
  }

  // A run that answers a remote ask settles it, keyed by agent and operation.
  // Inside the report's transaction.
  private settleRemoteRun(agent: string, op: string, status: string, reason: string | null, text: string | null) {
    const row = this.sql.exec("SELECT kind FROM remote_inbound WHERE id = ? AND to_agent = ? AND state IN ('dispatching', 'open')", op, agent).toArray()[0];
    if (!row || row.kind !== 'ask') return null;
    const [closeReason, reply] = remoteResult(status, reason, text);
    return this.settleRow(op, closeReason, reply);
  }

  // One settlement push to the source, after the commit and off the caller's
  // path. A lost push is recovered by the source reading the record back.
  private pushSettled(row: RemoteInbound) {
    if (row.kind !== 'ask') return;
    // Managed to managed: the source's row is in this same Gateway.
    if (row.from_machine === MANAGED) {
      this.settleOutbound(row.id, row.close_reason ?? 'unanswered', row.reply);
      void this.owner().then((owner) => { if (owner) this.ctx.waitUntil(followUp(this.env as MeshEnv, owner)); });
      return;
    }
    const env = this.env as { RELAY_URL?: string; CONTROL_TOKEN?: string };
    if (!env.RELAY_URL || !env.CONTROL_TOKEN) return;
    const send = async () => {
      const owner = await this.owner();
      if (!owner) return;
      const response = await fetch(`${env.RELAY_URL!.replace(/\/$/, '')}/api/v1/managed/machine-request`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${env.CONTROL_TOKEN}`, 'X-Repowire-Owner': owner, 'Content-Type': 'application/json' },
        body: JSON.stringify({ machine: row.from_machine, method: 'POST', path: '/remote/settled',
          body: { id: row.id, close_reason: row.close_reason, ...(row.reply != null ? { reply: row.reply } : {}), store_epoch: this.storeEpoch() } }),
      });
      if (!response.ok) console.warn('Remote settlement push failed; the source reads it back', row.id, response.status);
    };
    this.ctx.waitUntil(send().catch((error) => console.warn('Remote settlement push failed; the source reads it back', row.id, error)));
  }

  execStarted(workspace: string) {
    this.sql.exec('UPDATE workspaces SET running = running + 1 WHERE id = ?', workspace);
    this.publishWorkspace(workspace);
  }

  execFinished(agent: string, workspace: string, callId: string, report: ExecReport) {
    const wallMs = Math.round(report.wall_ms);
    this.sql.exec(
      `UPDATE workspaces SET running = MAX(running - 1, 0), exec_count = exec_count + 1,
         container_ms = container_ms + ?, last_exec_at = ?, snapshot_at = COALESCE(?, snapshot_at),
         dirty = COALESCE(?, dirty) WHERE id = ?`,
      wallMs, now(), report.snapshot_at, report.dirty == null ? null : Number(report.dirty), workspace);
    this.meter(`exec:${agent}:${callId}`, agent, 'exec', { wallMs, cost: execCost(wallMs) });
    this.publishWorkspace(workspace);
  }

  // Records one usage event once; the daily rollup moves only when the event
  // is new, so a resent report never double counts.
  private meter(id: string, agent: string, kind: string, value: { tokensIn?: number; tokensOut?: number; wallMs?: number; cost?: number; detail?: string }) {
    const at = now();
    const day = at.slice(0, 10);
    const wallMs = Math.round(value.wallMs ?? 0);
    const inserted = this.sql.exec(
      `INSERT OR IGNORE INTO usage_event (id, at, day, agent, kind, tokens_in, tokens_out, wall_ms, cost_usd, detail)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      id, at, day, agent, kind, value.tokensIn ?? 0, value.tokensOut ?? 0, wallMs, value.cost ?? 0, value.detail ?? null,
    ).rowsWritten;
    if (!inserted) return;
    this.sql.exec(
      `INSERT INTO usage_day (day, agent, kind, events, tokens_in, tokens_out, wall_ms, cost_usd) VALUES (?, ?, ?, 1, ?, ?, ?, ?)
       ON CONFLICT (day, agent, kind) DO UPDATE SET events = events + 1, tokens_in = tokens_in + excluded.tokens_in,
         tokens_out = tokens_out + excluded.tokens_out, wall_ms = wall_ms + excluded.wall_ms, cost_usd = cost_usd + excluded.cost_usd`,
      day, agent, kind, value.tokensIn ?? 0, value.tokensOut ?? 0, wallMs, value.cost ?? 0);
  }

  // ---- admin --------------------------------------------------------------

  async summary() {
    const day = utcDay(Date.now());
    const states: Record<AgentState, number> = { idle: 0, working: 0, parked: 0, failed: 0, retired: 0 };
    for (const row of this.sql.exec('SELECT state, COUNT(*) AS n FROM agents GROUP BY state').toArray()) {
      states[row.state as AgentState] = Number(row.n);
    }
    return {
      owner: await this.owner(), agents: Object.values(states).reduce((a, b) => a + b, 0), agents_by_state: states,
      workspaces: Number(this.sql.exec('SELECT COUNT(*) AS n FROM workspaces').one().n),
      running_containers: Number(this.sql.exec('SELECT COALESCE(SUM(running), 0) AS n FROM workspaces').one().n),
      connected_daemons: this.ctx.getWebSockets('daemon').length,
      last_active_at: this.sql.exec('SELECT MAX(state_at) AS at FROM agents').one().at,
      today: this.usageTotal('day = ?', day), month: this.usageTotal('day >= ?', `${day.slice(0, 7)}-01`),
    };
  }

  usage(days: number) {
    const from = utcDay(Date.now() - (Math.max(1, Math.min(days, 90)) - 1) * 86_400_000);
    const byDay = new Map<string, Usage & { day: string }>();
    for (const row of this.sql.exec('SELECT * FROM usage_day WHERE day >= ? ORDER BY day', from).toArray()) {
      const entry = byDay.get(row.day as string) ?? { day: row.day as string, ...emptyUsage() };
      addUsage(entry, row);
      byDay.set(entry.day, entry);
    }
    return [...byDay.values()];
  }

  // ---- mesh link ----------------------------------------------------------

  // A mesh tool call. The owner's rate limit applies to every sent message;
  // an agent on managed is served here, a daemon's agent by its daemon.
  async call(call: GatewayCall): Promise<string> {
    if (MESH_TOOLS.has(call.tool)) {
      const kept = meshWindow((await this.ctx.storage.get<number[]>('mesh:recent')) ?? [], Date.now());
      if (!kept) throw new Error('mesh_rate_limited: too many agent-originated messages from this account in 10 minutes');
      await this.ctx.storage.put('mesh:recent', kept);
    }
    const row = this.agent(call.agent);
    if (row && onManaged(row)) {
      const owner = await this.owner();
      if (!owner) throw new Error('This account is not set up for mesh tools');
      const env = this.env as MeshEnv;
      const principal = { peer_id: row.id, display_name: (row.display_name as string | null) || row.id, circle: String(row.circle ?? MANAGED) };
      try {
        return await managedTool(env, owner, principal, call.tool, call.args);
      } finally {
        this.ctx.waitUntil(followUp(env, owner));
      }
    }
    const socket = this.daemon();
    if (!socket) throw new Error('The Repowire daemon is not connected; mesh tools are unavailable right now');
    const key = `${call.agent}:${call.id}`;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => this.settle(key, new Error(`The daemon did not answer ${call.tool} within 60 seconds; it may still have run`)), CALL_TIMEOUT_MS);
      this.pending.set(key, {
        socket,
        resolve: (text) => { clearTimeout(timer); resolve(text); },
        reject: (error) => { clearTimeout(timer); reject(error); },
      });
      socket.send(JSON.stringify({ type: 'call', id: key, agent: call.agent, tool: call.tool, args: call.args }));
    });
  }

  async webSocketMessage(socket: WebSocket, message: string | ArrayBuffer) {
    if (typeof message !== 'string' || !this.ctx.getTags(socket).includes('daemon')) return;
    const frame = JSON.parse(message) as { type?: string; id?: string; text?: string; error?: boolean };
    if (typeof frame.id !== 'string') return;
    if (frame.type === 'ack' && frame.id.startsWith('outbox:')) {
      await this.ctx.storage.delete(frame.id);
      await this.publishOutbox();
      return;
    }
    if (frame.type !== 'result') return;
    this.settle(frame.id, frame.error ? new Error(frame.text || 'Mesh tool failed') : frame.text ?? '');
  }

  async webSocketClose(socket: WebSocket) {
    if (!this.ctx.getTags(socket).includes('daemon')) return;
    for (const [key, pending] of this.pending) {
      if (pending.socket === socket) this.settle(key, new Error('The daemon disconnected before answering; the call may have run'));
    }
    const info = socket.deserializeAttachment() as Client | null;
    if (!info?.connected) return;
    const gone: Client = { ...info, connected: false, disconnected_at: now() };
    socket.serializeAttachment(gone);
    const recent = (await this.ctx.storage.get<Client[]>('clients:recent')) ?? [];
    await this.ctx.storage.put('clients:recent', [gone, ...recent].slice(0, RECENT_CLIENTS));
    this.ui({ type: 'client', client: gone });
  }

  // ---- internals ----------------------------------------------------------

  private daemon() {
    return this.ctx.getWebSockets('daemon').filter((socket) => (socket.deserializeAttachment() as Client | null)?.connected !== false).at(-1);
  }

  private settle(key: string, outcome: string | Error) {
    const pending = this.pending.get(key);
    if (!pending) return;
    this.pending.delete(key);
    if (outcome instanceof Error) pending.reject(outcome);
    else pending.resolve(outcome);
  }

  // Recomputes an agent's state from its runs after any change.
  private refresh(id: string, op?: string, reason?: string | null) {
    const row = this.sql.exec('SELECT retired_at FROM agents WHERE id = ?', id).toArray()[0];
    if (!row) return null;
    const working = Number(this.sql.exec("SELECT COUNT(*) AS n FROM runs WHERE agent = ? AND status = 'working'", id).one().n);
    const last = this.sql.exec(
      'SELECT status, reason FROM runs WHERE agent = ? AND settled_at IS NOT NULL ORDER BY settled_at DESC LIMIT 1', id).toArray()[0];
    const state = deriveState(row.retired_at != null, working, last && { status: last.status as string, reason: last.reason as string | null });
    this.sql.exec('UPDATE agents SET state = ?, state_at = ?, last_op = COALESCE(?, last_op) WHERE id = ?', state, now(), op ?? null, id);
    if (reason !== undefined) this.sql.exec('UPDATE agents SET last_reason = ? WHERE id = ?', reason, id);
    return this.publish(id);
  }

  private publish(id: string) {
    const agent = this.agent(id);
    if (agent) this.ui({ type: 'agent', agent });
    return agent;
  }

  private publishWorkspace(id: string) {
    const workspace = this.workspaces().find((item) => item.id === id);
    if (workspace) this.ui({ type: 'workspace', workspace });
  }

  private async publishOutbox() {
    this.ui({ type: 'outbox', depth: (await this.ctx.storage.list({ prefix: 'outbox:' })).size });
  }

  private ui(frame: unknown) {
    const text = JSON.stringify(frame);
    for (const socket of this.ctx.getWebSockets('ui')) {
      try { socket.send(text); } catch { /* a closing socket drops the frame; its next connect gets a snapshot */ }
    }
  }

  private async snapshot() {
    const { items, outbox } = await this.clients();
    return { type: 'snapshot', agents: this.agents(), workspaces: this.workspaces(), clients: items, outbox, at: now() };
  }

  private agentRow(row: Row) {
    const totals = this.usageTotal('agent = ?', row.id as string);
    return {
      id: row.id as string, workspace: row.workspace, display_name: row.display_name, state: row.state, state_at: row.state_at,
      last_op: row.last_op, last_reason: row.last_reason, description: row.description, home_machine: row.home_machine, circle: row.circle,
      created_at: row.created_at, created_by: row.created_by, retired_at: row.retired_at,
      runs: Number(this.sql.exec('SELECT COUNT(*) AS n FROM runs WHERE agent = ?', row.id as string).one().n),
      tokens_in: totals.tokens_in, tokens_out: totals.tokens_out, cost_usd: totals.cost_usd,
    };
  }

  private usageTotal(where: string, ...args: SqlStorageValue[]) {
    const total = emptyUsage();
    for (const row of this.sql.exec(`SELECT * FROM usage_day WHERE ${where}`, ...args).toArray()) addUsage(total, row);
    return total;
  }
}

/** The global list of owners, for the admin view's fan-out. */
export class Registry extends DurableObject {
  async touch(owner: string) {
    if (!(await this.ctx.storage.get(`owner:${owner}`))) await this.ctx.storage.put(`owner:${owner}`, now());
  }

  async owners(): Promise<string[]> {
    return [...(await this.ctx.storage.list({ prefix: 'owner:' })).keys()].map((key) => key.slice('owner:'.length));
  }
}
