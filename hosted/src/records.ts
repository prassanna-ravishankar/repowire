// The owner's records of the cross-machine protocol (rfcs/managed-in-the-mesh.md):
// requests other machines sent to managed agents (inbound, with fences for
// ids read before they arrived), and asks managed agents sent elsewhere
// (outbound). They are the owner's, not an agent's: reconciliation claims
// them in batches across agents, and a run's report settles its ask in the
// same transaction as the run.
import { followUp, type MeshEnv } from './mesh';
import { MANAGED, remoteResult } from './protocol';
import type { TableRetention } from './retention';

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
  state: string; close_reason: string | null; reply: string | null; delivered: number; disposition: string | null; checked_at: number; delivery_at: number;
}

export const RECORDS_SCHEMA = `
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
  close_reason TEXT, reply TEXT, delivered INTEGER NOT NULL DEFAULT 0, disposition TEXT, checked_at INTEGER NOT NULL DEFAULT 0,
  delivery_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS remote_outbound_open ON remote_outbound (state, delivered, checked_at);
`;
// Columns added after the first deploy, for directories created before them.
export const RECORDS_ADDED_COLUMNS = ['ALTER TABLE remote_outbound ADD COLUMN delivery_at INTEGER NOT NULL DEFAULT 0'];

// Cross-machine records keep the protocol's fixed window whatever the
// policy: a live remote ask is retried and answered from its row, and a
// fence guards its id against a late create until then.
export const REMOTE_RETENTION: TableRetention = {
  tables: ['remote_inbound', 'remote_outbound', 'remote_fences'],
  rules: (c) => ({
    records: ['remote_outbound', 'remote_inbound', 'remote_fences'].map((table) => ({ name: `${table}.delete`, table, where: 'created_at < ?', args: [c.remote] })),
  }),
};

// What the records need from the Gateway that holds them.
export interface RecordsHooks {
  env: unknown;
  owner(): Promise<string | undefined>;
  // An agent's open asks changed: its directory row is published again.
  changed(agent: string): void;
  waitUntil(work: Promise<unknown>): void;
}

export class RemoteRecords {
  constructor(private sql: SqlStorage, private storage: DurableObjectStorage, private hooks: RecordsHooks) {}

  // This owner's side of the cross-machine protocol when managed is the
  // target. Every multi-statement change is one synchronous transaction, and
  // every terminal write is a compare-and-set, so a racing settle, read or
  // replayed report changes nothing once a result is recorded.

  storeEpoch(): string {
    let epoch = this.storage.kv.get<string>('store_epoch');
    if (!epoch) {
      epoch = crypto.randomUUID();
      this.storage.kv.put('store_epoch', epoch);
    }
    return epoch;
  }

