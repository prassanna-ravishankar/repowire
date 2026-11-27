import { DurableObject } from 'cloudflare:workers';
import { deriveState, meshWindow, utcDay, type AgentState } from './ledger';
import { execCost, modelCost, type TokenUsage } from './prices';
import { followUp, managedTool, type MeshEnv } from './mesh';
import { MANAGED } from './protocol';
import { DEFAULT_POLICY, mostConservative, SWEEP_INTERVAL_MS, sweep, type RetentionPolicy, type TableRetention } from './retention';
import type { ExecOutcome, SaveResult, Workspace } from './workspace';
import type { HostedAgent } from './index';
import { scopedName } from './http';
import { LEDGER_ADDED_COLUMNS, LEDGER_SCHEMA, OwnerLedger, REQUESTS_RETENTION, RUNS_RETENTION, USAGE_RETENTION, type RelayRequest } from './owner-ledger';
import { RECORDS_ADDED_COLUMNS, RECORDS_SCHEMA, REMOTE_RETENTION, RemoteRecords, type RemoteEnvelope, type RemoteInbound, type RemoteOutboundInit } from './records';
import { wholeAge } from './retention';

// A run's answer as the run log keeps it.
export const REPORT_TEXT_LIMIT = 64_000;
// Agents and workspaces one sweep deletes whole, each a call to its own object.
const PURGE_BATCH = 4;
// Leftover keys of the daemon's gateway link one sweep deletes.
const LEGACY_BATCH = 500;
// A purge claim older than this was cut off (an eviction mid-call) and lapses.
const PURGE_CLAIM_MS = 5 * 60_000;
const purgeClaimFloor = () => new Date(Date.now() - PURGE_CLAIM_MS).toISOString();
const MESH_TOOLS = new Set(['notify_peer', 'ask']);
// A reported save time, kept only when it is later than the one recorded, so
// reports arriving out of order never move it back. Takes the time three times.
const LATER_SAVE = 'CASE WHEN ? IS NOT NULL AND (snapshot_at IS NULL OR ? > snapshot_at) THEN ? ELSE snapshot_at END';

export interface GatewayCall { id: string; agent: string; tool: string; args: Record<string, unknown> }

export interface GatewayReport {
  agent: string; operationId: string; status: string; reason?: string; text?: string; usage?: TokenUsage;
  // The run's admission, so a report that arrives first can create its row.
  source?: string | null; startedAt?: string;
}
export type { RelayRequest } from './owner-ledger';
// A finished command as the agent reports it: the workspace's own outcome, or
// when the workspace could not answer, the agent's elapsed time and no state.
export type ExecReport = Pick<ExecOutcome, 'wall_ms' | 'snapshot_at'> & { dirty: boolean | null };
export interface AgentInit { id: string; workspace: string | null; display_name?: string | null; created_by: string; circle?: string }
export type { RemoteEnvelope, RemoteInbound, RemoteOutbound, RemoteOutboundInit } from './records';

const now = () => new Date().toISOString();

const SCHEMA = `
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, workspace TEXT, display_name TEXT, description TEXT, home_machine TEXT, circle TEXT NOT NULL DEFAULT 'managed', asks_rev INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, created_by TEXT NOT NULL, retired_at TEXT,
  state TEXT NOT NULL DEFAULT 'idle', state_at TEXT NOT NULL, last_op TEXT, last_reason TEXT);
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY, exec_count INTEGER NOT NULL DEFAULT 0, container_ms INTEGER NOT NULL DEFAULT 0,
  last_exec_at TEXT, running INTEGER NOT NULL DEFAULT 0, snapshot_at TEXT, dirty INTEGER NOT NULL DEFAULT 0);
`;
// Columns added after the first deploy, for directories created before them.
const ADDED_COLUMNS = [
  'ALTER TABLE workspaces ADD COLUMN snapshot_at TEXT',
  'ALTER TABLE workspaces ADD COLUMN dirty INTEGER NOT NULL DEFAULT 0',
  "ALTER TABLE agents ADD COLUMN circle TEXT NOT NULL DEFAULT 'managed'",
  'ALTER TABLE agents ADD COLUMN asks_rev INTEGER NOT NULL DEFAULT 0',
  'ALTER TABLE agents ADD COLUMN purged_at TEXT',
  'ALTER TABLE workspaces ADD COLUMN purged_at TEXT',
  'ALTER TABLE workspaces ADD COLUMN purging_at TEXT',
];

