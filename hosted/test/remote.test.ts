import { test } from 'node:test';
import assert from 'node:assert/strict';
import worker from '../src/index';
import { Gateway } from '../src/gateway';
import { fakeState } from './support/state';
import { hostedAgent } from './support/agent';

// The managed machine as a cross-machine target, through the Worker's fetch.
function managed(createdBy = 'web') {
  const parts = hostedAgent(new Gateway(fakeState() as never, {}), createdBy);
  const env = {
    CONTROL_TOKEN: 'control', HOSTED_TOKEN: 'daemon', HOSTED_OWNER: 'alice',
    GATEWAYS: { getByName: () => parts.directory }, AGENTS: { getByName: () => parts.agent },
  };
  const call = async (path: string, body: unknown, { machine = 'laptop', token = 'control', deadline = Date.now() + 25_000 } = {}) => {
    const res = await worker.fetch(new Request(`https://hosted${path}`, {
      method: 'POST', body: JSON.stringify(body),
      headers: { Authorization: `Bearer ${token}`, 'X-Repowire-Owner': 'alice', 'X-Repowire-From-Machine': machine, 'X-Repowire-Deadline': String(deadline) },
    }), env as never);
    return { status: res.status, body: await res.json() as Record<string, unknown> };
  };
  return { ...parts, call };
}

const from = { peer_id: 'repow-1-abc', display_name: 'asker', circle: 'managed' };
let counter = 0;
const remoteID = (kind = 'ask', at = Math.floor(Date.now() / 1000)) => `${kind}-${at}-${(++counter).toString(16).padStart(32, '0')}`;
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

async function epochOf(call: ReturnType<typeof managed>['call']) {
  return (await call('/remote/peers', { from })).body.store_epoch as string;
}

test('the remote API is for the relay, naming another machine', async () => {
  const { call } = managed();
  assert.equal((await call('/remote/peers', { from }, { token: 'daemon' })).status, 403);
  assert.equal((await call('/remote/peers', { from }, { machine: 'managed' })).status, 400);
  assert.equal((await call('/remote/peers', { from: { ...from, circle: 'default' } })).status, 403);
});

test('discovery lists live managed agents in the caller circle only', async () => {
  const { call, directory } = managed();
  directory.addAgent({ id: 'other', workspace: null, created_by: 'web', circle: 'research' });
  directory.addAgent({ id: 'mine', workspace: null, created_by: 'daemon', home_machine: 'laptop' });
  const listed = await call('/remote/peers', { from });
  assert.deepEqual((listed.body.peers as { peer_id: string }[]).map((p) => p.peer_id), ['a']);
  assert.ok(listed.body.store_epoch);
  assert.deepEqual(((await call('/remote/peers', { from: { ...from, circle: 'research' } })).body.peers as { peer_id: string }[]).map((p) => p.peer_id), ['other']);
  assert.deepEqual((await call('/remote/peers', { from, name: 'nobody' })).body.peers, []);
});

test('an ask is recorded, dispatched once, and settles from its run', async () => {
  const { call, pi } = managed();
  const epoch = await epochOf(call);
  const id = remoteID();
  const ask = { id, from, to_peer: 'a', text: 'what is 6*7?', expected_epoch: epoch };
  const created = await call('/remote/ask', ask);
  assert.equal(created.status, 200);
  assert.equal(created.body.state, 'open');
  assert.ok(pi.submitted.has(id));
  pi.submitted.delete(id);
  // A retried create answers its receipt and dispatches nothing.
  assert.equal((await call('/remote/ask', ask)).body.state, 'open');
  assert.equal(pi.submitted.has(id), false);
  pi.submitted.add(id);
  assert.equal((await call('/remote/ask', { ...ask, text: 'other' })).status, 409);

  pi.settle(id, { status: 'done', text: '42' });
  await flush();
  const read = await call(`/remote/asks/${id}`, { from });
  assert.deepEqual([read.body.state, read.body.close_reason, read.body.reply, read.body.responder], ['settled', 'ack_with_msg', '42', 'a']);
  assert.equal((await call(`/remote/asks/${id}`, { from }, { machine: 'desktop' })).status, 403);
  assert.equal((await call(`/remote/asks/${id}`, { from: { ...from, peer_id: 'someone' } })).status, 403);
});

test('a waited read settles from the result before the report lands', async () => {
  const { call, pi, gateway } = managed();
  const epoch = await epochOf(call);
  const id = remoteID();
  await call('/remote/ask', { id, from, to_peer: 'a', text: 'q', expected_epoch: epoch });
  gateway.failReports = 5;
  const waiting = call(`/remote/asks/${id}`, { from, wait: 2 });
  await flush();
  pi.settle(id, { status: 'unanswered', reason: 'aborted' });
  const read = await waiting;
  assert.deepEqual([read.body.state, read.body.close_reason], ['settled', 'aborted']);
});

