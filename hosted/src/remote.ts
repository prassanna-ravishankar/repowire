// The managed machine as a cross-machine target (rfcs/managed-in-the-mesh.md,
// rfcs/cross-machine-circles.md). The relay forwards a daemon's /remote
// request here with the control token, the owner and the sending machine.
// The owner's Gateway keeps the records; agents on managed are the targets.
import { sharedCircle } from './circles';
import type { Gateway, RemoteInbound } from './gateway';
import { remoteResult } from './gateway';
import { escape, HttpError, scopedName, within } from './http';
import type { HostedAgent, OperationState } from './index';

const CREATE_HORIZON_S = 6 * 24 * 3600;
const RETENTION_S = 7 * 24 * 3600;
const FUTURE_SKEW_S = 5 * 60;
const WAIT_CAP_S = 20;
const RECOVER_BATCH = 8;
const RECOVER_MS = 2000;
// A request younger than this may still be on its way to its agent; lazy
// recovery leaves it to its own dispatch.
const RECOVER_AFTER_S = 30;

export interface RemoteEnv {
  AGENTS: DurableObjectNamespace<HostedAgent>;
  GATEWAYS: DurableObjectNamespace<Gateway>;
}

interface Principal { peer_id: string; display_name: string; circle: string }
type Row = { id: string; display_name: unknown; circle: unknown; state: unknown; description: unknown; retired_at: unknown; created_by: unknown; home_machine: unknown };

const json = (value: unknown, status = 200) => Response.json(value, { status });
// The daemon's error shape: {detail}.
const refuse = (status: number, detail: unknown) => json({ detail }, status);

const ID = { ask: /^ask-(\d+)-[0-9a-f]{32}$/, notify: /^notif-(\d+)-[0-9a-f]{32}$/ };
const idTime = (kind: 'ask' | 'notify', id: unknown) => {
  const match = typeof id === 'string' ? ID[kind].exec(id) : null;
  return match ? Number(match[1]) : null;
};

function principal(value: unknown): Principal | null {
  const from = value as Partial<Principal> | null;
  const text = (v: unknown) => typeof v === 'string' && v.length > 0 && v.length <= 200;
  if (!from || !text(from.peer_id) || !text(from.display_name) || typeof from.circle !== 'string' || !sharedCircle(from.circle)) return null;
  return { peer_id: from.peer_id!, display_name: from.display_name!, circle: from.circle };
}

const onManaged = (row: Row) => row.created_by !== 'daemon' && !row.home_machine && !row.retired_at;
const nameOf = (row: Row) => (typeof row.display_name === 'string' && row.display_name) || row.id;

