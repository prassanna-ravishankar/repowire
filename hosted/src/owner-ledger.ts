// The owner's ledger (rfcs/managed-accounts.md, rfcs/retention.md "Hosted"):
// every run of every agent, the usage it was billed for, and the relay's
// requests to agents. They are the owner's, not an agent's: spend caps sum
// across agents, a requester's open asks span agents, and a run's report
// settles a cross-machine ask in the same transaction (src/records.ts).
import { admission, utcDay } from './ledger';
import type { TableRetention } from './retention';

const now = () => new Date().toISOString();

export interface RelayRequest {
  id: string; agent: string; kind: 'ask' | 'notify'; requester: string; text: string;
  created_at: string; state: 'submitting' | 'accepted' | 'not_delivered';
  // SHA-256 of the text as submitted, so a retry still matches its request
  // once retention has stripped the text. Null for requests made before it.
  digest: string | null;
  // When a request that never ran was settled as not delivered.
  settled_at: string | null;
}

export const LEDGER_SCHEMA = `
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
`;
// Columns added after the first deploy, for directories created before them.
export const LEDGER_ADDED_COLUMNS = [
  'ALTER TABLE runs ADD COLUMN answer TEXT',
  'ALTER TABLE requests ADD COLUMN digest TEXT',
  'ALTER TABLE requests ADD COLUMN settled_at TEXT',
];

type Row = Record<string, SqlStorageValue>;
// A run as listed and pushed; its answer is fetched one run at a time.
export const RUN = 'agent, op, source, started_at, settled_at, status, reason, tokens_in, tokens_out, cost_usd';

export type Usage = ReturnType<typeof emptyUsage>;

export const emptyUsage = () => ({ runs: 0, tokens_in: 0, tokens_out: 0, model_usd: 0, execs: 0, container_ms: 0, container_usd: 0, cost_usd: 0 });

export function addUsage(total: Usage, row: Row) {
  const cost = Number(row.cost_usd);
  if (row.kind === 'model') {
    total.runs += Number(row.events); total.tokens_in += Number(row.tokens_in); total.tokens_out += Number(row.tokens_out); total.model_usd += cost;
  } else if (row.kind === 'exec') {
    total.execs += Number(row.events); total.container_ms += Number(row.wall_ms); total.container_usd += cost;
  }
  total.cost_usd += cost;
}


// A request is a record; its text is content. One whose run is still open
// stays whole, since its reader still waits on it.
export const REQUESTS_RETENTION: TableRetention = {
  tables: ['requests'],
  rules: (c) => {
    // Only on proof that it ended, timed from when it ended: its run settled,
    // or it was settled as never delivered. A request with no run yet is
    // still pending (its reader reconciles it), and its record outlives
    // neither its run nor its reader's wait.
    const ended = `(EXISTS (SELECT 1 FROM runs r WHERE r.agent = requests.agent AND r.op = requests.id AND r.settled_at IS NOT NULL AND r.settled_at < ?)
      OR (state = 'not_delivered' AND COALESCE(settled_at, created_at) < ?))`;
    return {
      records: c.records ? [{ name: 'requests.delete', table: 'requests', where: ended, args: [c.records.iso, c.records.iso] }] : [],
      // Text goes only where a digest still identifies the request for a retry.
      content: c.content ? [{ name: 'requests.strip', table: 'requests', set: "text = ''", where: `text != '' AND digest IS NOT NULL AND ${ended}`,
        args: [c.content.iso, c.content.iso] }] : [],
    };
  },
};

// A settled run is a record and its answer is content; an open run stays.
// The usage ledger is what an owner is billed from, so it is kept forever
// (rfcs/managed-accounts.md).
export const RUNS_RETENTION: TableRetention = {
  tables: ['runs'],
  rules: (c) => ({
    records: c.records ? [{ name: 'runs.delete', table: 'runs', where: 'settled_at IS NOT NULL AND settled_at < ?', args: [c.records.iso] }] : [],
    content: c.content ? [{ name: 'runs.strip', table: 'runs', set: 'answer = NULL', where: 'answer IS NOT NULL AND settled_at IS NOT NULL AND settled_at < ?', args: [c.content.iso] }] : [],
  }),
};
export const USAGE_RETENTION: TableRetention = { tables: ['usage_event', 'usage_day'], exempt: 'billing ledger, kept forever' };

// A settled run as an agent reports it, with its model usage priced.
export interface SettledRun {
  agent: string; op: string; source: string | null; startedAt: string; settledAt: string; status: string; reason: string | null;
  text: string | null; tokensIn: number; tokensOut: number; cost: number;
  // Whether the run reported model usage; a run that did is metered, even at zero.
  metered: boolean;
}

export class OwnerLedger {
  constructor(private sql: SqlStorage) {}

  runs(agent: string) {
    return this.sql.exec(`SELECT ${RUN} FROM runs WHERE agent = ? ORDER BY started_at DESC LIMIT 100`, agent).toArray();
  }

  run(agent: string, op: string, answer = false) {
    return this.sql.exec(`SELECT ${RUN}${answer ? ', answer' : ''} FROM runs WHERE agent = ? AND op = ?`, agent, op).toArray()[0] ?? null;
  }

