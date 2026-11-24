import assert from 'node:assert/strict';
const base = process.env.HOSTED_URL ?? 'http://localhost:8791';
const token = process.env.HOSTED_TOKEN;
if (!token) throw new Error('Set HOSTED_TOKEN (or use node --env-file=.dev.vars)');
const recallIndex = process.argv.indexOf('--recall');
const recallId = recallIndex >= 0 ? process.argv[recallIndex + 1] : null;
if (recallIndex >= 0 && !recallId) throw new Error('--recall requires an agent ID');
const id = recallId ?? `smoke-${crypto.randomUUID()}`;
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
async function ask(agentId, text) {
  const operationId = crypto.randomUUID();
  await api(`/agents/${agentId}/messages`, { operationId, text }, 202);
  const result = await api(`/agents/${agentId}/operations/${operationId}`);
  assert.equal(result.status, 'done', JSON.stringify(result));
  return result.text;
}
if (recallId) {
  const answer = await ask(id, 'What exact phrase did I ask you to remember earlier? Reply only with the phrase.');
  assert.match(answer, /violet otter 739/i);
  const state = await api(`/agents/${id}`);
  assert.equal(state.busy, false);
  assert.deepEqual(state.pending, []);
  console.log(`PASS recall and idle state; runtime ${state.runtimeId}`);
  process.exit(0);
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

if (process.argv.includes('--workspace')) {
  const workspace = `workspace-${crypto.randomUUID()}`;
  const writer = `writer-${crypto.randomUUID()}`;
  const reader = `reader-${crypto.randomUUID()}`;
  const marker = crypto.randomUUID();
  await api('/agents', { id: writer, workspace }, 201);
  await api('/agents', { id: reader, workspace }, 201);
  await ask(writer, `Use workspace_exec to write the exact text ${marker} to /tmp/repowire-marker. Then stop.`);
  const answer = await ask(reader, 'Use workspace_exec to cat /tmp/repowire-marker. Reply only with the exact file contents.');
  assert.ok(answer.includes(marker), `Workspace restore failed: ${answer}`);
  const entries = JSON.stringify(await api(`/agents/${reader}/messages`));
  assert.ok(entries.includes('workspace_exec'), 'Reader must actually use its workspace tool');
  console.log('PASS two independent agents sharing a workspace across sandbox checkpoint/stop/restore');
}