type Row = Record<string, SqlStorageValue>;

/**
 * One owner's Durable Object. It holds the owner's directory (agents and
 * workspaces, with the state each pushes here), their ledger of runs, usage
 * and relay requests (src/owner-ledger.ts), and their cross-machine records
 * (src/records.ts), and it runs what spans them: a report that settles a run
 * and its remote ask together, agents' mesh tools, and retention. The relay
 * watches the owner over a hibernating
 * WebSocket (tag `ui`) that gets a snapshot and then every change. Agents and
 * workspaces push their state here, so listing never wakes an agent.
 * Protocol pings are answered by the runtime, so an idle watch never wakes
 * this object.
 */
export class Gateway extends DurableObject {
  private sql = this.ctx.storage.sql;

  private records: RemoteRecords;
  private ledger = new OwnerLedger(this.sql);

  constructor(ctx: DurableObjectState, env: unknown) {
    super(ctx, env as never);
    this.sql.exec(SCHEMA);
    this.sql.exec(LEDGER_SCHEMA);
    this.sql.exec(RECORDS_SCHEMA);
    for (const statement of [...ADDED_COLUMNS, ...LEDGER_ADDED_COLUMNS, ...RECORDS_ADDED_COLUMNS]) {
      try { this.sql.exec(statement); } catch (error) {
        if (!/duplicate column/i.test(String(error))) throw error;
      }
    }
    this.records = new RemoteRecords(this.sql, ctx.storage, {
      env, owner: () => this.owner(), changed: (agent) => { this.publish(agent); }, waitUntil: (work) => ctx.waitUntil(work),
    });
  }

  async fetch(request: Request): Promise<Response> {
    if (request.headers.get('Upgrade') !== 'websocket') return new Response('Expected WebSocket', { status: 426 });
    if (new URL(request.url).pathname !== '/events') return new Response('Not found', { status: 404 });
    const [client, server] = Object.values(new WebSocketPair());
    this.ctx.acceptWebSocket(server, ['ui']);
    server.send(JSON.stringify(this.snapshot()));
    return new Response(null, { status: 101, webSocket: client });
  }

  // ---- directory ----------------------------------------------------------

  // An agent row is its identity: ownership is immutable, and a retired row is
  // the tombstone that keeps its id from moving. A workspace row is the live
  // state of its container.
  static readonly directoryRetention: TableRetention = {
    tables: ['agents', 'workspaces'], exempt: 'identity and live state, kept while the owner exists; a retired agent and an unused workspace are deleted whole by their own objects (purgeWhole)',
  };

  // The owner this object belongs to, recorded by the first caller that names it.
  async owner(name?: string): Promise<string | undefined> {
    if (name && !(await this.ctx.storage.get('owner'))) await this.ctx.storage.put('owner', name);
    return this.ctx.storage.get<string>('owner');
  }

