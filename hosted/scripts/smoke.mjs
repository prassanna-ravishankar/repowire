import assert from 'node:assert/strict';
const base = process.env.HOSTED_URL ?? 'http://localhost:8791';
const token = process.env.HOSTED_TOKEN;
if (!token) throw new Error('Set HOSTED_TOKEN (or use node --env-file=.dev.vars)');
const id = `smoke-${crypto.randomUUID()}`;
async function api(path, data, expected = 200, authenticated = true) {
  const response = await fetch(`${base}${path}`, {
    method: data ? 'POST' : 'GET',
    headers: { ...(authenticated ? { Authorization: `Bearer ${token}` } : {}), 'Content-Type': 'application/json' },
    ...(data ? { body: JSON.stringify(data) } : {}),
    signal: AbortSignal.timeout(120000),
  });
  const result = await response.json();
  assert.equal(response.status, expected, JSON.stringify(result));
  return result;
}
await api('/health', null, 200, false);
await api('/agents', { id }, 401, false);
assert.equal((await api('/agents', { id }, 201)).created, true);
assert.equal((await api('/agents', { id })).created, false);
await api('/agents', { id, workspace: 'different' }, 409);
const state = await api(`/agents/${id}`);
assert.equal(state.agent.id, id);
assert.equal(state.busy, false);
assert.deepEqual(await api(`/agents/${id}/messages`), []);
console.log('PASS health, auth, spawn, idempotent spawn, immutable workspace, status, transcript');
if (process.argv.includes('--model')) {
  const operationId = 'remember';
  const text = 'Remember the exact secret phrase violet otter 739. Reply with only that phrase.';
  const receipt = await api(`/agents/${id}/messages`, { operationId, text }, 202);
  assert.equal(receipt.accepted, true);
  assert.equal((await api(`/agents/${id}/messages`, { operationId, text }, 202)).accepted, false);
  const answer = await api(`/agents/${id}/operations/${operationId}`);
  assert.equal(answer.status, 'done', JSON.stringify(answer));
  assert.match(answer.text, /violet otter 739/i);
  console.log('PASS durable submit, duplicate rejection, model reply');
  console.log(`Agent for restart/recall verification: ${id}`);
}
