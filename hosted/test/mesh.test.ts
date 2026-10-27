import { test, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import worker from '../src/index';
import { Gateway } from '../src/gateway';
import { fakeState } from './support/state';
import { hostedAgent } from './support/agent';

// Two agents on managed in one owner's Gateway, the Worker's env wired the
// way Cloudflare wires it: the Gateway reaches agents and itself by name.
function owner() {
  const env: Record<string, unknown> = { CONTROL_TOKEN: 'control', HOSTED_TOKEN: 'daemon', HOSTED_OWNER: 'alice', RELAY_URL: 'https://relay.test' };
  const state = fakeState();
  const directory = new Gateway(state as never, env);
  void directory.owner('alice');
  const a = hostedAgent(directory, 'web');
  const b = hostedAgent(directory, 'web');
  b.state.storage.kv.put('config', { owner: 'alice', id: 'b', workspace: null });
  directory.addAgent({ id: 'b', workspace: null, created_by: 'web', display_name: 'reviewer' });
  Object.assign(env, {
    GATEWAYS: { getByName: () => directory },
    AGENTS: { getByName: (name: string) => (JSON.parse(name)[1] === 'b' ? b.agent : a.agent) },
  });
  const settle = async () => {
    for (let i = 0; i < 30; i++) {
      await new Promise((resolve) => setTimeout(resolve, 0));
      await Promise.allSettled(state.background.splice(0));
    }
  };
  const remote = async (path: string, body: unknown, machine = 'laptop') => {
    const res = await worker.fetch(new Request(`https://hosted${path}`, {
      method: 'POST', body: JSON.stringify(body),
      headers: { Authorization: 'Bearer control', 'X-Repowire-Owner': 'alice', 'X-Repowire-From-Machine': machine, 'X-Repowire-Deadline': String(Date.now() + 20_000) },
    }), env as never);
    return { status: res.status, body: await res.json() as Record<string, unknown> };
  };
  const tool = (agent: string, name: string, args: Record<string, unknown> = {}) => directory.call({ id: `c-${Math.random()}`, agent, tool: name, args });
  return { directory, a, b, settle, remote, tool };
}

const realFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = realFetch; });

// A relay with one daemon, "laptop", serving bob in circle managed.
function fakeRelay(handlers: { ask?: (body: Record<string, unknown>) => Response; read?: (id: string) => Response } = {}) {
  const sent: { route: string; body: Record<string, unknown> }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input));
    const body = JSON.parse(String(init?.body ?? '{}'));
    sent.push({ route: url.pathname, body });
    if (url.pathname === '/api/v1/managed/machines') return Response.json({ machines: ['laptop'] });
    const path = body.path as string;
    if (path === '/remote/peers') return Response.json({ peers: [{ peer_id: 'repow-1-bob', display_name: 'bob', circle: 'managed', status: 'online' }], store_epoch: 'e1' });
    if (path === '/remote/ask') return handlers.ask?.(body.body) ?? Response.json({ id: body.body.id, state: 'open', store_epoch: 'e1' });
    if (path === '/remote/notify') return Response.json({ id: body.body.id, state: 'delivered', store_epoch: 'e1' });
    if (path?.startsWith('/remote/asks/')) return handlers.read?.(path.slice('/remote/asks/'.length)) ?? Response.json({ state: 'open', store_epoch: 'e1' });
    if (path === '/remote/settled') return Response.json({ ok: true });
    return Response.json({ detail: 'not found' }, { status: 404 });
  }) as typeof fetch;
  return sent;
}

