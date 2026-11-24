import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Gateway } from '../src/gateway';
import { fakeState } from './support/state';

const gateway = (state = fakeState()) => new Gateway(state as never, {});

test('a workspace whose checkpoint failed needs repair and keeps its last real snapshot', () => {
  const owner = gateway();
  owner.addAgent({ id: 'a', workspace: 'w', created_by: 'web' });
  owner.execStarted('w');
  owner.execFinished('a', 'w', 'call-1', { wall_ms: 1000, dirty: false, snapshot_at: '2026-10-25T10:00:00.000Z' });
  assert.deepEqual(pick(owner.workspaces()[0]), { state: 'idle', snapshot_at: '2026-10-25T10:00:00.000Z', container_ms: 1000 });
  owner.execStarted('w');
  owner.execFinished('a', 'w', 'call-2', { wall_ms: 500, dirty: true, snapshot_at: '2026-10-25T10:00:00.000Z' });
  assert.deepEqual(pick(owner.workspaces()[0]), { state: 'needs_repair', snapshot_at: '2026-10-25T10:00:00.000Z', container_ms: 1500 });
  // An agent that could not hear back from the workspace leaves its state alone.
  owner.execStarted('w');
  owner.execFinished('a', 'w', 'call-3', { wall_ms: 200, dirty: null, snapshot_at: null });
  assert.deepEqual(pick(owner.workspaces()[0]), { state: 'needs_repair', snapshot_at: '2026-10-25T10:00:00.000Z', container_ms: 1700 });
});

test('container time is metered once per call, as the workspace measured it', () => {
  const owner = gateway();
  owner.addAgent({ id: 'a', workspace: 'w', created_by: 'web' });
  for (const call of ['call-1', 'call-2', 'call-1']) {
    owner.execStarted('w');
    owner.execFinished('a', 'w', call, { wall_ms: 45_000, dirty: false, snapshot_at: null });
  }
  const [day] = owner.usage(1);
  assert.equal(day.execs, 2);
  assert.equal(day.container_ms, 90_000);
});

test('a directory created before the workspace columns gains them', () => {
  const state = fakeState();
  state.storage.sql.exec(`CREATE TABLE workspaces (id TEXT PRIMARY KEY, exec_count INTEGER NOT NULL DEFAULT 0,
    container_ms INTEGER NOT NULL DEFAULT 0, last_exec_at TEXT, running INTEGER NOT NULL DEFAULT 0)`);
  const owner = gateway(state);
  gateway(state);
  owner.addAgent({ id: 'a', workspace: 'w', created_by: 'web' });
  assert.equal(owner.workspaces()[0].state, 'idle');
});

function pick(workspace: { state: string; snapshot_at: unknown; container_ms: number }) {
  return { state: workspace.state, snapshot_at: workspace.snapshot_at, container_ms: workspace.container_ms };
}

test('a run settles whether its start or its report arrives first', async () => {
  const owner = gateway();
  owner.addAgent({ id: 'a', workspace: null, created_by: 'daemon' });
  const usage = { input: 10, cacheRead: 0, output: 5, cost: { total: 0.01 } };

  owner.runStarted('a', 'op-1', 'planner');
  await owner.report({ agent: 'a', operationId: 'op-1', status: 'done', text: 'hi', usage });
  // The report arrives first: it creates the row, and the late start keeps it settled.
  await owner.report({ agent: 'a', operationId: 'op-2', status: 'done', usage, source: 'planner', startedAt: '2026-10-25T09:00:00.000Z' });
  owner.runStarted('a', 'op-2', 'planner');
  // A resent report changes nothing.
  await owner.report({ agent: 'a', operationId: 'op-2', status: 'unanswered', reason: 'aborted', usage });

  const runs = Object.fromEntries(owner.runs('a').map((run) => [run.op, run]));
  assert.equal(runs['op-1'].status, 'done');
  assert.equal(runs['op-2'].status, 'done');
  assert.equal(runs['op-2'].source, 'planner');
  assert.equal(runs['op-2'].started_at, '2026-10-25T09:00:00.000Z');
  assert.ok(runs['op-2'].settled_at);
  assert.equal(owner.agent('a')?.state, 'idle');
  assert.equal(owner.usage(1)[0].runs, 2);
});

test('a withdrawn start leaves no working run behind, and never removes a settled one', async () => {
  const owner = gateway();
  owner.addAgent({ id: 'a', workspace: null, created_by: 'daemon' });
  owner.runStarted('a', 'op-1', null);
  assert.equal(owner.agent('a')?.state, 'working');
  owner.runWithdrawn('a', 'op-1');
  assert.equal(owner.runs('a').length, 0);
  assert.equal(owner.agent('a')?.state, 'idle');
  await owner.report({ agent: 'a', operationId: 'op-2', status: 'done' });
  owner.runWithdrawn('a', 'op-2');
  assert.equal(owner.runs('a').length, 1);
});
