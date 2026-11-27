import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Gateway } from '../src/gateway';
import { markerAge, REMOTE_WINDOW_S, wholeAge } from '../src/retention';
import { hostedAgent } from './support/agent';
import { fakeState } from './support/state';

const DAY_S = 86400;
const DAY = DAY_S * 1000;
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
const CONFIG = { owner: 'alice', id: 'a', workspace: null };

test('markers and whole objects are never kept for less than the window or the ask lifetime', () => {
  assert.equal(markerAge({ content_s: 0, records_s: 0, ask_life_s: 3600 }), 0, 'records kept forever keep the markers');
  assert.equal(markerAge({ content_s: DAY_S, records_s: DAY_S, ask_life_s: 3600 }), REMOTE_WINDOW_S);
  assert.equal(markerAge({ content_s: DAY_S, records_s: 30 * DAY_S, ask_life_s: 3600 }), 30 * DAY_S);
  assert.equal(wholeAge({ content_s: 0, records_s: 0, ask_life_s: 3600 }), 0);
  assert.equal(wholeAge({ content_s: 30 * DAY_S, records_s: 10 * DAY_S, ask_life_s: 3600 }), 10 * DAY_S, 'content goes no later than its record');
  assert.equal(wholeAge({ content_s: DAY_S, records_s: 0, ask_life_s: 3600 }), REMOTE_WINDOW_S);
});

test('an operation\'s markers carry their time and expire with the owner\'s records', async () => {
  const { agent, directory, pi, state } = hostedAgent();
  directory.setRetention({ content_s: 7 * DAY_S, records_s: 30 * DAY_S, ask_life_s: DAY_S });
  await agent.submit('one', 'op-a');
  pi.settle('op-a');
  await flush();
  const kv = state.storage.kv;
  assert.equal(typeof kv.get('reported:op-a'), 'number');
  kv.put('reported:old', Date.now() - 40 * DAY);
  kv.put('undelivered:legacy', true);
  kv.put('undelivered:young', Date.now() - DAY);

  const now = Date.now() + 2 * 3600 * 1000;
  assert.deepEqual(await agent.housekeep(CONFIG, now), { 'agent.markers': 1 });
  assert.equal(kv.get('reported:old'), undefined);
  assert.equal(kv.get('undelivered:legacy'), now, 'a marker from before timestamps starts its age now');
  assert.ok(kv.get('reported:op-a') && kv.get('undelivered:young'));
  assert.equal(await agent.housekeep(CONFIG, now + 60_000), null, 'at most hourly');

  directory.setRetention({ content_s: 0, records_s: 0, ask_life_s: DAY_S });
  assert.deepEqual(await agent.housekeep(CONFIG, now + 2 * 3600 * 1000 + 999 * DAY), {}, 'records kept forever keep every marker');
});

test('only a retired agent with nothing in flight or unreported is deleted whole', async () => {
  const { agent, pi, state } = hostedAgent();
  await agent.submit('one', 'op-a');
  assert.deepEqual(await agent.purge(), { purged: false, reason: 'live' });
  state.storage.kv.put('config', { ...CONFIG, retired: true });
  assert.deepEqual(await agent.purge(), { purged: false, reason: 'pending' });
  pi.settle('op-a');
  await flush();
  assert.deepEqual(await agent.purge(), { purged: true });
  assert.deepEqual(agent.lifecycle.stopped, ['alarms', 'disposed'], 'nothing in memory outlives the storage');
  assert.equal(state.storage.kv.get('reported:op-a'), undefined);
  // Only the fence is left: the id never runs again, and reads say so.
  assert.deepEqual(state.storage.kv.get('config'), { ...CONFIG, retired: true, purged: true });
  assert.deepEqual(await agent.submit('again', 'op-a', undefined), { retired: true });
  assert.deepEqual(await agent.spawn(CONFIG), { created: false, agent: { ...CONFIG, retired: true, purged: true } });
  assert.deepEqual(await agent.operation('op-a'), { status: 'fenced' });
  assert.equal((await agent.outcome('op-a')).reason, 'expired');
  assert.deepEqual(await agent.purge(), { purged: true }, 'a purge of a purged agent is done');
});

