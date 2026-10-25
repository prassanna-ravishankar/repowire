import { test } from 'node:test';
import assert from 'node:assert/strict';
import worker from '../src/index';
import { Gateway } from '../src/gateway';
import { hostedAgent } from './support/agent';
import { fakeState } from './support/state';

// The relay's request API over the real agent and directory code, for an
// agent the relay created (createdBy 'web') unless a test says otherwise.
function relay(createdBy = 'web') {
  const parts = hostedAgent(new Gateway(fakeState() as never, {}), createdBy);
  const env = {
    CONTROL_TOKEN: 'control', HOSTED_TOKEN: 'daemon', HOSTED_OWNER: 'alice',
    GATEWAYS: { getByName: () => parts.directory }, AGENTS: { getByName: () => parts.agent },
  };
  const call = (method: string, path: string, body?: unknown, token = 'control') => worker.fetch(new Request(`https://hosted${path}`, {
    method, body: body === undefined ? undefined : JSON.stringify(body),
    headers: { Authorization: `Bearer ${token}`, 'X-Repowire-Owner': 'alice' },
  }), env as never);
  return { ...parts, call };
}

const id = (n: number) => `rq-${String(n).padStart(32, '0')}`;
const ask = (n: number, extra: Record<string, unknown> = {}) => ({ id: id(n), agent: 'a', kind: 'ask', requester: 'relay-mcp-g1', text: 'what is 6*7?', ...extra });
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

test('an ask is answered through the run it started, and never reaches the daemon outbox', async () => {
  const { call, pi, directory } = relay();
  const sent = await call('POST', '/requests', ask(1));
  assert.equal(sent.status, 202);
  assert.deepEqual(await sent.json(), { request_id: id(1), state: 'accepted', created: true });
  assert.ok(pi.submitted.has(id(1)));

  assert.deepEqual(await (await call('GET', `/requests/${id(1)}?requester=relay-mcp-g1`)).json(), { correlation_id: id(1), status: 'pending' });
  assert.deepEqual((await (await call('GET', '/requests?requester=relay-mcp-g1')).json()).items.map((row: { request_id: string }) => row.request_id), [id(1)]);

  pi.settle(id(1), { status: 'done', text: '42' });
  await flush();
  assert.deepEqual(await (await call('GET', `/requests/${id(1)}?requester=relay-mcp-g1&wait=5`)).json(),
    { correlation_id: id(1), status: 'resolved', responder: 'a', close_reason: 'answered', reply: '42' });
  assert.deepEqual((await (await call('GET', '/requests?requester=relay-mcp-g1')).json()).items, []);
  assert.equal((await directory.clients()).outbox, 0);
});

test('a retry returns the first receipt before budget or retirement is checked again', async () => {
  const { call, agent, pi } = relay();
  assert.equal((await call('POST', '/requests', ask(2))).status, 202);
  await agent.retire();
  const retry = await call('POST', '/requests', ask(2));
  assert.equal(retry.status, 202);
  assert.equal((await retry.json()).created, false);
  assert.equal(pi.submitted.size, 1);
  assert.equal((await call('POST', '/requests', ask(3))).status, 410);
});

test('a reused id with a different body is a conflict', async () => {
  const { call } = relay();
  await call('POST', '/requests', ask(4));
  assert.equal((await call('POST', '/requests', ask(4, { text: 'something else' }))).status, 409);
});

test('only the requester reads its request', async () => {
  const { call } = relay();
  await call('POST', '/requests', ask(5));
  assert.equal((await call('GET', `/requests/${id(5)}?requester=relay-mcp-other`)).status, 404);
  assert.deepEqual((await (await call('GET', '/requests?requester=relay-mcp-other')).json()).items, []);
});

test('notifies are not open requests', async () => {
  const { call } = relay();
  assert.equal((await call('POST', '/requests', ask(6, { kind: 'notify' }))).status, 202);
  assert.deepEqual((await (await call('GET', '/requests?requester=relay-mcp-g1')).json()).items, []);
});

