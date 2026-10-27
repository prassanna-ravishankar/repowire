// Managed agents as cross-machine sources (rfcs/managed-in-the-mesh.md,
// "Worker as source"). The owner's Gateway runs these tools for an agent on
// the managed machine instead of sending them to a daemon. Daemons are
// reached through the relay as "managed"; other managed agents through the
// same /remote handlers, in-process.
import type { Gateway, RemoteOutbound } from './gateway';
import { escape, scopedName, within } from './http';
import type { HostedAgent, OperationState } from './index';
import { framedReply, MANAGED, newRemoteID } from './protocol';
import { remoteRoute } from './remote';

export interface MeshEnv {
  AGENTS: DurableObjectNamespace<HostedAgent>;
  GATEWAYS: DurableObjectNamespace<Gateway>;
  RELAY_URL?: string;
  CONTROL_TOKEN?: string;
}

export interface Principal { peer_id: string; display_name: string; circle: string }
interface Peer { peer_id: string; display_name: string; circle: string; status: string; description?: string }
type Answer = { status: number; body: Record<string, unknown> };

const SEND_MS = 20_000;
const DISCOVERY_MS = 2000;
const FANOUT = 16;
const BATCH = 8;
const CHECK_CUTOFF_S = 30;
const PASS_MS = 2000;

// One /remote request to a machine. Managed is this owner's own Gateway, so
// it runs the target handlers in-process; a daemon is reached through the
// relay. A transport failure is status 0: the outcome is unknown.
export async function machineCall(env: MeshEnv, owner: string, machine: string, path: string, body: unknown, ms: number): Promise<Answer> {
  const deadline = Date.now() + ms;
  try {
    let response: Response;
    if (machine === MANAGED) {
      response = await remoteRoute(env, owner, MANAGED, new Request('https://managed' + path, { method: 'POST' }), path,
        body as Record<string, unknown>, deadline, (work) => void work);
    } else {
      if (!env.RELAY_URL || !env.CONTROL_TOKEN) return { status: 503, body: { detail: 'no relay is configured for cross-machine requests' } };
      response = await relay(env, owner, '/machine-request', { machine, method: 'POST', path, body, deadline }, ms);
    }
    return { status: response.status, body: await response.json().catch(() => ({})) as Record<string, unknown> };
  } catch (error) {
    return { status: 0, body: { detail: String(error) } };
  }
}