// deadline is the relay's absolute Unix-ms deadline for this request. Work
// that would start after it is not started: the source has given up, and its
// later read fences the id.
export async function remoteRoute(env: RemoteEnv, owner: string, fromMachine: string, request: Request, path: string, input: Record<string, unknown>,
  deadline: number, background: (work: Promise<unknown>) => void) {
  if (request.method !== 'POST') throw new HttpError(405, 'Method not allowed');
  const late = () => Date.now() >= deadline;
  if (late()) return refuse(504, 'the request deadline passed before it was handled; nothing was done');
  const directory = env.GATEWAYS.getByName(owner);
  const now = Math.floor(Date.now() / 1000);
  // Housekeeping never holds this request: pruning is one statement, and
  // recovery of other stuck requests runs in the background.
  await directory.pruneRemote(now - RETENTION_S);
  background(recoverDispatching(env, owner));
  const epoch = await directory.storeEpoch();
  const receipt = (record: RemoteInbound, responder: string) => json({
    id: record.id, state: record.state, ...(record.close_reason ? { close_reason: record.close_reason } : {}),
    ...(record.reply != null ? { reply: record.reply } : {}), responder, store_epoch: epoch,
  });

  if (path === '/remote/peers') {
    const from = principal(input.from);
    if (!from) return refuse(403, 'from must name the sending peer in a shared circle');
    const name = typeof input.name === 'string' ? input.name : '';
    const peers = (await directory.agents() as Row[])
      .filter((row) => onManaged(row) && row.circle === from.circle && (!name || nameOf(row) === name || row.id === name))
      .map((row) => ({
        peer_id: row.id, display_name: nameOf(row), circle: row.circle, backend: 'hosted',
        status: row.state === 'working' ? 'busy' : 'online', ...(row.description ? { description: row.description } : {}),
      }));
    return json({ peers, store_epoch: epoch });
  }

  const create = /^\/remote\/(ask|notify)$/.exec(path);
  if (create) {
    const kind = create[1] as 'ask' | 'notify';
    const created = idTime(kind, input.id);
    if (created == null) return refuse(400, 'malformed id');
    if (created < now - CREATE_HORIZON_S || created > now + FUTURE_SKEW_S) return refuse(400, 'stale id');
    if (input.expected_epoch !== epoch) return refuse(409, { error: 'epoch_changed', store_epoch: epoch });
    const from = principal(input.from);
    if (!from) return refuse(403, 'from must name the sending peer in a shared circle');
    if (typeof input.text !== 'string' || !input.text.trim() || input.text.length > 16000) return refuse(400, 'text must contain 1-16000 characters');
    const target = typeof input.to_peer === 'string' ? await directory.agent(input.to_peer) as Row | null : null;
    if (!target || !onManaged(target) || target.circle !== from.circle) {
      return refuse(404, `no reachable peer ${String(input.to_peer)} in circle ${from.circle}`);
    }
    if (late()) return refuse(504, 'the request deadline passed before it was handled; nothing was done');
    const result = await directory.createInbound({
      id: input.id as string, kind, from_machine: fromMachine, from_peer: from.peer_id, from_name: from.display_name,
      from_circle: from.circle, to_agent: target.id, text: input.text, created_at: created,
    });
    if ('conflict' in result) return refuse(409, 'request id already used for a different request');
    const record = result.created ? await dispatch(env, owner, result.record, late) : result.record;
    return receipt(record, nameOf(target));
  }

  const read = /^\/remote\/asks\/([^/]+)$/.exec(path);
  if (read) {
    const id = read[1];
    const created = idTime(id.startsWith('notif-') ? 'notify' : 'ask', id);
    if (created == null) return refuse(400, 'malformed id');
    const from = input.from as Partial<Principal> | undefined;
    const asker = typeof from?.peer_id === 'string' ? from.peer_id : '';
    const look = async () => await directory.readInbound(id, fromMachine, asker, created, now - CREATE_HORIZON_S) as RemoteInbound | 'not_found' | 'expired';
    const first = await look();
    if (first === 'not_found') return refuse(403, "not this origin's request");
    if (first === 'expired') return refuse(410, { error: 'expired', store_epoch: epoch });
    let record: RemoteInbound = first;
    if (record.state === 'dispatching') record = await recover(env, owner, record, Math.min(RECOVER_MS, deadline - Date.now() - 500));
    const remaining = Math.max(0, (deadline - Date.now()) / 1000 - 1);
    const wait = Math.min(Math.max(Number(input.wait) || 0, 0), WAIT_CAP_S, remaining);
    if (record.kind === 'ask' && record.state === 'open' && wait > 0) {
      // Settle from pi's result when the report has not landed yet; the
      // report's later write is a no-op.
      const outcome = await within(agentOf(env, owner, record.to_agent).outcome(id), wait * 1000);
      if (outcome && outcome.reason !== 'not_found') {
        const [reason, reply] = remoteResult(outcome.status, outcome.reason ?? null, outcome.text ?? null);
        await directory.settleInbound(id, reason, reply);
      }
      const reread = await look();
      if (typeof reread !== 'string') record = reread;
    }
    const target = await directory.agent(record.to_agent) as Row | null;
    return receipt(record, target ? nameOf(target) : record.to_agent);
  }

  throw new HttpError(404, 'Not found');
}

