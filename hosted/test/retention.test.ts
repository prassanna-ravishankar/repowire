import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { Gateway } from '../src/gateway';
import { cutoffs, DEFAULT_POLICY, REMOTE_WINDOW_S } from '../src/retention';
import { fakeState, setPolicy } from './support/state';

const DAY = 24 * 3600 * 1000;
const NOW = Date.parse('2026-11-24T12:00:00.000Z');
const ago = (ms: number) => new Date(NOW - ms).toISOString();
const agoS = (ms: number) => Math.floor((NOW - ms) / 1000);

function directory() {
  const state = fakeState();
  const owner = new Gateway(state as never, {});
  const sql = state.storage.sql;
  const rows = (query: string, ...args: (string | number | null)[]) => (sql.exec(query, ...args).toArray() as Record<string, unknown>[]).map((row) => ({ ...row }));
  return { owner, sql, rows };
}

// Every table declares its retention beside its queries, or why it is exempt
// (as TestEveryTableDeclaresRetention does for the daemon's store).
test('every Gateway table declares its retention exactly once', () => {
  const { rows } = directory();
  const tables = rows("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").map((row) => row.name as string);
  const declared = Gateway.retention.flatMap((d) => d.tables);
  for (const table of tables) {
    assert.equal(declared.filter((name) => name === table).length, 1, `table ${table} needs exactly one retention declaration`);
  }
  for (const name of declared) assert.ok(tables.includes(name), `retention declared for ${name}, which is not a table`);
});

test('the defaults are the daemon\'s own file', () => {
  const shared = JSON.parse(readFileSync('../daemon-go/proto/retention.json', 'utf8'));
  assert.deepEqual(DEFAULT_POLICY, { content_s: shared.content_days * 86400, records_s: shared.records_days * 86400, ask_life_s: shared.ask_life_hours * 3600 });
  assert.equal(REMOTE_WINDOW_S, shared.remote_window_days * 86400);
});

test('a sweep strips content, then deletes records, and keeps open work and the ledger', () => {
  const { owner, sql, rows } = directory();
  const run = (op: string, started: string, settled: string | null) => {
    sql.exec(`INSERT INTO runs (agent, op, started_at, settled_at, status, answer) VALUES ('a', ?, ?, ?, ?, 'the answer')`, op, started, settled, settled ? 'done' : 'working');
    sql.exec(`INSERT INTO requests (id, agent, kind, requester, text, digest, created_at, state) VALUES (?, 'a', 'ask', 'r', 'the question', 'h', ?, 'accepted')`, op, started);
  };
  run('fresh', ago(DAY), ago(DAY));
  run('stale', ago(10 * DAY), ago(10 * DAY));
  run('old', ago(200 * DAY), ago(200 * DAY));
  run('open', ago(300 * DAY), null);
  sql.exec(`INSERT INTO requests (id, agent, kind, requester, text, digest, created_at, state, settled_at)
    VALUES ('lost', 'a', 'ask', 'r', 'never arrived', 'h', ?, 'not_delivered', ?)`, ago(10 * DAY), ago(10 * DAY));
  // No run yet: still pending, whatever its age, until its reader reconciles it.
  sql.exec(`INSERT INTO requests (id, agent, kind, requester, text, digest, created_at, state) VALUES ('unrun', 'a', 'ask', 'r', 'on its way', 'h', ?, 'submitting')`, ago(300 * DAY));
  // Made before digests: its text stays, so a retry can still be matched.
  sql.exec(`INSERT INTO runs (agent, op, started_at, settled_at, status) VALUES ('a', 'legacy', ?, ?, 'done')`, ago(10 * DAY), ago(10 * DAY));
  sql.exec(`INSERT INTO requests (id, agent, kind, requester, text, created_at, state) VALUES ('legacy', 'a', 'ask', 'r', 'kept', ?, 'accepted')`, ago(10 * DAY));
  sql.exec(`INSERT INTO usage_event (id, at, day, agent, kind, cost_usd) VALUES ('u', ?, '2025-10-01', 'a', 'model', 1)`, ago(400 * DAY));
  sql.exec(`INSERT INTO usage_day (day, agent, kind, events, cost_usd) VALUES ('2025-10-01', 'a', 'model', 1, 1)`);

  const counts = owner.housekeep(NOW);
  assert.deepEqual(counts, { 'runs.delete': 1, 'requests.delete': 1, 'runs.strip': 1, 'requests.strip': 2 });
  const answers = Object.fromEntries(rows('SELECT op, answer FROM runs').map((row) => [row.op, row.answer]));
  assert.deepEqual(answers, { fresh: 'the answer', stale: null, open: 'the answer', legacy: null });
  const texts = Object.fromEntries(rows('SELECT id, text FROM requests').map((row) => [row.id, row.text]));
  assert.deepEqual(texts, { fresh: 'the question', stale: '', open: 'the question', lost: '', unrun: 'on its way', legacy: 'kept' });
  assert.equal(rows('SELECT * FROM usage_event').length, 1);
  assert.equal(rows('SELECT * FROM usage_day').length, 1);
  // An open request is still listed whole for its requester.
  assert.deepEqual(owner.openRequests('r').map((row: Record<string, unknown>) => row.id), ['open', 'unrun']);
});