function relay(env: MeshEnv, owner: string, route: string, body: unknown, ms: number) {
  return fetch(`${env.RELAY_URL!.replace(/\/$/, '')}/api/v1/managed${route}`, {
    method: 'POST', signal: AbortSignal.timeout(ms),
    headers: { Authorization: `Bearer ${env.CONTROL_TOKEN}`, 'X-Repowire-Owner': owner, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

async function daemons(env: MeshEnv, owner: string, ms: number): Promise<string[]> {
  if (!env.RELAY_URL || !env.CONTROL_TOKEN) return [];
  const response = await relay(env, owner, '/machines', {}, ms);
  if (!response.ok) throw new Error(`relay machine list returned ${response.status}`);
  return ((await response.json()) as { machines?: string[] }).machines ?? [];
}

const detail = (answer: Answer) => String((answer.body.detail as string | undefined) ?? (answer.body.error as string | undefined) ?? `HTTP ${answer.status}`);

// A target named x@machine is on that machine; x alone (or x@managed) is
// another managed agent.
function address(peerName: string): { name: string; machine: string } {
  const at = peerName.lastIndexOf('@');
  if (at <= 0 || at === peerName.length - 1) return { name: peerName, machine: MANAGED };
  return { name: peerName.slice(0, at), machine: peerName.slice(at + 1) };
}

const label = (name: string, machine: string) => `${name}@${machine}`;

// Directed resolution on the target machine, recording its store epoch.
async function resolve(env: MeshEnv, owner: string, from: Principal, peerName: string) {
  const { name, machine } = address(peerName);
  const answer = await machineCall(env, owner, machine, '/remote/peers', { from, name }, DISCOVERY_MS * 2);
  if (answer.status !== 200) throw new Error(`cannot reach ${machine}: ${detail(answer)}`);
  const peers = (answer.body.peers as Peer[] | undefined) ?? [];
  const exact = peers.filter((p) => p.peer_id === name || p.display_name === name);
  if (exact.length !== 1) throw new Error(exact.length ? `${peerName} is ambiguous; use its peer id` : `no peer ${name} in circle ${from.circle} on ${machine}`);
  if (machine === MANAGED && exact[0].peer_id === from.peer_id) throw new Error('an agent cannot message itself');
  return { machine, peer: exact[0], epoch: String(answer.body.store_epoch ?? '') };
}

export async function managedTool(env: MeshEnv, owner: string, from: Principal, tool: string, args: Record<string, unknown>): Promise<string> {
  const directory = env.GATEWAYS.getByName(owner);
  if (tool === 'whoami') return `peer_id=${from.peer_id} display_name=${from.display_name} circle=${from.circle} machine=${MANAGED}`;
  if (tool === 'set_description') return 'description set';
  if (tool === 'list_peers') return listPeers(env, owner, from);
  if (tool !== 'ask' && tool !== 'notify_peer') throw new Error(`${tool} is not available to managed agents`);

  const kind = tool === 'ask' ? 'ask' : 'notify';
  const text = String(args[kind === 'ask' ? 'query' : 'message'] ?? '');
  if (!text.trim()) throw new Error('a message is required');
  const target = await resolve(env, owner, from, String(args.peer_name ?? ''));
  const name = label(target.peer.display_name, target.machine);
  const id = newRemoteID(kind);
  if (kind === 'ask') {
    await directory.insertOutbound({ id, machine: target.machine, store_epoch: target.epoch, to_peer: target.peer.peer_id, to_name: target.peer.display_name,
      from_agent: from.peer_id, text, created_at: Math.floor(Date.now() / 1000) });
  }
  const answer = await machineCall(env, owner, target.machine, `/remote/${kind}`,
    { id, from, to_peer: target.peer.peer_id, text, expected_epoch: target.epoch }, SEND_MS);
  if (answer.status === 0 || answer.status >= 500) {
    if (kind === 'ask') await directory.markOutboundUncertain(id);
    return `${id} delivery_state=uncertain: the outcome is unknown (${detail(answer)}); do not resend`;
  }
  if (answer.status >= 400) {
    if (kind === 'ask') {
      await directory.settleOutbound(id, 'refused', detail(answer));
      await directory.markOutboundDelivered(id, 'told_at_send');
    }
    throw new Error(`${name} refused the ${kind}: ${detail(answer)}`);
  }
  if (kind === 'notify') return `${id} delivery_state=${answer.body.state === 'settled' ? String(answer.body.close_reason) : 'delivered'}`;
  if (answer.body.state === 'settled') {
    await directory.settleOutbound(id, String(answer.body.close_reason ?? 'unanswered'), (answer.body.reply as string | undefined) ?? null);
  }
  return `ask ${id} sent to @${name}; the reply arrives later as a message starting [ack #${id} from @${name}]`;
}

async function listPeers(env: MeshEnv, owner: string, from: Principal): Promise<string> {
  const deadline = Date.now() + DISCOVERY_MS;
  const rows: string[] = [];
  const partial: string[] = [];
  const add = (machine: string, peers: Peer[]) => {
    for (const p of peers) {
      if (machine === MANAGED && p.peer_id === from.peer_id) continue;
      const name = machine === MANAGED ? p.display_name : label(p.display_name, machine);
      rows.push([name, p.peer_id, p.circle, p.status, p.description ?? ''].join('\t'));
    }
  };
  const local = await machineCall(env, owner, MANAGED, '/remote/peers', { from }, DISCOVERY_MS);
  add(MANAGED, (local.body.peers as Peer[] | undefined) ?? []);
  let machines: string[] = [];
  try {
    machines = await daemons(env, owner, Math.max(deadline - Date.now(), 1));
  } catch {
    partial.push('relay');
  }
  partial.push(...machines.slice(FANOUT));
  await Promise.all(machines.slice(0, FANOUT).map(async (machine) => {
    const answer = await machineCall(env, owner, machine, '/remote/peers', { from }, Math.max(deadline - Date.now(), 1));
    if (answer.status === 200) add(machine, (answer.body.peers as Peer[] | undefined) ?? []);
    else partial.push(machine);
  }));
  const out = ['name\tpeer_id\tcircle\tstatus\tdescription', ...rows.sort()];
  if (partial.length) out.push(`# partial: ${partial.sort().join(',')}`);
  return out.join('\n');
}

// ---- follow-up: reconcile open asks, then deliver settled replies ----------
// Request-triggered and single-flight per owner in this isolate; never
// awaited by its trigger. The durable rows are the work list, so a pass lost
// to an eviction is picked up by the next trigger.

// A trigger that lands while a pass runs is not absorbed: the pass runs once
// more, so work the trigger was for (a row that just became due) is not left
// for some later trigger.
const passes = new Map<string, { done: Promise<void>; again: boolean }>();

export function followUp(env: MeshEnv, owner: string): Promise<void> {
  const running = passes.get(owner);
  if (running) {
    running.again = true;
    return running.done;
  }
  const entry = { done: Promise.resolve(), again: false };
  entry.done = (async () => {
    do {
      entry.again = false;
      await within(reconcile(env, owner), PASS_MS);
      await within(deliverReplies(env, owner), PASS_MS);
    } while (entry.again);
  })().catch((error) => console.warn('Managed follow-up failed; the next trigger retries', owner, error))
    .finally(() => passes.delete(owner));
  passes.set(owner, entry);
  return entry.done;
}

async function reconcile(env: MeshEnv, owner: string) {
  const directory = env.GATEWAYS.getByName(owner);
  const now = Math.floor(Date.now() / 1000);
  const rows = await directory.claimOutboundChecks(now - CHECK_CUTOFF_S, now, BATCH) as RemoteOutbound[];
  await Promise.allSettled(rows.map(async (row) => {
    const asker = await directory.agent(row.from_agent) as { display_name?: string | null; circle?: string } | null;
    const from = { peer_id: row.from_agent, display_name: asker?.display_name || row.from_agent, circle: asker?.circle ?? MANAGED };
    const answer = await machineCall(env, owner, row.machine, `/remote/asks/${row.id}`, { from, wait: 0 }, PASS_MS);
    const lost = answer.status === 410 || (answer.status === 200 && answer.body.store_epoch !== row.store_epoch);
    if (lost) await directory.settleOutbound(row.id, 'result_lost', null);
    else if (answer.status === 200 && answer.body.state === 'settled') {
      await directory.settleOutbound(row.id, String(answer.body.close_reason ?? 'unanswered'), (answer.body.reply as string | undefined) ?? null);
    }
  }));
}

async function deliverReplies(env: MeshEnv, owner: string) {
  const directory = env.GATEWAYS.getByName(owner);
  const now = Math.floor(Date.now() / 1000);
  const rows = await directory.claimDeliveries(now - CHECK_CUTOFF_S, now, BATCH) as RemoteOutbound[];
  for (const row of rows) {
    try {
      await deliverReply(env, owner, row);
    } catch (error) {
      console.warn('Managed reply delivery failed; the next trigger retries', row.id, error);
    }
  }
}

// Readback first, then retirement, then budget, then the submit. At least
// once, idempotent on the agent through the operation id.
async function deliverReply(env: MeshEnv, owner: string, row: RemoteOutbound) {
  const directory = env.GATEWAYS.getByName(owner);
  if ((row.close_reason === 'ack' || row.close_reason === 'ack_with_msg') && !row.reply) {
    await directory.markOutboundDelivered(row.id, 'bare_ack');
    return;
  }
  const agent = env.AGENTS.getByName(scopedName(owner, row.from_agent));
  const op = `reply-${row.id}`;
  const known = await within(agent.operation(op) as Promise<OperationState>, PASS_MS);
  if (!known) return;
  if (known.status === 'durable' || known.status === 'terminal') {
    await directory.markOutboundDelivered(row.id);
    return;
  }
  if (known.status === 'fenced') {
    console.error('A reply operation was fenced; it is never resubmitted', row.id);
    await directory.markOutboundDelivered(row.id, 'fenced');
    return;
  }
  const asker = await directory.agent(row.from_agent) as { retired_at?: unknown } | null;
  if (!asker || asker.retired_at) {
    await directory.markOutboundDelivered(row.id, 'asker_retired');
    return;
  }
  if (await directory.admit(row.from_agent)) return;
  const from = label(row.to_name, row.machine);
  // Peer content, wrapped like any peer message: never mistaken for the human.
  const text = `<peer-message from="@${escape(from)}" type="reply" correlation-id="${row.id}">\n${escape(framedReply(row.id, from, row.close_reason ?? 'unanswered', row.reply))}\n</peer-message>`;
  const accepted = await agent.submit(text, op, { from, type: 'reply', correlationId: row.id }, 'control');
  await directory.markOutboundDelivered(row.id, 'retired' in accepted ? 'asker_retired' : null);
}
