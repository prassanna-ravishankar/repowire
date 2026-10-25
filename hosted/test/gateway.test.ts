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