test('a submit the runtime never took reads as not delivered, not as a resend invitation', async () => {
  const { call, pi } = relay();
  pi.failSubmit = true;
  const sent = await call('POST', '/requests', ask(7));
  assert.equal(sent.status, 502);
  assert.match((await sent.json()).error, /do not resend/);
  assert.deepEqual(await (await call('GET', `/requests/${id(7)}?requester=relay-mcp-g1`)).json(),
    { correlation_id: id(7), status: 'resolved', responder: 'a', close_reason: 'not_delivered' });
});

test('an aborted ask resolves with its reason and no reply', async () => {
  const { call, pi } = relay();
  await call('POST', '/requests', ask(8));
  pi.settle(id(8), { status: 'unanswered', reason: 'aborted' });
  await flush();
  assert.deepEqual(await (await call('GET', `/requests/${id(8)}?requester=relay-mcp-g1`)).json(),
    { correlation_id: id(8), status: 'resolved', responder: 'a', close_reason: 'aborted' });
});

test('a submit that never reached the agent settles as not delivered, and a late submit is refused', async () => {
  const { call, agent, pi } = relay();
  const start = agent.lifecycle.start;
  agent.lifecycle.start = async () => { throw new Error('lifecycle unavailable'); };
  assert.equal((await call('POST', '/requests', ask(10))).status, 502);
  agent.lifecycle.start = start;
  // The retry finds the record, not a fresh submission.
  assert.equal((await (await call('POST', '/requests', ask(10))).json()).state, 'submitting');
  assert.deepEqual(await (await call('GET', `/requests/${id(10)}?requester=relay-mcp-g1`)).json(),
    { correlation_id: id(10), status: 'resolved', responder: 'a', close_reason: 'not_delivered' });
  assert.deepEqual((await (await call('GET', '/requests?requester=relay-mcp-g1')).json()).items, []);
  // A submit arriving after that decision is refused, never run.
  assert.deepEqual(await agent.submit('late', id(10), undefined, 'control'), { undelivered: true });
  assert.equal(pi.submitted.size, 0);
});

test('a read while the submission holds admission returns pending within its wait', async () => {
  const { call, agent } = relay();
  let release!: () => void;
  const start = agent.lifecycle.start;
  agent.lifecycle.start = () => new Promise<void>((resolve) => { release = resolve; });
  const sending = call('POST', '/requests', ask(11));
  await flush();
  for (const wait of [0, 1]) {
    const began = Date.now();
    assert.deepEqual(await (await call('GET', `/requests/${id(11)}?requester=relay-mcp-g1&wait=${wait}`)).json(),
      { correlation_id: id(11), status: 'pending' });
    assert.ok(Date.now() - began < 1500, `wait=${wait} took ${Date.now() - began}ms`);
  }
  release();
  agent.lifecycle.start = start;
  assert.equal((await sending).status, 202);
  assert.deepEqual(await (await call('GET', `/requests/${id(11)}?requester=relay-mcp-g1`)).json(), { correlation_id: id(11), status: 'pending' });
});

test('a step-limited run reads the same before and after its report lands', async () => {
  const { call, agent, pi, gateway } = relay();
  await call('POST', '/requests', ask(12));
  agent.kv.put(`steps:${id(12)}`, 25);
  gateway.failReports = 1;
  pi.settle(id(12), { status: 'unanswered', reason: 'aborted' });
  await flush();
  const before = await (await call('GET', `/requests/${id(12)}?requester=relay-mcp-g1&wait=1`)).json();
  assert.match(before.close_reason, /^step_limit/);
});

test('requests are for the relay, and only reach agents on the managed machine', async () => {
  const { call, directory } = relay();
  assert.equal((await call('POST', '/requests', ask(9), 'daemon')).status, 403);
  directory.addAgent({ id: 'a', workspace: null, created_by: 'daemon', home_machine: 'laptop' });
  // home_machine is kept once set, so this agent now belongs to the laptop.
  assert.equal((await call('POST', '/requests', ask(9))).status, 404);
});

test('a daemon-created agent without a home is not on the managed machine', async () => {
  const { call } = relay('daemon');
  assert.equal((await call('POST', '/requests', ask(13))).status, 404);
});
