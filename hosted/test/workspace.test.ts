import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Workspace, type Backups } from '../src/workspace';
import { fakeState } from './support/state';

// A container that runs commands as files in a map, and backups that copy
// that map to "R2", so the tests see what a restart keeps.
function workspace() {
  const state = fakeState();
  const files = new Map<string, string>();
  const r2 = new Map<string, Map<string, string>>();
  const container = {
    running: false, starts: 0, idle: 0, envs: [] as Record<string, string>[],
    images: { sandbox: 'sandbox-image' },
    start() { this.running = true; this.starts++; files.clear(); },
    async setInactivityTimeout(ms: number) { this.idle = ms; },
    execs: [] as string[][],
    async exec(argv: string[], options: { cwd?: string; env?: Record<string, string> } = {}) {
      this.execs.push(argv);
      if (argv[0] === 'mkdir') return { output: async () => ({ stdout: new Uint8Array(), stderr: new Uint8Array(), exitCode: 0 }) };
      this.envs.push(options.env ?? {});
      const script = argv.at(-1)!;
      const write = /^echo (\S+) > (\S+)$/.exec(script);
      if (write) files.set(write[2], write[1]);
      const read = /^cat (\S+)$/.exec(script);
      const stdout = read ? files.get(read[1]) ?? '' : '';
      return { output: async () => ({ stdout: new TextEncoder().encode(stdout), stderr: new Uint8Array(), exitCode: read && !files.has(read[1]) ? 1 : 0 }) };
    },
    stop() { this.running = false; },
    async destroy() { this.running = false; this.destroyed++; },
    destroyed: 0,
  };
  let failBackup = false;
  let failRestore = false;
  let nextId = 0;
  const backups: Backups = {
    backup: async () => {
      if (failBackup) throw new Error('R2 unreachable');
      const id = `b${++nextId}`;
      r2.set(id, new Map(files));
      return { id, dir: '/workspace' } as never;
    },
    restore: async (record) => {
      if (failRestore) throw new Error('restore failed');
      for (const [k, v] of r2.get((record as { id: string }).id)!) files.set(k, v);
    },
    delete: async (record) => { r2.delete((record as { id: string }).id); },
  };
  Object.assign(state, { container });
  const open = () => {
    const ws = new Workspace(state as never, {} as never);
    Object.defineProperty(ws, 'backups', { value: () => backups });
    return ws;
  };
  return { ws: open(), reopen: open, container, r2, state, failBackup: (value: boolean) => { failBackup = value; }, failRestore: (value: boolean) => { failRestore = value; } };
}

test('a command runs in the agent\'s name, and the container stays up between commands', async () => {
  const { ws, container } = workspace();
  const first = await ws.exec('echo hello > notes.txt', 'alice');
  assert.equal(first.ok, true);
  assert.equal((await ws.exec('cat notes.txt', 'bob')).result?.stdout, 'hello');
  assert.equal(container.starts, 1, 'one container for both commands');
  assert.deepEqual(container.envs, [{ REPOWIRE_AGENT: 'alice' }, { REPOWIRE_AGENT: 'bob' }]);
  assert.equal(container.idle, 30 * 60_000);
});

test('a turn end saves the files once; a restarted container restores them', async () => {
  const { ws, container, r2 } = workspace();
  await ws.exec('echo v1 > f', 'a');
  assert.equal((await ws.checkpoint()).saved, true);
  assert.equal((await ws.checkpoint()).saved, false, 'nothing changed since the save');
  assert.equal(container.idle, 2 * 60_000, 'a saved workspace stops soon');
  await ws.exec('echo v2 > f', 'a');
  await ws.checkpoint();
  assert.equal(r2.size, 1, 'the new save replaces the old one');
  container.stop();
  const read = await ws.exec('cat f', 'a');
  assert.equal(read.result?.stdout, 'v2');
  assert.equal(read.lost, undefined);
  assert.ok(read.snapshot_at);
});