  addAgent(init: AgentInit) {
    // A workspace being deleted takes no new agent until the deletion ends:
    // its files are going, whatever the newcomer expects of them.
    if (init.workspace && this.sql.exec('SELECT 1 FROM workspaces WHERE id = ? AND purging_at > ?', init.workspace, purgeClaimFloor()).toArray().length) {
      return { conflict: `Workspace ${init.workspace} is being deleted; try again shortly` };
    }
    const at = now();
    this.sql.exec(
      `INSERT INTO agents (id, workspace, display_name, circle, created_at, created_by, state, state_at)
       VALUES (?, ?, ?, ?, ?, ?, 'idle', ?)
       ON CONFLICT (id) DO UPDATE SET display_name = COALESCE(excluded.display_name, agents.display_name)`,
      init.id, init.workspace, init.display_name ?? null, init.circle ?? 'managed', at, init.created_by, at);
    // A purged workspace taken up again starts empty.
    if (init.workspace) this.sql.exec('INSERT INTO workspaces (id) VALUES (?) ON CONFLICT (id) DO UPDATE SET purged_at = NULL', init.workspace);
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

  // ---- the owner's ledger (src/owner-ledger.ts) ----------------------------

  runs(agent: string) { return this.ledger.runs(agent); }
  run(agent: string, op: string, answer = false) { return this.ledger.run(agent, op, answer); }
  request(id: string): RelayRequest | null { return this.ledger.request(id); }
  openRequest(init: Omit<RelayRequest, 'state' | 'created_at' | 'settled_at'>) { return this.ledger.openRequest(init); }
  acceptRequest(id: string) { this.ledger.acceptRequest(id); }
  requestUndelivered(id: string) { this.ledger.requestUndelivered(id); }
  dropRequest(id: string) { this.ledger.dropRequest(id); }
  openRequests(requester: string) { return this.ledger.openRequests(requester); }
  admit(agent: string): string | null { return this.ledger.admit(agent); }
  usage(days: number) { return this.ledger.usage(days); }

  workspaces() {
    const users = new Map<string, string[]>();
    for (const row of this.sql.exec('SELECT id, workspace FROM agents WHERE workspace IS NOT NULL').toArray()) {
      const workspace = row.workspace as string;
      users.set(workspace, [...(users.get(workspace) ?? []), row.id as string]);
    }
    return this.sql.exec('SELECT * FROM workspaces ORDER BY id').toArray().map((row) => ({
      id: row.id as string, agents: users.get(row.id as string) ?? [], exec_count: Number(row.exec_count),
      container_ms: Number(row.container_ms), last_exec_at: row.last_exec_at, snapshot_at: row.snapshot_at,
      // A failed save outranks activity: changes since the last save are at risk.
      state: row.purged_at ? 'purged' : row.dirty ? 'needs_repair' : row.running ? 'running' : 'idle',
    }));
  }

  runStarted(agent: string, op: string, source: string | null, startedAt = now()) {
    this.ledger.startRun(agent, op, source, startedAt);
    const run = this.run(agent, op);
    if (run) this.ui({ type: 'run', agent, run });
    return this.refresh(agent, op);
  }

  runWithdrawn(agent: string, op: string) {
    this.ledger.withdrawRun(agent, op);
    return this.refresh(agent);
  }

  async report(report: GatewayReport) {
    const { source, startedAt, ...settled } = report;
    const stored = { ...settled, text: report.text?.slice(0, REPORT_TEXT_LIMIT) };
    const usage = report.usage;
    const cost = usage ? modelCost(usage) : 0;
    const tokensIn = usage ? usage.input + usage.cacheRead : 0;
    const at = now();
    // The run, its metering and a remote ask's settlement commit together,
    // before the report is acknowledged; a replayed report repairs a part
    // that did not.
    const remote = this.ctx.storage.transactionSync(() => {
      this.ledger.settleRun({ agent: report.agent, op: report.operationId, source: source ?? null, startedAt: startedAt ?? at, settledAt: at, status: report.status,
        reason: report.reason ?? null, text: stored.text ?? null, tokensIn, tokensOut: usage?.output ?? 0, cost, metered: Boolean(usage) });
      return this.records.settleRemoteRun(report.agent, report.operationId, report.status, report.reason ?? null, stored.text ?? null);
    });
    if (remote) this.records.pushSettled(remote);
    // The asker's run completed: a reply it is owed may be deliverable now.
    const owner = await this.owner();
    if (owner) this.ctx.waitUntil(followUp(this.env as MeshEnv, owner));
    const run = this.run(report.agent, report.operationId);
    if (run) this.ui({ type: 'run', agent: report.agent, run });
    this.refresh(report.agent, report.operationId, report.reason ?? null);
    this.housekeep();
  }

  // ---- cross-machine records (src/records.ts) -----------------------------

  storeEpoch() { return this.records.storeEpoch(); }
  createInbound(record: RemoteEnvelope) { return this.records.createInbound(record); }
  readInbound(id: string, fromMachine: string, fromPeer: string, createdAt: number, horizon: number) { return this.records.readInbound(id, fromMachine, fromPeer, createdAt, horizon); }
  inboundRecord(id: string) { return this.records.inboundRecord(id); }
  advanceInbound(id: string, from: string, to: string) { return this.records.advanceInbound(id, from, to); }
  settleInbound(id: string, reason: string, reply: string | null): RemoteInbound | null { return this.records.settleInbound(id, reason, reply); }
  dispatchingInbound(limit: number, before: number) { return this.records.dispatchingInbound(limit, before); }
  insertOutbound(row: RemoteOutboundInit) { return this.records.insertOutbound(row); }
  outboundRecord(id: string) { return this.records.outboundRecord(id); }
  markOutboundUncertain(id: string) { return this.records.markOutboundUncertain(id); }
  settleOutbound(id: string, reason: string, reply: string | null) { return this.records.settleOutbound(id, reason, reply); }
  markOutboundDelivered(id: string, disposition: string | null = null) { return this.records.markOutboundDelivered(id, disposition); }
  claimOutboundChecks(checkedBefore: number, now: number, limit: number) { return this.records.claimOutboundChecks(checkedBefore, now, limit); }
  claimDeliveries(triedBefore: number, now: number, limit: number) { return this.records.claimDeliveries(triedBefore, now, limit); }
  openOutbound(agent: string) { return this.records.openOutbound(agent); }

  // A command is about to run in workspace for agent. Refused while the
  // workspace is being deleted, and an agent the directory has not seen yet
  // (one from before it) is added first, under the same check. Once counted
  // as running, the workspace cannot be claimed for deletion.
  execStarted(workspace: string, agent?: string) {
    if (agent && !this.agent(agent)) {
      const added = this.addAgent({ id: agent, workspace, created_by: 'web' });
      if (added && 'conflict' in added) throw new Error(String(added.conflict));
    }
    if (this.sql.exec('SELECT 1 FROM workspaces WHERE id = ? AND purging_at > ?', workspace, purgeClaimFloor()).toArray().length) {
      throw new Error(`Workspace ${workspace} is being deleted; try again shortly`);
    }
    this.sql.exec('UPDATE workspaces SET running = running + 1 WHERE id = ?', workspace);
    this.publishWorkspace(workspace);
  }

  execFinished(agent: string, workspace: string, callId: string, report: ExecReport) {
    const wallMs = Math.round(report.wall_ms);
    this.sql.exec(
      `UPDATE workspaces SET running = MAX(running - 1, 0), exec_count = exec_count + 1,
         container_ms = container_ms + ?, last_exec_at = ?, snapshot_at = ${LATER_SAVE},
         dirty = COALESCE(?, dirty) WHERE id = ?`,
      wallMs, now(), report.snapshot_at, report.snapshot_at, report.snapshot_at, report.dirty == null ? null : Number(report.dirty), workspace);
    this.ledger.meter(`exec:${agent}:${callId}`, agent, 'exec', { wallMs, cost: execCost(wallMs) });
    this.publishWorkspace(workspace);
  }

  // How a turn-end save went: the saved time moves forward, and dirty says
  // whether changes since then are at risk.
  workspaceSaved(workspace: string, result: SaveResult) {
    this.sql.exec(`UPDATE workspaces SET snapshot_at = ${LATER_SAVE}, dirty = ? WHERE id = ?`,
      result.snapshot_at, result.snapshot_at, result.snapshot_at, Number(result.dirty), workspace);
    this.publishWorkspace(workspace);
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
      last_active_at: this.sql.exec('SELECT MAX(state_at) AS at FROM agents').one().at,
      today: this.usageTotal('day = ?', day), month: this.usageTotal('day >= ?', `${day.slice(0, 7)}-01`),
    };
  }