test('sweeps run at most hourly, on the request that finds one due', () => {
  const { owner, sql, rows } = directory();
  assert.ok(owner.housekeep(NOW));
  sql.exec(`INSERT INTO runs (agent, op, started_at, settled_at, status) VALUES ('a', 'x', ?, ?, 'done')`, ago(400 * DAY), ago(400 * DAY));
  assert.equal(owner.housekeep(NOW + 59 * 60 * 1000), null);
  assert.equal(rows('SELECT * FROM runs').length, 1);
  assert.deepEqual(owner.housekeep(NOW + 61 * 60 * 1000), { 'runs.delete': 1 });
});

test('cross-machine records keep the protocol window whatever the policy', () => {
  const { owner, sql, rows } = directory();
  setPolicy(owner, { content_s: 0, records_s: 0, ask_life_s: 0 });
  for (const [id, age] of [['old', 8 * DAY], ['live', 6 * DAY]] as const) {
    sql.exec(`INSERT INTO remote_fences (id, from_machine, from_peer, created_at) VALUES (?, 'm', 'p', ?)`, id, agoS(age));
    sql.exec(`INSERT INTO remote_inbound (id, kind, from_machine, from_peer, from_name, from_circle, to_agent, text, created_at, state)
      VALUES (?, 'ask', 'm', 'p', 'n', 'c', 'a', 't', ?, 'open')`, id, agoS(age));
  }
  owner.housekeep(NOW);
  assert.deepEqual(rows('SELECT id FROM remote_inbound').map((row) => row.id), ['live']);
  assert.deepEqual(rows('SELECT id FROM remote_fences').map((row) => row.id), ['live']);
});

test('the owner\'s policy governs, with nothing younger than the ask lifetime expiring', () => {
  const { owner, sql, rows } = directory();
  setPolicy(owner, { content_s: 60, records_s: 0, ask_life_s: 6 * 3600 });
  for (const [op, age] of [['young', 2 * 3600 * 1000], ['past', 7 * 3600 * 1000]] as const) {
    sql.exec(`INSERT INTO runs (agent, op, started_at, settled_at, status, answer) VALUES ('a', ?, ?, ?, 'done', 'text')`, op, ago(age), ago(age));
  }
  sql.exec(`INSERT INTO runs (agent, op, started_at, settled_at, status) VALUES ('a', 'ancient', ?, ?, 'done')`, ago(999 * DAY), ago(999 * DAY));
  assert.deepEqual(owner.housekeep(NOW), { 'runs.strip': 1 });
  const answers = Object.fromEntries(rows('SELECT op, answer FROM runs').map((row) => [row.op, row.answer]));
  assert.deepEqual(answers, { young: 'text', past: null, ancient: null }, 'records kept forever, content past the floor stripped');
});