test('every marker gets its turn, however many there are', async () => {
  const { agent, directory, state } = hostedAgent();
  directory.setRetention({ content_s: 7 * DAY_S, records_s: 30 * DAY_S, ask_life_s: DAY_S });
  const kv = state.storage.kv;
  const young = Date.now();
  // More young markers than one sweep looks at, ahead of an old one.
  for (let i = 0; i < 2500; i++) kv.put(`reported:a${String(i).padStart(5, '0')}`, young);
  kv.put('reported:zzz-old', young - 40 * DAY);
  let now = Date.now();
  let removed = 0;
  for (let sweep = 0; sweep < 3 && kv.get('reported:zzz-old') !== undefined; sweep++) {
    now += 2 * 3600 * 1000;
    removed += (await agent.housekeep(CONFIG, now))?.['agent.markers'] ?? 0;
  }
  assert.equal(kv.get('reported:zzz-old'), undefined);
  assert.equal(removed, 1);
});

test('the Gateway deletes retired agents and unused workspaces whole, past the age, and keeps their rows', async () => {
  const purged: string[] = [];
  const object = (name: string, done = true) => ({ purge: async () => { purged.push(name); return name.startsWith('ws') ? done : { purged: done }; } });
  const state = fakeState();
  const owner = new Gateway(state as never, {
    AGENTS: { getByName: (name: string) => object(JSON.parse(name)[1]) },
    WORKSPACES: { getByName: (name: string) => object(`ws:${JSON.parse(name)[1]}`) },
  });
  await owner.owner('alice');
  const sql = state.storage.sql;
  const old = new Date(Date.now() - 400 * DAY).toISOString();
  for (const [id, workspace] of [['gone', 'w-old'], ['busy', 'w-old'], ['live', 'w-live'], ['fresh', null]] as const) {
    owner.addAgent({ id, workspace, created_by: 'web' });
  }
  sql.exec('UPDATE agents SET retired_at = ? WHERE id IN (\'gone\', \'busy\')', old);
  sql.exec('UPDATE agents SET retired_at = ? WHERE id = \'fresh\'', new Date().toISOString());
  sql.exec(`INSERT INTO runs (agent, op, started_at, status) VALUES ('busy', 'op', ?, 'working')`, old);
  sql.exec('UPDATE workspaces SET last_exec_at = ?', old);

  assert.deepEqual(await owner.purgeWhole(), { agents: 1, workspaces: 0 });
  assert.deepEqual(purged, ['gone'], 'an open run keeps its agent; a fresh retirement waits; a workspace a live agent uses stays');

  sql.exec(`UPDATE runs SET settled_at = ?, status = 'done'`, old);
  assert.deepEqual(await owner.purgeWhole(), { agents: 1, workspaces: 1 });
  assert.deepEqual(purged, ['gone', 'busy', 'ws:w-old']);
  const rows = Object.fromEntries(owner.agents().map((row) => [row.id, Boolean(row.purged_at)]));
  assert.deepEqual(rows, { gone: true, busy: true, live: false, fresh: false });
  assert.deepEqual(owner.workspaces().map((w) => [w.id, w.state]), [['w-live', 'idle'], ['w-old', 'purged']]);

  owner.addAgent({ id: 'again', workspace: 'w-old', created_by: 'web' });
  assert.equal(owner.workspaces().find((w) => w.id === 'w-old')?.state, 'idle', 'a purged workspace taken up again starts over');

  owner.setRetention({ content_s: 0, records_s: 0, ask_life_s: DAY_S });
  sql.exec('UPDATE agents SET retired_at = ? WHERE id = \'fresh\'', old);
  assert.deepEqual(await owner.purgeWhole(), { agents: 0, workspaces: 0 }, 'content kept forever keeps agents and workspaces');
});

test('a workspace in use again is not purged, whatever the Gateway selected', async () => {
  const state = fakeState();
  const calls: string[] = [];
  const owner = new Gateway(state as never, {
    AGENTS: { getByName: () => ({ purge: async () => {
      // While this agent's purge is awaited, another agent takes w-b up.
      owner.addAgent({ id: 'newcomer', workspace: 'w-b', created_by: 'web' });
      calls.push('agent');
      return { purged: true };
    } }) },
    WORKSPACES: { getByName: (name: string) => ({ purge: async () => { calls.push(JSON.parse(name)[1]); return true; } }) },
  });
  await owner.owner('alice');
  const old = new Date(Date.now() - 400 * DAY).toISOString();
  owner.addAgent({ id: 'gone', workspace: 'w-a', created_by: 'web' });
  state.storage.sql.exec(`INSERT INTO workspaces (id, last_exec_at) VALUES ('w-b', ?)`, old);
  state.storage.sql.exec('UPDATE agents SET retired_at = ?', old);
  state.storage.sql.exec('UPDATE workspaces SET last_exec_at = ?', old);
  await owner.purgeWhole();
  assert.deepEqual(calls, ['agent', 'w-a'], 'w-b was taken up before its turn, so it is not purged');
  assert.equal(owner.workspaces().find((w) => w.id === 'w-b')?.state, 'idle');
});

