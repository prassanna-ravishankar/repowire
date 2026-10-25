import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Gateway } from '../src/gateway';
import { hostedAgent } from './support/agent';

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
const response = (cost: number) => ({ input: 1, output: 1, cacheRead: 0, cost: { total: cost } });
const ledger = (directory: Gateway) => directory.usage(1)[0]?.model_usd ?? 0;

test('two runs settling together share usage once', async () => {
  const { agent, directory, pi } = hostedAgent();
  await agent.submit('one', 'op-a');
  await agent.submit('two', 'op-b');
  agent.meterResponse(response(1));
  pi.settle('op-a');
  pi.settle('op-b');
  await flush();
  assert.equal(ledger(directory), 1);
});

test('a retried report resends its own usage, and later usage goes to the next run', async () => {
  const { agent, directory, gateway, pi } = hostedAgent();
  await agent.submit('one', 'op-a');
  agent.meterResponse(response(1));
  gateway.failReports = 1;
  pi.settle('op-a');
  await flush();
  assert.equal(ledger(directory), 0);
  await agent.submit('two', 'op-b');
  agent.meterResponse(response(2));
  // The report job retries op-a with the usage it was allocated.
  assert.deepEqual(await agent.sweepReports(), { remaining: 1, failed: false });
  assert.equal(ledger(directory), 1);
  pi.settle('op-b');
  await flush();
  assert.equal(ledger(directory), 3);
});

test('resubmitting a reported operation does not report it again', async () => {
  const { agent, gateway, pi } = hostedAgent();
  await agent.submit('one', 'op-a');
  pi.settle('op-a');
  await flush();
  const calls = gateway.calls;
  const receipt = await agent.submit('one', 'op-a');
  await flush();
  assert.equal(receipt.accepted, false);
  assert.equal(gateway.calls, calls);
});

test('the report job stays due until the gateway accepts every settled run', async () => {
  const { agent, directory, gateway, pi, jobs } = hostedAgent();
  await agent.submit('one', 'op-a');
  assert.equal(jobs.get('report')?.fn, 'report');
  // The in-memory wait fails once, then the job twice.
  gateway.failReports = 3;
  pi.settle('op-a');
  await flush();
  const job = { job: jobs.get('report') };
  const first = await agent.onJob(job);
  const second = await agent.onJob(job);
  assert.ok(second.rescheduleAt - Date.now() > first.rescheduleAt - Date.now(), 'backs off while the gateway fails');
  assert.equal(await agent.onJob(job), undefined);
  assert.equal(directory.runs('a')[0].status, 'done');
});

test('admission records a pending run first, and withdraws it when pi refuses', async () => {
  const { agent, directory, pi } = hostedAgent();
  pi.failSubmit = true;
  await assert.rejects(agent.submit('one', 'op-a'), /pi is unavailable/);
  assert.equal(directory.runs('a').length, 0);
  assert.equal(directory.agent('a')?.state, 'idle');
  pi.failSubmit = false;
  await agent.submit('one', 'op-a');
  assert.equal(directory.runs('a')[0].status, 'working');
});

test('a run admitted before its report job ever saw it still reports', async () => {
  const { agent, directory, pi } = hostedAgent();
  await agent.submit('one', 'op-a');
  // An eviction loses the in-memory wait; the next start re-arms it.
  agent.watching = new Set();
  pi.settle('op-a');
  await agent.onStart();
  await flush();
  assert.equal(directory.runs('a')[0].status, 'done');
});

test('a retire that wins the race refuses the submission behind it', async () => {
  const { agent, pi } = hostedAgent();
  const retiring = agent.retire();
  const receipt = await agent.submit('late', 'op-a');
  await retiring;
  assert.deepEqual(receipt, { retired: true });
  assert.equal(pi.submitted.size, 0);
});

test('a run aborted at the step limit reports step_limit', async () => {
  const { agent, state, directory, pi } = hostedAgent();
  await agent.submit('loop', 'op-a');
  // What the step-limit hook leaves behind (test/steps.test.ts runs it on real pi).
  state.storage.kv.put('steps:op-a', 25);
  pi.settle('op-a', { status: 'unanswered', reason: 'aborted' });
  await flush();
  const [run] = directory.runs('a');
  assert.match(String(run.reason), /^step_limit/);
  assert.equal(directory.agent('a')?.state, 'failed');
  assert.equal(state.storage.kv.get('steps:op-a'), undefined);
});

test('an abort the user asked for stays an abort', async () => {
  const { agent, state, directory, pi } = hostedAgent();
  await agent.submit('one', 'op-a');
  state.storage.kv.put('steps:op-a', 3);
  pi.settle('op-a', { status: 'unanswered', reason: 'aborted' });
  await flush();
  assert.equal(directory.runs('a')[0].reason, 'aborted');
  assert.equal(directory.agent('a')?.state, 'idle');
});

test('an operation recorded but never admitted is withdrawn, and its retry runs as a first admission', async () => {
  const { agent, state, directory, gateway, pi } = hostedAgent();
  // An eviction after the record and the pending run row, before pi admitted it.
  state.storage.kv.put('report:op-a', { source: 'planner', startedAt: '2026-10-25T09:00:00.000Z' });
  directory.runStarted('a', 'op-a', 'planner', '2026-10-25T09:00:00.000Z');
  await agent.onStart();
  await flush();
  assert.equal(directory.runs('a').length, 0);
  assert.equal(state.storage.kv.get('report:op-a'), undefined);
  assert.equal(state.storage.kv.get('reported:op-a'), undefined);
  assert.equal(gateway.calls, 0);
  assert.equal(directory.agent('a')?.state, 'idle');

  const receipt = await agent.submit('retry', 'op-a', { from: 'planner', type: 'ask' });
  assert.equal(receipt.accepted, true);
  agent.meterResponse(response(1));
  pi.settle('op-a');
  await flush();
  const [run] = directory.runs('a');
  assert.equal(run.status, 'done');
  assert.equal(ledger(directory), 1);
});

test('the report job withdraws a never-admitted operation it could not withdraw at once', async () => {
  const { agent, state, directory, pi, jobs } = hostedAgent();
  pi.failSubmit = true;
  const withdrawn = directory.runWithdrawn.bind(directory);
  let failWithdraw = true;
  directory.runWithdrawn = (agentId: string, op: string) => {
    if (failWithdraw) throw new Error('gateway unreachable');
    return withdrawn(agentId, op);
  };
  await assert.rejects(agent.submit('one', 'op-a'), /pi is unavailable/);
  assert.equal(directory.runs('a')[0].status, 'working');
  failWithdraw = false;
  assert.equal(await agent.onJob({ job: jobs.get('report') }), undefined);
  assert.equal(directory.runs('a').length, 0);
  assert.equal(state.storage.kv.get('report:op-a'), undefined);
});
