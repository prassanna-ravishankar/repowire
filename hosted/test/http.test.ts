import { test } from 'node:test';
import assert from 'node:assert/strict';
import { authenticate, body, identifier, scopedName } from '../src/http';

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
