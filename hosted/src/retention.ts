// Retention for an owner's Gateway (rfcs/retention.md, "Hosted"). The same
// policy as the daemon's: two classes, content (text someone wrote) and
// records (who did what, when, how it ended), never anything younger than
// the ask lifetime, and a fixed window for cross-machine records. The
// defaults are the daemon's own file; a connected daemon names its policy on
// every connect. The Gateway keeps each daemon's policy and sweeps by the most
// conservative of them (mostConservative).
import defaults from '../../daemon-go/proto/retention.json';

const DAY_S = 24 * 3600;

// Seconds; 0 keeps a class forever.
export interface RetentionPolicy { content_s: number; records_s: number; ask_life_s: number }

export const DEFAULT_POLICY: RetentionPolicy = {
  content_s: defaults.content_days * DAY_S,
  records_s: defaults.records_days * DAY_S,
  ask_life_s: defaults.ask_life_hours * 3600,
};
export const REMOTE_WINDOW_S = defaults.remote_window_days * DAY_S;

// The least time between two sweeps, and how much one sweep does: a sweep
// runs on a request's path, so a large backlog is taken in parts.
export const SWEEP_INTERVAL_MS = 3600_000;
const BATCH = 500;
const MAX_BATCHES = 20;
const MAX_S = 36500 * DAY_S;

// The X-Repowire-Retention header, or null when it is absent or malformed.
export function parsePolicy(raw: string | null): RetentionPolicy | null {
  if (!raw) return null;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    const field = (key: string) => {
      const n = value[key];
      return typeof n === 'number' && Number.isInteger(n) && n >= 0 && n <= MAX_S ? n : null;
    };
    const [content_s, records_s, ask_life_s] = [field('content_s'), field('records_s'), field('ask_life_s')];
    return content_s === null || records_s === null || ask_life_s === null ? null : { content_s, records_s, ask_life_s };
  } catch {
    return null;
  }
}

// The policy a sweep applies when several daemons named one: per class the
// longest keep, where 0 (forever) outranks any age, and the longest ask
// lifetime. No machine's data goes sooner than its own policy allows.
export function mostConservative(policies: RetentionPolicy[]): RetentionPolicy {
  if (!policies.length) return DEFAULT_POLICY;
  const keep = (ages: number[]) => (ages.includes(0) ? 0 : Math.max(...ages));
  return {
    content_s: keep(policies.map((p) => p.content_s)),
    records_s: keep(policies.map((p) => p.records_s)),
    ask_life_s: Math.max(...policies.map((p) => p.ask_life_s)),
  };
}

// Age in seconds after which an operation's dedupe marker may go: the records
// age, never shorter than the cross-machine window or the ask lifetime, since
// a resend or read-back of the id may come until then. 0 keeps it forever.
export function markerAge(p: RetentionPolicy) {
  return p.records_s === 0 ? 0 : Math.max(p.records_s, REMOTE_WINDOW_S, p.ask_life_s);
}

// Age in seconds after which a retired agent, or a workspace no live agent
// uses, is deleted whole. It is content (a conversation, a filesystem), so the
// content age applies, no later than records, with the same floors as the
// markers it takes with it. 0 keeps it forever.
export function wholeAge(p: RetentionPolicy) {
  const content = p.records_s > 0 && (p.content_s === 0 || p.records_s < p.content_s) ? p.records_s : p.content_s;
  return content === 0 ? 0 : Math.max(content, REMOTE_WINDOW_S, p.ask_life_s);
}

// The cutoffs of one sweep. A class kept forever has none. Content goes no
// later than its record. Text columns hold ISO instants, integer columns
// Unix seconds; both forms are given.
export interface Cutoffs {
  records: { iso: string; s: number } | null;
  content: { iso: string; s: number } | null;
  remote: number;
}

export function cutoffs(policy: RetentionPolicy, nowMs: number): Cutoffs {
  // Nothing younger than the ask lifetime expires (state.RetentionPolicy.Effective).
  const floor = (age: number) => (age > 0 && age < policy.ask_life_s ? policy.ask_life_s : age);
  const records = floor(policy.records_s);
  let content = floor(policy.content_s);
  if (records > 0 && (content === 0 || records < content)) content = records;
  const at = (age: number) => (age > 0 ? { iso: new Date(nowMs - age * 1000).toISOString(), s: Math.floor(nowMs / 1000) - age } : null);
  return { records: at(records), content: at(content), remote: Math.floor(nowMs / 1000) - REMOTE_WINDOW_S };
}

// One statement over at most BATCH rows of table. set is the UPDATE
// assignment, empty for a delete; where must stop matching a row once set
// has run, so the batches end.
export interface SweepRule { name: string; table: string; where: string; set?: string; args: SqlStorageValue[] }

// A domain's declaration for the tables it owns: its rules per class, or why
// the tables are exempt. Records rules of every table run before any content
// rule, so content rules only see rows that are staying.
export type TableRetention =
  | { tables: string[]; rules: (c: Cutoffs) => { records?: SweepRule[]; content?: SweepRule[] } }
  | { tables: string[]; exempt: string };

interface Sql { exec(query: string, ...args: SqlStorageValue[]): { rowsWritten: number } }

// Runs every rule once, in batches; returns the rows each changed. A rule
// that still had work when the sweep's budget ran out finishes next time.
export function sweep(sql: Sql, declarations: TableRetention[], policy: RetentionPolicy, nowMs: number) {
  const c = cutoffs(policy, nowMs);
  const records: SweepRule[] = [];
  const content: SweepRule[] = [];
  for (const d of declarations) {
    if ('exempt' in d) continue;
    const r = d.rules(c);
    records.push(...(r.records ?? []));
    content.push(...(r.content ?? []));
  }
  const counts: Record<string, number> = {};
  let budget = MAX_BATCHES;
  for (const rule of [...records, ...content]) {
    const rows = `rowid IN (SELECT rowid FROM ${rule.table} WHERE ${rule.where} LIMIT ${BATCH})`;
    const statement = rule.set ? `UPDATE ${rule.table} SET ${rule.set} WHERE ${rows}` : `DELETE FROM ${rule.table} WHERE ${rows}`;
    while (budget > 0) {
      budget--;
      const changed = sql.exec(statement, ...rule.args).rowsWritten;
      if (changed) counts[rule.name] = (counts[rule.name] ?? 0) + changed;
      if (changed < BATCH) break;
    }
  }
  return counts;
}