test('managed to managed: an ask is answered and the reply reaches the asker as a new message', async () => {
  const { directory, a, b, settle, tool } = owner();
  fakeRelay();
  const sent = await tool('a', 'ask', { peer_name: 'reviewer', query: 'what is 6*7?' });
  const id = /ask (ask-\S+) sent to @reviewer@managed/.exec(sent)?.[1];
  assert.ok(id, sent);
  assert.match(b.pi.texts.get(id) ?? '', /<peer-message from="@a@managed" type="ask" correlation-id="ask-/);

  b.pi.settle(id, { status: 'done', text: '42' });
  await settle();
  assert.equal(a.pi.texts.get(`reply-${id}`), `[ack #${id} from @reviewer@managed] 42`);
  assert.equal(directory.outboundRecord(id)?.delivered, 1);
  // A second pass delivers nothing twice.
  await tool('a', 'whoami');
  await settle();
  assert.equal([...a.pi.texts.keys()].filter((op) => op === `reply-${id}`).length, 1);
});

test('an agent cannot message itself, and discovery lists its circle across machines', async () => {
  const { tool } = owner();
  fakeRelay();
  await assert.rejects(tool('a', 'ask', { peer_name: 'a', query: 'hi' }), /itself/);
  const listed = await tool('a', 'list_peers');
  assert.match(listed, /^reviewer\tb\tmanaged/m);
  assert.match(listed, /^bob@laptop\trepow-1-bob\tmanaged/m);
  assert.doesNotMatch(listed, /^a\t/m);
});

test('managed to a daemon: sent through the relay as managed, settled by its push, and refused from another machine', async () => {
  const { directory, a, settle, remote, tool } = owner();
  const sent = fakeRelay();
  const reply = await tool('a', 'ask', { peer_name: 'bob@laptop', query: 'review this' });
  const id = /ask (ask-\S+) sent to @bob@laptop/.exec(reply)?.[1]!;
  const ask = sent.find((call) => call.body.path === '/remote/ask')!;
  assert.deepEqual([ask.body.machine, (ask.body.body as { from: { peer_id: string; circle: string } }).from], ['laptop', { peer_id: 'a', display_name: 'a', circle: 'managed' }]);
  assert.equal((ask.body.body as { expected_epoch: string }).expected_epoch, 'e1');

  assert.equal((await remote('/remote/settled', { id, close_reason: 'ack_with_msg', reply: 'lgtm', store_epoch: 'e1' }, 'desktop')).status, 404);
  assert.equal((await remote('/remote/settled', { id, close_reason: 'ack_with_msg', reply: 'lgtm', store_epoch: 'e1' })).status, 200);
  await settle();
  assert.equal(a.pi.texts.get(`reply-${id}`), `[ack #${id} from @bob@laptop] lgtm`);
  assert.equal(directory.outboundRecord(id)?.state, 'settled');
});

test('an uncertain send is never resent; a later read settles it, and a changed epoch loses the result', async () => {
  const { directory, a, settle, tool } = owner();
  let reads = 0;
  const sent = fakeRelay({
    ask: () => Response.json({ detail: 'machine did not respond in time' }, { status: 504 }),
    read: () => { reads++; return Response.json({ state: 'settled', close_reason: 'declined', reply: 'busy', store_epoch: 'e1' }); },
  });
  const first = await tool('a', 'ask', { peer_name: 'bob@laptop', query: 'q' });
  assert.match(first, /delivery_state=uncertain/);
  const id = /(ask-\S+) delivery_state/.exec(first)![1];
  assert.equal(directory.outboundRecord(id)?.state, 'uncertain');
  assert.equal(sent.filter((call) => call.body.path === '/remote/ask').length, 1);
  await settle();
  assert.ok(reads >= 1);
  assert.equal(a.pi.texts.get(`reply-${id}`), `[declined #${id} from @bob@laptop] busy`);

  fakeRelay({ read: () => Response.json({ state: 'settled', close_reason: 'ack_with_msg', reply: 'x', store_epoch: 'other' }) });
  const second = await tool('a', 'ask', { peer_name: 'bob@laptop', query: 'q2' });
  const lostId = /ask (ask-\S+) sent/.exec(second)![1];
  await settle();
  assert.equal(directory.outboundRecord(lostId)?.close_reason, 'result_lost');
  assert.match(a.pi.texts.get(`reply-${lostId}`) ?? '', /\[closed #ask-\S+ @bob@laptop: result_lost\]/);
});

test('a reply to a retired asker is not submitted, and a bare ack starts no run', async () => {
  const { directory, a, settle, remote, tool } = owner();
  fakeRelay();
  const id = /ask (ask-\S+) sent/.exec(await tool('a', 'ask', { peer_name: 'bob@laptop', query: 'q' }))![1];
  await remote('/remote/settled', { id, close_reason: 'ack', store_epoch: 'e1' });
  await settle();
  assert.equal(directory.outboundRecord(id)?.disposition, 'bare_ack');
  assert.equal(a.pi.texts.has(`reply-${id}`), false);

  const other = /ask (ask-\S+) sent/.exec(await tool('a', 'ask', { peer_name: 'bob@laptop', query: 'q' }))![1];
  directory.retire('a');
  await remote('/remote/settled', { id: other, close_reason: 'ack_with_msg', reply: 'late', store_epoch: 'e1' });
  await settle();
  assert.equal(directory.outboundRecord(other)?.disposition, 'asker_retired');
  assert.equal(a.pi.texts.has(`reply-${other}`), false);
});
