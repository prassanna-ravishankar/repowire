import { test } from 'node:test';
import assert from 'node:assert/strict';
import { authenticate, body, identifier, scopedName, source } from '../src/http';

test('private routes fail closed, including when configuration is missing', async () => {
  const request = new Request('https://example.test');
  await assert.rejects(authenticate(request), { status: 503 });
  await assert.rejects(authenticate(request, 'secret'), { status: 401 });
  await assert.rejects(authenticate(new Request(request, { headers: { Authorization: 'Bearer wrong' } }), 'secret'), { status: 401 });
  await authenticate(new Request(request, { headers: { Authorization: 'Bearer secret' } }), 'secret');
});

test('tenant and agent components cannot collide or inject paths', () => {
  assert.notEqual(scopedName('a:b', 'c'), scopedName('a', 'b:c'));
  assert.notEqual(scopedName('alice', 'agent'), scopedName('bob', 'agent'));
  for (const value of ['../other', 'a/b', '', null, 'x'.repeat(81)]) {
    assert.throws(() => identifier(value, 'id'), { status: 400 });
  }
});

test('body limits apply without Content-Length', async () => {
  const request = (value: string) => new Request('https://example.test', { method: 'POST', body: value });
  await assert.rejects(body(request('x'.repeat(65537))), { status: 413 });
  for (const value of ['null', '[]', '{broken']) await assert.rejects(body(request(value)), { status: 400 });
  assert.deepEqual(await body(request('{"text":"hello"}')), { text: 'hello' });
});

test('an optional body is empty or a valid object, never a parse failure', async () => {
  const request = (value?: string) => new Request('https://example.test', { method: 'POST', body: value });
  assert.deepEqual(await body(request(), true), {});
  assert.deepEqual(await body(request(''), true), {});
  for (const value of ['{broken', 'null', '[]']) await assert.rejects(body(request(value), true), { status: 400 });
  await assert.rejects(body(request('x'.repeat(65537)), true), { status: 413 });
  assert.deepEqual(await body(request('{"operationId":"op-1"}'), true), { operationId: 'op-1' });
});

test('mesh source is optional, bounded, and requires a sender and type', () => {
  assert.equal(source(undefined), undefined);
  assert.deepEqual(source({ from: 'planner', type: 'ask', correlationId: 'ask-1' }),
    { from: 'planner', type: 'ask', correlationId: 'ask-1' });
  for (const value of ['x', [], { type: 'ask' }, { from: 'a', type: 'x'.repeat(201) }]) {
    assert.throws(() => source(value), { status: 400 });
  }
});

test('callers: the daemon token is bound to its owner, the control token names one or reaches admin', async () => {
  const { authorize } = await import('../src/http');
  const env = { HOSTED_TOKEN: 'daemon-secret', HOSTED_OWNER: 'owner-a', CONTROL_TOKEN: 'control-secret' };
  const call = (token: string, owner?: string) => new Request('https://example.test', {
    headers: { Authorization: `Bearer ${token}`, ...(owner ? { 'X-Repowire-Owner': owner } : {}) },
  });
  assert.deepEqual(await authorize(call('daemon-secret', 'owner-b'), env), { role: 'daemon', owner: 'owner-a' });
  assert.deepEqual(await authorize(call('control-secret', 'token-abc'), env), { role: 'control', owner: 'token-abc' });
  assert.deepEqual(await authorize(call('control-secret'), env), { role: 'control', owner: null });
  await assert.rejects(authorize(call('control-secret', '../x'), env), { status: 400 });
  await assert.rejects(authorize(call('wrong'), env), { status: 401 });
  await assert.rejects(authorize(call('x'), {}), { status: 503 });
});