  // Records an inbound request before dispatch. A fence for the id answers
  // not_delivered; a known id answers its record; a different envelope is a
  // conflict.
  createInbound(record: RemoteEnvelope): { conflict: true } | { record: RemoteInbound; created: boolean } {
    return this.storage.transactionSync(() => {
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
    return this.storage.transactionSync(() => {
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
    const won = this.storage.transactionSync(() => this.settleRow(id, reason, reply));
    if (won) this.pushSettled(won);
    return won;
  }

  // Requests caught mid-dispatch, for lazy recovery.
  dispatchingInbound(limit: number, before: number) {
    return this.sql.exec("SELECT * FROM remote_inbound WHERE state = 'dispatching' AND created_at < ? ORDER BY created_at LIMIT ?", before, limit).toArray() as unknown as RemoteInbound[];
  }

  // ---- outbound: managed agents as askers. The row is authoritative; its
  // envelope is immutable and every transition a compare-and-set.

  // A fresh row counts as just checked: its own send (at most 20 s) is still
  // creating it on the target, and a read inside the 30 s cutoff would fence
  // it there.
  insertOutbound(row: RemoteOutboundInit) {
    this.sql.exec(
      `INSERT INTO remote_outbound (id, machine, store_epoch, to_peer, to_name, from_agent, text, created_at, state, checked_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'sent', ?)`,
      row.id, row.machine, row.store_epoch, row.to_peer, row.to_name, row.from_agent, row.text, row.created_at, row.created_at);
    this.asksChanged(row.from_agent);
  }

  // An agent's open asks changed, whether or not a run followed: bump its
  // revision (strictly, unlike a clock) and publish it, so a page showing its
  // asks reloads.
  private asksChanged(agent: string | undefined) {
    if (!agent) return;
    this.sql.exec('UPDATE agents SET asks_rev = asks_rev + 1 WHERE id = ?', agent);
    this.hooks.changed(agent);
  }

  private outboundAgent(id: string) {
    return this.sql.exec('SELECT from_agent FROM remote_outbound WHERE id = ?', id).toArray()[0]?.from_agent as string | undefined;
  }

  outboundRecord(id: string) {
    return (this.sql.exec('SELECT * FROM remote_outbound WHERE id = ?', id).toArray()[0] as unknown as RemoteOutbound | undefined) ?? null;
  }

  markOutboundUncertain(id: string) {
    const changed = this.sql.exec("UPDATE remote_outbound SET state = 'uncertain' WHERE id = ? AND state = 'sent'", id).rowsWritten > 0;
    if (changed) this.asksChanged(this.outboundAgent(id));
    return changed;
  }

  // One terminal transition for a push, a read and a create response alike.
  settleOutbound(id: string, reason: string, reply: string | null) {
    const changed = this.sql.exec("UPDATE remote_outbound SET state = 'settled', close_reason = ?, reply = ? WHERE id = ? AND state IN ('sent', 'uncertain')",
      reason, reply, id).rowsWritten > 0;
    if (changed) this.asksChanged(this.outboundAgent(id));
    return changed;
  }

  markOutboundDelivered(id: string, disposition: string | null = null) {
    if (this.sql.exec('UPDATE remote_outbound SET delivered = 1, disposition = ? WHERE id = ? AND delivered = 0', disposition, id).rowsWritten > 0) {
      this.asksChanged(this.outboundAgent(id));
    }
  }

  // Unsettled rows not read in the last cutoff, oldest check first; each is
  // stamped as it is claimed so every row gets its turn.
  claimOutboundChecks(checkedBefore: number, now: number, limit: number) {
    return this.storage.transactionSync(() => {
      const rows = this.sql.exec("SELECT * FROM remote_outbound WHERE state != 'settled' AND checked_at < ? ORDER BY checked_at, created_at LIMIT ?",
        checkedBefore, limit).toArray() as unknown as RemoteOutbound[];
      for (const row of rows) this.sql.exec('UPDATE remote_outbound SET checked_at = ? WHERE id = ?', now, row.id);
      return rows;
    });
  }

  // Settled replies to deliver, claimed like checks: never-tried first, and
  // each stamped, so a reply blocked on its asker (over budget, unavailable)
  // waits out the cutoff instead of holding the batch.
  claimDeliveries(triedBefore: number, now: number, limit: number) {
    return this.storage.transactionSync(() => {
      const rows = this.sql.exec("SELECT * FROM remote_outbound WHERE state = 'settled' AND delivered = 0 AND delivery_at < ? ORDER BY delivery_at, created_at LIMIT ?",
        triedBefore, limit).toArray() as unknown as RemoteOutbound[];
      for (const row of rows) this.sql.exec('UPDATE remote_outbound SET delivery_at = ? WHERE id = ?', now, row.id);
      return rows;
    });
  }

  // An agent's open cross-machine asks, for /managed.
  openOutbound(agent: string) {
    return this.sql.exec("SELECT * FROM remote_outbound WHERE from_agent = ? AND (state != 'settled' OR delivered = 0) ORDER BY created_at", agent)
      .toArray() as unknown as RemoteOutbound[];
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
  settleRemoteRun(agent: string, op: string, status: string, reason: string | null, text: string | null) {
    const row = this.sql.exec("SELECT kind FROM remote_inbound WHERE id = ? AND to_agent = ? AND state IN ('dispatching', 'open')", op, agent).toArray()[0];
    if (!row || row.kind !== 'ask') return null;
    const [closeReason, reply] = remoteResult(status, reason, text);
    return this.settleRow(op, closeReason, reply);
  }

  // One settlement push to the source, after the commit and off the caller's
  // path. A lost push is recovered by the source reading the record back.
  pushSettled(row: RemoteInbound) {
    if (row.kind !== 'ask') return;
    // Managed to managed: the source's row is in this same Gateway.
    if (row.from_machine === MANAGED) {
      this.settleOutbound(row.id, row.close_reason ?? 'unanswered', row.reply);
      void this.hooks.owner().then((owner) => { if (owner) this.hooks.waitUntil(followUp(this.hooks.env as MeshEnv, owner)); });
      return;
    }
    const env = this.hooks.env as { RELAY_URL?: string; CONTROL_TOKEN?: string };
    if (!env.RELAY_URL || !env.CONTROL_TOKEN) return;
    const send = async () => {
      const owner = await this.hooks.owner();
      if (!owner) return;
      const response = await fetch(`${env.RELAY_URL!.replace(/\/$/, '')}/api/v1/managed/machine-request`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${env.CONTROL_TOKEN}`, 'X-Repowire-Owner': owner, 'Content-Type': 'application/json' },
        body: JSON.stringify({ machine: row.from_machine, method: 'POST', path: '/remote/settled',
          body: { id: row.id, close_reason: row.close_reason, ...(row.reply != null ? { reply: row.reply } : {}), store_epoch: this.storeEpoch() } }),
      });
      if (!response.ok) console.warn('Remote settlement push failed; the source reads it back', row.id, response.status);
    };
    this.hooks.waitUntil(send().catch((error) => console.warn('Remote settlement push failed; the source reads it back', row.id, error)));
  }
}