  // ---- mesh link ----------------------------------------------------------

  // A mesh tool call, run through the relay as the calling agent. The
  // owner's rate limit applies to every sent message.
  async call(call: GatewayCall): Promise<string> {
    if (MESH_TOOLS.has(call.tool)) {
      const kept = meshWindow((await this.ctx.storage.get<number[]>('mesh:recent')) ?? [], Date.now());
      if (!kept) throw new Error('mesh_rate_limited: too many agent-originated messages from this account in 10 minutes');
      await this.ctx.storage.put('mesh:recent', kept);
    }
    const row = this.agent(call.agent);
    if (!row) throw new Error(`Agent ${call.agent} is not in this directory`);
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

  // ---- internals ----------------------------------------------------------

  // Every table's retention, each declared beside its queries. Key-value
  // state is bounded or live and is not swept: the mesh window, the owner,
  // the store epoch and the retention keys themselves.
  static readonly retention: TableRetention[] = [
    Gateway.directoryRetention, REQUESTS_RETENTION, RUNS_RETENTION, USAGE_RETENTION, REMOTE_RETENTION,
  ];

  // The owner's policy (rfcs/managed-accounts.md: retention is per account),
  // or the shared defaults (daemon-go/proto/retention.json) until one is set.
  setRetention(policy: RetentionPolicy) { this.ctx.storage.kv.put('retention:policy', policy); }

  retentionPolicy(): RetentionPolicy {
    const kv = this.ctx.storage.kv;
    const own = kv.get<RetentionPolicy>('retention:policy');
    if (own) return own;
    // A Gateway from before kept one policy per connected daemon: the most
    // conservative of them becomes the owner's, once, so no data goes sooner
    // than a policy it was kept under allowed.
    const legacy = [...kv.list<RetentionPolicy>({ prefix: 'retention:policy:' })];
    if (!legacy.length) return DEFAULT_POLICY;
    const policy = mostConservative(legacy.map(([, value]) => value));
    this.ctx.storage.transactionSync(() => {
      kv.put('retention:policy', policy);
      for (const [key] of legacy) kv.delete(key);
    });
    return policy;
  }

  // Sweeps at most hourly, on the path of a request that found one due:
  // nothing here runs on a timer. Returns the rows each rule changed, or null
  // when no sweep was due.
  housekeep(nowMs = Date.now()) {
    const last = this.ctx.storage.kv.get<number>('retention:swept_at') ?? 0;
    if (nowMs - last < SWEEP_INTERVAL_MS) return null;
    // Stamped first: a sweep that throws waits for the next interval rather
    // than retrying on every request.
    this.ctx.storage.kv.put('retention:swept_at', nowMs);
    const policy = this.retentionPolicy();
    this.dropLegacy();
    const counts = this.ctx.storage.transactionSync(() => sweep(this.sql, Gateway.retention, policy, nowMs));
    if (Object.keys(counts).length) console.log('retention_swept', JSON.stringify(counts));
    this.ctx.waitUntil(this.purgeWhole(policy, nowMs).catch((error) => console.error('Hosted purge failed', error)));
    return counts;
  }

  // The daemon's report outbox and connection list, from before managed agents
  // were reached only through the relay. Nothing reads them, and an outbox
  // entry holds a run's answer, so a stale copy must not outlive retention: a
  // batch goes with each sweep until none is left.
  private dropLegacy() {
    const kv = this.ctx.storage.kv;
    kv.delete('clients:recent');
    for (const [key] of [...kv.list({ prefix: 'outbox:', limit: LEGACY_BATCH })]) kv.delete(key);
  }

  // Retired agents, and workspaces no live agent uses, past the whole-object
  // age (src/retention.ts) are deleted by their own Durable Objects, a few per
  // sweep. An agent goes only once it has no open run and no open
  // cross-machine record. The rows stay, marked purged_at, as identity: an
  // agent id never moves owner, and a workspace taken up again starts empty.
  // A workspace deletes its saved copy in R2 with it.
  async purgeWhole(policy = this.retentionPolicy(), nowMs = Date.now()) {
    const age = wholeAge(policy);
    const owner = await this.owner();
    if (age === 0 || !owner) return { agents: 0, workspaces: 0 };
    const cutoff = new Date(nowMs - age * 1000).toISOString();
    const env = this.env as { AGENTS: DurableObjectNamespace<HostedAgent>; WORKSPACES: DurableObjectNamespace<Workspace> };
    let agents = 0;
    let workspaces = 0;
    const retired = this.sql.exec(
      `SELECT id FROM agents a WHERE retired_at IS NOT NULL AND purged_at IS NULL AND retired_at < ?
         AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.agent = a.id AND r.settled_at IS NULL)
         AND NOT EXISTS (SELECT 1 FROM remote_inbound i WHERE i.to_agent = a.id AND i.state != 'settled')
         AND NOT EXISTS (SELECT 1 FROM remote_outbound o WHERE o.from_agent = a.id AND (o.state != 'settled' OR o.delivered = 0))
       ORDER BY retired_at LIMIT ?`, cutoff, PURGE_BATCH).toArray();
    for (const row of retired) {
      const id = row.id as string;
      if ((await env.AGENTS.getByName(scopedName(owner, id)).purge()).purged) {
        this.sql.exec('UPDATE agents SET purged_at = ? WHERE id = ? AND purged_at IS NULL', now(), id);
        agents++;
      }
    }
    const unused = this.sql.exec(
      `SELECT id FROM workspaces w WHERE purged_at IS NULL AND running = 0 AND COALESCE(last_exec_at, '') < ?
         AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.workspace = w.id AND a.purged_at IS NULL)
       ORDER BY id LIMIT ?`, cutoff, PURGE_BATCH).toArray();
    for (const row of unused) {
      if (await this.purgeWorkspace(env, owner, row.id as string, nowMs - age * 1000, 'a.purged_at IS NULL') === 'deleted') workspaces++;
    }
    if (agents || workspaces) console.log('retention_purged', JSON.stringify({ agents, workspaces }));
    return { agents, workspaces };
  }

  // An owner deletes a workspace on purpose: a sandbox is an owner-level
  // computer, not tied to one agent. It is kept while a live agent uses it or
  // its container runs; retired agents do not hold it.
  async deleteWorkspace(id: string): Promise<{ deleted: true } | { deleted: false; reason: 'not_found' | 'running' | 'in_use'; agents?: string[] }> {
    const owner = await this.owner();
    if (!owner || !this.sql.exec('SELECT 1 FROM workspaces WHERE id = ? AND purged_at IS NULL', id).toArray().length) return { deleted: false, reason: 'not_found' };
    const users = this.sql.exec('SELECT id FROM agents WHERE workspace = ? AND retired_at IS NULL ORDER BY id', id).toArray().map((row) => row.id as string);
    if (users.length) return { deleted: false, reason: 'in_use', agents: users };
    const env = this.env as { WORKSPACES: DurableObjectNamespace<Workspace> };
    const outcome = await this.purgeWorkspace(env, owner, id, Date.now(), 'a.retired_at IS NULL');
    return outcome === 'deleted' ? { deleted: true } : { deleted: false, reason: outcome === 'in_use' ? 'in_use' : 'running' };
  }

  // Deletes one workspace's files through its own object. It is claimed
  // while no agent matching `holds` uses it, before the call: from then on no
  // agent can take it up (addAgent refuses) until the claim ends. The
  // workspace itself refuses while its container runs, or if a command ran
  // there since `idleSinceMs`. It is marked purged only if still unused: a
  // claim that lapsed mid-call may have let an agent take it up again.
  private async purgeWorkspace(env: { WORKSPACES: DurableObjectNamespace<Workspace> }, owner: string, id: string, idleSinceMs: number, holds: string) {
    const unused = `id = ? AND purged_at IS NULL AND NOT EXISTS (SELECT 1 FROM agents a WHERE a.workspace = workspaces.id AND ${holds})`;
    // A command the directory counts as running keeps it, whoever started it.
    if (this.sql.exec('SELECT 1 FROM workspaces WHERE id = ? AND running > 0', id).toArray().length) return 'refused';
    const claimed = this.sql.exec(`UPDATE workspaces SET purging_at = ? WHERE ${unused} AND running = 0 AND (purging_at IS NULL OR purging_at <= ?)`,
      now(), id, purgeClaimFloor()).rowsWritten;
    if (!claimed) return 'in_use';
    let done = false;
    try {
      done = await env.WORKSPACES.getByName(scopedName(owner, id)).purge(idleSinceMs);
    } finally {
      this.sql.exec('UPDATE workspaces SET purging_at = NULL WHERE id = ?', id);
    }
    if (!done) return 'refused';
    if (!this.sql.exec(`UPDATE workspaces SET purged_at = ? WHERE ${unused}`, now(), id).rowsWritten) return 'in_use';
    this.publishWorkspace(id);
    return 'deleted';
  }

  // Recomputes an agent's state from its runs after any change.
  private refresh(id: string, op?: string, reason?: string | null) {
    const row = this.sql.exec('SELECT retired_at FROM agents WHERE id = ?', id).toArray()[0];
    if (!row) return null;
    const { working, last } = this.ledger.runState(id);
    const state = deriveState(row.retired_at != null, working, last);
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


  private ui(frame: unknown) {
    const text = JSON.stringify(frame);
    for (const socket of this.ctx.getWebSockets('ui')) {
      try { socket.send(text); } catch { /* a closing socket drops the frame; its next connect gets a snapshot */ }
    }
  }

  private snapshot() {
    return { type: 'snapshot', agents: this.agents(), workspaces: this.workspaces(), at: now() };
  }

  private agentRow(row: Row) {
    const totals = this.usageTotal('agent = ?', row.id as string);
    return {
      id: row.id as string, workspace: row.workspace, display_name: row.display_name, state: row.state, state_at: row.state_at,
      last_op: row.last_op, last_reason: row.last_reason, description: row.description, circle: row.circle, asks_rev: Number(row.asks_rev ?? 0),
      created_at: row.created_at, retired_at: row.retired_at, purged_at: row.purged_at ?? null,
      runs: this.ledger.runCount(row.id as string),
      tokens_in: totals.tokens_in, tokens_out: totals.tokens_out, cost_usd: totals.cost_usd,
    };
  }

  private usageTotal(where: string, ...args: SqlStorageValue[]) { return this.ledger.usageTotal(where, ...args); }
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