test('creates are refused for a stale id, a changed epoch, another circle or a non-managed agent', async () => {
  const { call, directory } = managed();
  const epoch = await epochOf(call);
  const ask = (extra: Record<string, unknown>) => call('/remote/ask', { id: remoteID(), from, to_peer: 'a', text: 'q', expected_epoch: epoch, ...extra });
  assert.equal((await ask({ id: remoteID('ask', Math.floor(Date.now() / 1000) - 7 * 24 * 3600) })).status, 400);
  assert.equal((await ask({ id: 'ask-1-xyz' })).status, 400);
  const changed = await ask({ expected_epoch: 'old' });
  assert.equal(changed.status, 409);
  assert.equal((changed.body.detail as { error: string }).error, 'epoch_changed');
  assert.equal((await ask({ from: { ...from, circle: 'research' } })).status, 404);
  directory.addAgent({ id: 'home', workspace: null, created_by: 'daemon', home_machine: 'laptop' });
  assert.equal((await ask({ to_peer: 'home' })).status, 404);
});

test('a read of an unknown id fences it, so a late create dispatches nothing', async () => {
  const { call, pi } = managed();
  const epoch = await epochOf(call);
  const id = remoteID();
  const fenced = await call(`/remote/asks/${id}`, { from });
  assert.deepEqual([fenced.body.state, fenced.body.close_reason], ['settled', 'not_delivered']);
  const late = await call('/remote/ask', { id, from, to_peer: 'a', text: 'q', expected_epoch: epoch });
  assert.deepEqual([late.body.state, late.body.close_reason], ['settled', 'not_delivered']);
  assert.equal(pi.submitted.has(id), false);
  // Past the create horizon an unknown id may have been pruned: expired.
  const old = remoteID('ask', Math.floor(Date.now() / 1000) - 6 * 24 * 3600 - 60);
  assert.equal((await call(`/remote/asks/${old}`, { from })).status, 410);
});

test('a notify is delivered once and needs no reply', async () => {
  const { call, pi } = managed();
  const epoch = await epochOf(call);
  const id = remoteID('notif');
  const sent = await call('/remote/notify', { id, from, to_peer: 'a', text: 'fyi', expected_epoch: epoch });
  assert.equal(sent.body.state, 'delivered');
  assert.ok(pi.submitted.has(id));
});

test('recovery settles from what pi has: absent is fenced, admitted is open', async () => {
  const { call, directory, pi, agent } = managed();
  const old = Math.floor(Date.now() / 1000) - 120;
  const envelope = (id: string) => ({ id, kind: 'ask' as const, from_machine: 'laptop', from_peer: from.peer_id, from_name: from.display_name, from_circle: 'managed', to_agent: 'a', text: 'q', created_at: old });
  const lost = remoteID('ask', old), admitted = remoteID('ask', old);
  directory.createInbound(envelope(lost));
  directory.createInbound(envelope(admitted));
  await agent.submit('q', admitted, undefined, 'control');
  // A crash before admission leaves a report marker pi never saw.
  agent.kv.put(`report:${lost}`, { source: null, via: 'control' });

  await call('/remote/peers', { from });
  // Recovery runs in the background, after the request.
  for (let i = 0; i < 20 && directory.inboundRecord(admitted)?.state !== 'open'; i++) await flush();
  assert.deepEqual([directory.inboundRecord(lost)?.state, directory.inboundRecord(lost)?.close_reason], ['settled', 'not_delivered']);
  assert.equal(directory.inboundRecord(admitted)?.state, 'open');
  assert.equal(agent.kv.get(`report:${lost}`), undefined);
  // The tombstone refuses the submit if it was still on its way.
  assert.ok('undelivered' in await agent.submit('q', lost, undefined, 'control'));
  assert.equal(pi.submitted.has(lost), false);
});

test('work past the relay deadline is never started', async () => {
  const { call, pi, directory } = managed();
  const epoch = await epochOf(call);
  const id = remoteID();
  const late = await call('/remote/ask', { id, from, to_peer: 'a', text: 'q', expected_epoch: epoch }, { deadline: Date.now() - 60_000 });
  assert.equal(late.status, 504);
  assert.equal(pi.submitted.has(id), false);
  assert.equal(directory.inboundRecord(id), null);
});

test('a stuck recovery never delays or fails other requests', async () => {
  const { call, directory, agent } = managed();
  const old = Math.floor(Date.now() / 1000) - 120;
  const stuck = remoteID('ask', old);
  directory.createInbound({ id: stuck, kind: 'ask', from_machine: 'laptop', from_peer: from.peer_id, from_name: 'asker', from_circle: 'managed', to_agent: 'a', text: 'q', created_at: old });
  agent.fence = () => new Promise(() => {});
  const started = Date.now();
  const listed = await call('/remote/peers', { from });
  assert.equal(listed.status, 200);
  assert.ok(Date.now() - started < 500, `discovery waited ${Date.now() - started} ms`);
  agent.fence = async () => { throw new Error('agent unavailable'); };
  assert.equal((await call('/remote/peers', { from })).status, 200);
  assert.equal(directory.inboundRecord(stuck)?.state, 'dispatching');
});