test('content goes no later than its record', () => {
  const c = cutoffs({ content_s: 30 * 86400, records_s: 10 * 86400, ask_life_s: 3600 }, NOW);
  assert.equal(c.content?.iso, c.records?.iso);
  assert.equal(cutoffs({ content_s: 0, records_s: 0, ask_life_s: 3600 }, NOW).content, null);
});

test('without a policy of its own an owner gets the shared defaults; one it sets replaces them', () => {
  const { owner } = directory();
  assert.deepEqual(owner.retentionPolicy(), DEFAULT_POLICY);
  const policy = { content_s: 3 * 86400, records_s: 30 * 86400, ask_life_s: 7200 };
  setPolicy(owner, policy);
  assert.deepEqual(owner.retentionPolicy(), policy);
});

test('a request outlives an ask that ran past the records window, timed from when it settled', () => {
  const { owner, sql, rows } = directory();
  setPolicy(owner, { content_s: 7 * 86400, records_s: 30 * 86400, ask_life_s: 86400 });
  sql.exec(`INSERT INTO runs (agent, op, started_at, settled_at, status, answer) VALUES ('a', 'long', ?, ?, 'done', '42')`, ago(90 * DAY), ago(60 * 1000));
  sql.exec(`INSERT INTO requests (id, agent, kind, requester, text, digest, created_at, state) VALUES ('long', 'a', 'ask', 'r', 'q', 'h', ?, 'accepted')`, ago(90 * DAY));
  assert.deepEqual(owner.housekeep(NOW), {});
  assert.deepEqual(rows('SELECT id, text FROM requests'), [{ id: 'long', text: 'q' }]);
  assert.deepEqual(rows('SELECT answer FROM runs'), [{ answer: '42' }]);
});

// A Gateway from before managed agents were reached only through the relay
// kept one policy per daemon that connected. The first policy read takes the
// most conservative of them as the owner's, so no deployed data goes sooner.
test('per-daemon policies from before become the owner policy, the most conservative of them', () => {
  const state = fakeState();
  state.storage.kv.put('retention:policy:laptop', { content_s: 0, records_s: 30 * 86400, ask_life_s: 3600 });
  state.storage.kv.put('retention:policy:desktop', { content_s: 3 * 86400, records_s: 10 * 86400, ask_life_s: 7200 });
  const owner = new Gateway(state as never, {});
  assert.deepEqual(owner.retentionPolicy(), { content_s: 0, records_s: 30 * 86400, ask_life_s: 7200 });
  assert.equal(state.storage.kv.list({ prefix: 'retention:policy:' }).length, 0, 'the per-daemon keys are folded in once');
  assert.deepEqual(owner.retentionPolicy(), { content_s: 0, records_s: 30 * 86400, ask_life_s: 7200 });
});

// The daemon's report outbox and its connection list have no reader any
// more. A sweep deletes them in bounded batches, so a run's answer never
// outlives retention in a stale copy.
test('a sweep deletes the old daemon outbox and connection list, a batch at a time', () => {
  const state = fakeState();
  for (let i = 0; i < 1200; i++) state.storage.kv.put(`outbox:a:op-${String(i).padStart(4, '0')}`, { text: 'answer' });
  state.storage.kv.put('clients:recent', [{ kind: 'daemon' }]);
  const owner = new Gateway(state as never, {});
  owner.housekeep(NOW);
  assert.ok(state.storage.kv.list({ prefix: 'outbox:' }).length < 1200, 'a sweep removes a batch');
  assert.equal(state.storage.kv.get('clients:recent'), undefined);
  for (let hour = 1; hour <= 5; hour++) owner.housekeep(NOW + hour * 3600_000);
  assert.equal(state.storage.kv.list({ prefix: 'outbox:' }).length, 0);
});