test('a workspace refuses a purge once a command ran there since the cutoff', async () => {
  const { Workspace } = await import('../src/workspace');
  const state = fakeState();
  const workspace = Object.assign(Object.create(Workspace.prototype), { ctx: { ...state, container: { running: false } }, exclusive: (fn: () => unknown) => fn() });
  await state.storage.put('lastExecAt', Date.now());
  assert.equal(await workspace.purge(Date.now() - DAY), false);
  assert.equal(await workspace.purge(Date.now() + DAY), true);
  assert.equal(await state.storage.get('lastExecAt'), undefined);
});

test('no agent takes a workspace up while it is being deleted', async () => {
  const state = fakeState();
  let attempt: unknown;
  const owner = new Gateway(state as never, {
    AGENTS: { getByName: () => ({ purge: async () => ({ purged: true }) }) },
    WORKSPACES: { getByName: () => ({ purge: async () => {
      // Mid-deletion, an agent tries to take the workspace up.
      attempt = owner.addAgent({ id: 'newcomer', workspace: 'w', created_by: 'web' });
      return true;
    } }) },
  });
  await owner.owner('alice');
  const old = new Date(Date.now() - 400 * DAY).toISOString();
  state.storage.sql.exec(`INSERT INTO workspaces (id, last_exec_at) VALUES ('w', ?)`, old);
  assert.deepEqual(await owner.purgeWhole(), { agents: 0, workspaces: 1 });
  assert.match(String((attempt as { conflict?: string }).conflict), /being deleted/);
  assert.equal(owner.agent('newcomer'), null);
  // Once the deletion ends, the workspace is taken up again, and starts empty.
  assert.ok(!('conflict' in (owner.addAgent({ id: 'newcomer', workspace: 'w', created_by: 'web' }) ?? {})));
  assert.equal(owner.workspaces().find((w) => w.id === 'w')?.state, 'idle');
});

test('a running agent the directory has not seen cannot run a command in a workspace being deleted', () => {
  const state = fakeState();
  const owner = new Gateway(state as never, {});
  state.storage.sql.exec(`INSERT INTO workspaces (id, purging_at) VALUES ('w', ?)`, new Date().toISOString());
  assert.throws(() => owner.execStarted('w', 'legacy'), /being deleted/);
  assert.equal(owner.agent('legacy'), null);
  assert.equal(owner.workspaces()[0].state, 'idle', 'nothing counted as running');
  // Once the deletion ends, the agent is added and its command counted, and
  // a running workspace cannot be claimed for deletion.
  state.storage.sql.exec('UPDATE workspaces SET purging_at = NULL');
  owner.execStarted('w', 'legacy');
  assert.equal(owner.agent('legacy')?.workspace, 'w');
  assert.equal(owner.workspaces()[0].state, 'running');
});

// A sandbox is an owner-level computer: deleted on purpose, not only by
// retention. A live agent using it, or a running container, keeps it.
test('an owner deletes a workspace no live agent uses; a running one, or one in use, is kept', async () => {
  const state = fakeState();
  let running = true;
  const purged: string[] = [];
  const owner = new Gateway(state as never, {
    AGENTS: { getByName: () => ({}) },
    WORKSPACES: { getByName: (name: string) => ({ purge: async () => {
      if (running) return false;
      purged.push(JSON.parse(name)[1]);
      return true;
    } }) },
  });
  await owner.owner('alice');
  owner.addAgent({ id: 'live', workspace: 'w', created_by: 'web' });
  assert.deepEqual(await owner.deleteWorkspace('w'), { deleted: false, reason: 'in_use', agents: ['live'] });
  owner.retire('live');
  // A command a retired agent started is still running in the directory's view.
  owner.execStarted('w', 'live');
  assert.deepEqual(await owner.deleteWorkspace('w'), { deleted: false, reason: 'running' });
  owner.execFinished('live', 'w', 'c1', { wall_ms: 1, snapshot_at: null, dirty: false });
  assert.deepEqual(await owner.deleteWorkspace('w'), { deleted: false, reason: 'running' });
  running = false;
  assert.deepEqual(await owner.deleteWorkspace('w'), { deleted: true });
  assert.deepEqual(purged, ['w']);
  assert.equal(owner.workspaces().find((w) => w.id === 'w')?.state, 'purged');
  assert.deepEqual(await owner.deleteWorkspace('nope'), { deleted: false, reason: 'not_found' });
});