const agentOf = (env: RemoteEnv, owner: string, agent: string) => env.AGENTS.getByName(scopedName(owner, agent));

// Hands a freshly recorded request to its agent once. A definite refusal
// settles it; a submit that may have landed leaves it dispatching for
// recovery, and the source hears that the outcome is unknown.
async function dispatch(env: RemoteEnv, owner: string, record: RemoteInbound, late: () => boolean): Promise<RemoteInbound> {
  const directory = env.GATEWAYS.getByName(owner);
  // Recorded but never handed over: settling it now is exact, and the
  // source's read finds the result.
  if (late()) return (await directory.settleInbound(record.id, 'not_delivered', null)) ?? record;
  const refusal = await directory.admit(record.to_agent);
  if (refusal) return (await directory.settleInbound(record.id, 'over_budget', null)) ?? record;
  if (late()) return (await directory.settleInbound(record.id, 'not_delivered', null)) ?? record;
  const sender = `${record.from_name}@${record.from_machine}`;
  const framed = `<peer-message from="@${escape(sender)}" type="${record.kind}" correlation-id="${record.id}">\n${escape(record.text)}\n</peer-message>`;
  let accepted;
  try {
    accepted = await agentOf(env, owner, record.to_agent).submit(framed, record.id, { from: sender, type: record.kind, correlationId: record.id }, 'control');
  } catch (error) {
    console.error('Remote request submit failed; left for recovery', record.id, error);
    throw new HttpError(502, `Delivery is uncertain; read ${record.id} for its outcome and do not resend`);
  }
  if ('retired' in accepted || 'undelivered' in accepted) return (await directory.settleInbound(record.id, 'not_delivered', null)) ?? record;
  await directory.advanceInbound(record.id, 'dispatching', record.kind === 'ask' ? 'open' : 'delivered');
  return (await directory.inboundRecord(record.id)) ?? record;
}

// Resolves a request caught mid-dispatch from what pi has, never from a
// marker. A timeout leaves it dispatching for a later pass.
async function recover(env: RemoteEnv, owner: string, record: RemoteInbound, ms = RECOVER_MS): Promise<RemoteInbound> {
  const directory = env.GATEWAYS.getByName(owner);
  // A slow or failed answer is unknown, never proof either way: the row
  // stays dispatching for a later pass.
  const found = await within(agentOf(env, owner, record.to_agent).fence(record.id) as Promise<OperationState>, ms)
    .catch((error) => { console.warn('Remote recovery failed; retried later', record.id, error); return null; });
  if (!found) return record;
  if (found.status === 'fenced') await directory.settleInbound(record.id, 'not_delivered', null);
  else if (found.status === 'durable' || record.kind === 'notify') {
    await directory.advanceInbound(record.id, 'dispatching', record.kind === 'ask' ? 'open' : 'delivered');
  } else if (found.status === 'terminal') {
    const [reason, reply] = remoteResult(found.result.status, found.result.reason, found.result.text);
    await directory.settleInbound(record.id, reason, reply);
  }
  return (await directory.inboundRecord(record.id)) ?? record;
}

// At most a batch of stuck requests, once at a time per owner in this
// isolate, in the background. A failed row is logged and left for a later
// pass; it never fails or delays another request.
const recovering = new Map<string, Promise<void>>();
function recoverDispatching(env: RemoteEnv, owner: string): Promise<void> {
  const running = recovering.get(owner);
  if (running) return running;
  const work = (async () => {
    const rows = await env.GATEWAYS.getByName(owner).dispatchingInbound(RECOVER_BATCH, Math.floor(Date.now() / 1000) - RECOVER_AFTER_S);
    const results = await Promise.allSettled(rows.map((row) => recover(env, owner, row)));
    for (const [i, result] of results.entries()) {
      if (result.status === 'rejected') console.warn('Remote recovery failed; retried later', rows[i].id, result.reason);
    }
  })().catch((error) => console.warn('Remote recovery pass failed', error)).finally(() => recovering.delete(owner));
  recovering.set(owner, work);
  return work;
}