  // How many of an agent's runs are open, and how its last settled run ended.
  runState(agent: string) {
    const working = Number(this.sql.exec("SELECT COUNT(*) AS n FROM runs WHERE agent = ? AND status = 'working'", agent).one().n);
    const last = this.sql.exec(
      'SELECT status, reason FROM runs WHERE agent = ? AND settled_at IS NOT NULL ORDER BY settled_at DESC LIMIT 1', agent).toArray()[0];
    return { working, last: last && { status: last.status as string, reason: last.reason as string | null } };
  }

  runCount(agent: string) { return Number(this.sql.exec('SELECT COUNT(*) AS n FROM runs WHERE agent = ?', agent).one().n); }

  // A run's start and its report may arrive in either order: neither regresses
  // the other. A start never touches an existing row, and a report creates
  // the row when the start was lost.
  startRun(agent: string, op: string, source: string | null, startedAt: string) {
    this.sql.exec(
      `INSERT INTO runs (agent, op, source, started_at, status) VALUES (?, ?, ?, ?, 'working')
       ON CONFLICT (agent, op) DO NOTHING`, agent, op, source, startedAt);
  }

  // A pending start the agent could not admit, so the run never ran.
  withdrawRun(agent: string, op: string) {
    this.sql.exec('DELETE FROM runs WHERE agent = ? AND op = ? AND settled_at IS NULL', agent, op);
    this.sql.exec("UPDATE requests SET state = 'not_delivered', settled_at = ? WHERE id = ? AND agent = ? AND state = 'submitting'", now(), op, agent);
  }

  // Records a settled run and meters its model usage, once. Called inside the
  // report's transaction.
  settleRun(run: SettledRun) {
    this.sql.exec(
      `INSERT INTO runs (agent, op, source, started_at, settled_at, status, reason, tokens_in, tokens_out, cost_usd, answer)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT (agent, op) DO UPDATE SET settled_at = excluded.settled_at, status = excluded.status,
         reason = excluded.reason, tokens_in = excluded.tokens_in, tokens_out = excluded.tokens_out,
         cost_usd = excluded.cost_usd, answer = excluded.answer
       WHERE runs.settled_at IS NULL`,
      run.agent, run.op, run.source, run.startedAt, run.settledAt, run.status, run.reason, run.tokensIn, run.tokensOut, run.cost, run.text);
    if (run.metered) {
      this.meter(`run:${run.agent}:${run.op}`, run.agent, 'model', { tokensIn: run.tokensIn, tokensOut: run.tokensOut, cost: run.cost });
    }
  }

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

  // Records one usage event once; the daily rollup moves only when the event
  // is new, so a resent report never double counts.
  meter(id: string, agent: string, kind: string, value: { tokensIn?: number; tokensOut?: number; wallMs?: number; cost?: number; detail?: string }) {
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

  usageTotal(where: string, ...args: SqlStorageValue[]) {
    const total = emptyUsage();
    for (const row of this.sql.exec(`SELECT * FROM usage_day WHERE ${where}`, ...args).toArray()) addUsage(total, row);
    return total;
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

  // ---- the relay's requests. A request's id is its run's operation id, so
  // its result is that run.

  request(id: string): RelayRequest | null {
    const row = this.sql.exec('SELECT * FROM requests WHERE id = ?', id).toArray()[0];
    return row ? (row as unknown as RelayRequest) : null;
  }

  // Records a request before it is submitted. Returns the existing one for a
  // known id, so a retry never resubmits.
  openRequest(init: Omit<RelayRequest, 'state' | 'created_at' | 'settled_at'>): { request: RelayRequest; created: boolean } {
    const created = this.sql.exec(
      `INSERT OR IGNORE INTO requests (id, agent, kind, requester, text, digest, created_at, state) VALUES (?, ?, ?, ?, ?, ?, ?, 'submitting')`,
      init.id, init.agent, init.kind, init.requester, init.text, init.digest, now()).rowsWritten > 0;
    return { request: this.request(init.id)!, created };
  }

  acceptRequest(id: string) { this.sql.exec("UPDATE requests SET state = 'accepted' WHERE id = ? AND state = 'submitting'", id); }

  // The agent confirmed it never took the request and will refuse it.
  requestUndelivered(id: string) { this.sql.exec("UPDATE requests SET state = 'not_delivered', settled_at = ? WHERE id = ? AND state = 'submitting'", now(), id); }

  // A request the agent refused as retired before recording it: forgotten.
  dropRequest(id: string) { this.sql.exec("DELETE FROM requests WHERE id = ? AND state = 'submitting'", id); }

  // A requester's asks that have not settled, newest last.
  openRequests(requester: string) {
    return this.sql.exec(
      `SELECT q.id, q.agent, q.text, q.created_at FROM requests q LEFT JOIN runs r ON r.agent = q.agent AND r.op = q.id
       WHERE q.requester = ? AND q.kind = 'ask' AND q.state != 'not_delivered' AND r.settled_at IS NULL
       ORDER BY q.created_at LIMIT 100`, requester).toArray();
  }
}
