import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { sharedCircle } from '../src/circles';
import { Gateway } from '../src/gateway';
import { fakeState } from './support/state';

test('sharedCircle agrees with the daemon on every shared vector', () => {
  const vectors: { circle: string; shared: boolean }[] = JSON.parse(readFileSync('../daemon-go/proto/testdata/shared_circles.json', 'utf8'));
  assert.ok(vectors.length > 10);
  for (const { circle, shared } of vectors) assert.equal(sharedCircle(circle), shared, JSON.stringify(circle));
});

test('an agent never changes owner: managed stays managed, a daemon keeps its home', () => {
  const owner = new Gateway(fakeState() as never, {});
  owner.addAgent({ id: 'web', workspace: null, created_by: 'web', circle: 'research' });
  assert.match(String((owner.addAgent({ id: 'web', workspace: null, created_by: 'daemon', home_machine: 'laptop' }) as { conflict?: string }).conflict), /managed/);
  assert.equal(owner.agent('web')?.home_machine, null);
  assert.equal(owner.agent('web')?.circle, 'research');

  owner.addAgent({ id: 'old', workspace: null, created_by: 'daemon' });
  // A daemon may fill the home of its own row created before homes existed.
  assert.equal((owner.addAgent({ id: 'old', workspace: null, created_by: 'daemon', home_machine: 'laptop' }) as { home_machine?: string }).home_machine, 'laptop');
  assert.match(String((owner.addAgent({ id: 'old', workspace: null, created_by: 'daemon', home_machine: 'desktop' }) as { conflict?: string }).conflict), /laptop/);
  assert.match(String((owner.addAgent({ id: 'old', workspace: null, created_by: 'web' }) as { conflict?: string }).conflict), /daemon/);
  // Recreating with the same owner is idempotent.
  assert.equal((owner.addAgent({ id: 'old', workspace: null, created_by: 'daemon', home_machine: 'laptop' }) as { conflict?: string }).conflict, undefined);
});

test('a directory created before agent circles gives every agent the managed circle', () => {
  const state = fakeState();
  state.storage.sql.exec(`CREATE TABLE agents (id TEXT PRIMARY KEY, workspace TEXT, display_name TEXT, description TEXT, home_machine TEXT,
    created_at TEXT NOT NULL, created_by TEXT NOT NULL, retired_at TEXT, state TEXT NOT NULL DEFAULT 'idle', state_at TEXT NOT NULL, last_op TEXT, last_reason TEXT)`);
  state.storage.sql.exec(`INSERT INTO agents (id, created_at, created_by, state_at) VALUES ('a', 'x', 'web', 'x')`);
  const owner = new Gateway(state as never, {});
  assert.equal(owner.agent('a')?.circle, 'managed');
  assert.equal(owner.setCircle('a', 'research')?.circle, 'research');
});

test('a directory created before reply rotation gains delivery_at and still delivers', () => {
  const state = fakeState();
  state.storage.sql.exec(`CREATE TABLE remote_outbound (
    id TEXT PRIMARY KEY, machine TEXT NOT NULL, store_epoch TEXT NOT NULL, to_peer TEXT NOT NULL, to_name TEXT NOT NULL,
    from_agent TEXT NOT NULL, text TEXT NOT NULL, created_at INTEGER NOT NULL, state TEXT NOT NULL,
    close_reason TEXT, reply TEXT, delivered INTEGER NOT NULL DEFAULT 0, disposition TEXT, checked_at INTEGER NOT NULL DEFAULT 0)`);
  state.storage.sql.exec(`INSERT INTO remote_outbound (id, machine, store_epoch, to_peer, to_name, from_agent, text, created_at, state, close_reason, reply)
    VALUES ('ask-1', 'laptop', 'e', 'bob', 'bob', 'a', 'q', 1, 'settled', 'ack_with_msg', 'yes')`);
  const owner = new Gateway(state as never, {});
  assert.deepEqual(owner.claimDeliveries(100, 100, 8).map((row) => row.id), ['ask-1']);
});