test('changes a stopped container never saved are reported as lost, and work carries on from the last save', async () => {
  const { ws, container } = workspace();
  await ws.exec('echo kept > f', 'a');
  await ws.checkpoint();
  await ws.exec('echo unsaved > f', 'a');
  container.stop();
  const read = await ws.exec('cat f', 'a');
  assert.equal(read.lost, true);
  assert.equal(read.result?.stdout, 'kept');
  assert.equal((await ws.exec('cat f', 'a')).lost, undefined, 'reported once');
});

test('a failed save keeps the changes for the next save and marks the workspace dirty', async () => {
  const { ws, failBackup } = workspace();
  await ws.exec('echo x > f', 'a');
  failBackup(true);
  await assert.rejects(ws.checkpoint(), /R2 unreachable/);
  assert.equal((await ws.exec('cat f', 'a')).dirty, true);
  failBackup(false);
  assert.equal((await ws.checkpoint()).saved, true);
  assert.equal((await ws.exec('cat f', 'a')).dirty, false);
});

test('a purge deletes the saved copy and the workspace\'s storage, never while it runs', async () => {
  const { ws, container, r2, state } = workspace();
  await ws.exec('echo x > f', 'a');
  await ws.checkpoint();
  assert.equal(await ws.purge(Date.now() + 1000), false, 'a running container is kept');
  container.stop();
  assert.equal(await ws.purge(0), false, 'a command since the cutoff keeps it');
  assert.equal(await ws.purge(Date.now() + 1000), true);
  assert.equal(r2.size, 0);
  assert.equal(state.storage.kv.get('saved'), undefined);
});

// /workspace lives in the container's writable layer, never in the image:
// restoring swaps the directory in, and overlayfs cannot rename a directory
// an image layer holds. A fresh container restores it, or creates it empty.
test('a fresh container creates /workspace itself when there is nothing to restore', async () => {
  const { ws, container } = workspace();
  await ws.exec('echo x > f', 'a');
  assert.deepEqual(container.execs[0], ['mkdir', '-p', '/workspace']);
  await ws.checkpoint();
  container.stop();
  container.execs.length = 0;
  await ws.exec('cat f', 'a');
  assert.equal(container.execs.some((argv) => argv[0] === 'mkdir'), false, 'a restore creates it');
});

test('a restore that fails leaves no container behind, so the next command restores again', async () => {
  const { ws, container, failRestore } = workspace();
  await ws.exec('echo kept > f', 'a');
  await ws.checkpoint();
  container.stop();
  failRestore(true);
  const failed = await ws.exec('cat f', 'a');
  assert.equal(failed.ok, false);
  assert.equal(container.running, false, 'the half-restored container is gone');
  failRestore(false);
  assert.equal((await ws.exec('cat f', 'a')).result?.stdout, 'kept');
});

test('a save after the container stopped reports the unsaved changes as lost', async () => {
  const { ws, container } = workspace();
  await ws.exec('echo x > f', 'a');
  container.stop();
  const result = await ws.checkpoint();
  assert.equal(result.saved, false);
  assert.equal(result.lost, true);
  assert.equal(result.dirty, true);
});

// A restarted object cannot know whether work it had started in the container
// finished, so it starts the container over from the last save and says so.
// A container that simply outlived an idle eviction is kept.
test('after an object restart, a container mid-command is replaced; an idle one is kept', async () => {
  const { ws, reopen, container, state } = workspace();
  await ws.exec('echo kept > f', 'a');
  await ws.checkpoint();
  await ws.exec('echo later > f', 'a');
  const idle = reopen();
  assert.equal((await idle.exec('cat f', 'a')).result?.stdout, 'later', 'an idle container is adopted');
  assert.equal(container.destroyed, 0);
  state.storage.kv.put('busy', 'exec');
  const cut = reopen();
  const read = await cut.exec('cat f', 'a');
  assert.equal(container.destroyed, 1);
  assert.equal(read.lost, true);
  assert.equal(read.result?.stdout, 'kept');
});

test('files from before directory backups are reported as not carried over, once', async () => {
  const { ws, state } = workspace();
  state.storage.kv.put('snapshotId', 'old-snapshot');
  const first = await ws.exec('ls', 'a');
  assert.equal(first.legacy, true);
  assert.equal(state.storage.kv.get('snapshotId'), undefined);
  assert.equal((await ws.exec('ls', 'a')).legacy, undefined);
});
