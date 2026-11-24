import { test } from 'node:test';
import assert from 'node:assert/strict';
import { admission, deriveState, meshWindow, utcDay } from '../src/ledger';
import { execCost, LIMITS, modelCost } from '../src/prices';

test('agent state: retired wins, then working, then the last settled run', () => {
  assert.equal(deriveState(true, 2, { status: 'unanswered', reason: 'run_failed' }), 'retired');
  assert.equal(deriveState(false, 1, { status: 'unanswered', reason: 'run_failed' }), 'working');
  assert.equal(deriveState(false, 0, { status: 'unanswered', reason: 'run_failed' }), 'failed');
  assert.equal(deriveState(false, 0, { status: 'unanswered', reason: 'aborted' }), 'idle');
  assert.equal(deriveState(false, 0, { status: 'done', reason: null }), 'idle');
  assert.equal(deriveState(false, 0), 'idle');
});

test('admission refuses at the first exhausted budget and names it', () => {
  assert.equal(admission({ ownerDay: 0.5, ownerMonth: 5, agentDay: 0.2 }), null);
  assert.match(admission({ ownerDay: 2, ownerMonth: 5, agentDay: 0.2 })!, /^budget_exceeded: owner spent \$2\.00 of \$2\.00/);
  assert.match(admission({ ownerDay: 1, ownerMonth: 20, agentDay: 0.2 })!, /owner this month/);
  assert.match(admission({ ownerDay: 1, ownerMonth: 5, agentDay: 1 })!, /^budget_exceeded: agent/);
});

test('mesh loop guard keeps a sliding window', () => {
  const now = 1_000_000_000;
  const full = Array.from({ length: LIMITS.meshMessagesPerWindow }, (_, i) => now - i);
  assert.equal(meshWindow(full, now), null);
  const stale = full.map((at) => at - LIMITS.meshWindowMs);
  assert.deepEqual(meshWindow(stale, now), [now]);
});

test('prices: Pi cost wins, the table is the fallback, commands cost cents per thousand', () => {
  assert.equal(modelCost({ input: 1e6, cacheRead: 0, output: 0, cost: { total: 0.42 } }), 0.42);
  assert.ok(Math.abs(modelCost({ input: 1e6, cacheRead: 1e6, output: 1e6 }) - 0.68) < 1e-9);
  const command = execCost(15_000);
  assert.ok(command > 0.00002 && command < 0.00005, `15s command costs ${command}`);
  assert.equal(utcDay(Date.UTC(2026, 9, 25, 23, 59)), '2026-10-25');
});
