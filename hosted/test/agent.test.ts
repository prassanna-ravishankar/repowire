import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Gateway } from '../src/gateway';
import { HostedAgent } from '../src/index';
import { serial } from '../src/serial';
import { fakeState } from './support/state';

// pi as the agent sees it: operations settle when the test says so.
function fakePi() {
  const settled = new Map<string, { status: string; reason?: string; text?: string }>();
  const waiters = new Map<string, (value: unknown) => void>();
  const submitted = new Set<string>();
  const pi = {
    submitted, aborted: [] as (string | undefined)[], failSubmit: false,
    async submit(_text: string, { operationId }: { operationId: string }) {
      if (pi.failSubmit) throw new Error('pi is unavailable');
      const accepted = !submitted.has(operationId);
      submitted.add(operationId);
      return { operationId, session: '1', accepted };
    },
    async pending() {
      return [...submitted].filter((op) => !settled.has(op)).map((operationId) => ({ operationId, session: '1', status: 'running' }));
    },
    wait(operationId: string) {
      const result = (value: { status: string; reason?: string; text?: string }) => ({ operationId, session: '1', ...value });
      if (!submitted.has(operationId)) return Promise.resolve(result({ status: 'unanswered', reason: 'not_found' }));
      if (settled.has(operationId)) return Promise.resolve(result(settled.get(operationId)!));
      return new Promise((resolve) => waiters.set(operationId, (value) => resolve(result(value as never))));
    },
    async abort(options: { operationId?: string }) { pi.aborted.push(options.operationId); return true; },
    settle(operationId: string, value: { status: string; reason?: string; text?: string } = { status: 'done', text: 'ok' }) {
      settled.set(operationId, value);
      waiters.get(operationId)?.(value);
    },
  };
  return pi;
}

// The real agent code over fake storage, pi, Lifecycle jobs, and a real
// Gateway. The constructor wires Workers AI and Lifecycle, so it is skipped.
function hostedAgent() {
  const state = fakeState();
  const directory = new Gateway(fakeState() as never, {});
  const gateway = { failReports: 0, calls: 0, directory };
  const owner = new Proxy(directory, {
    get(target, name) {
      if (name === 'report') {
        return async (...args: Parameters<Gateway['report']>) => {
          gateway.calls++;
          if (gateway.failReports > 0) { gateway.failReports--; throw new Error('gateway unreachable'); }
          return target.report(...args);
        };
      }
      const value = Reflect.get(target, name);
      return typeof value === 'function' ? value.bind(target) : value;
    },
  });
  const pi = fakePi();
  const jobs = new Map<string, { id: string; fn: string; time: number }>();
  const agent = Object.assign(Object.create(HostedAgent.prototype), {
    ctx: state, env: { GATEWAYS: { getByName: () => owner } }, kv: state.storage.kv,
    admission: serial(), watching: new Set(), harness: pi,
    lifecycle: {
      start: async () => {},
      jobs: { get: (id: string) => jobs.get(id), push: async (job: { id: string; fn: string; time: number }) => void jobs.set(job.id, job) },
    },
  });
  state.storage.kv.put('config', { owner: 'alice', id: 'a', workspace: null });
  directory.addAgent({ id: 'a', workspace: null, created_by: 'daemon' });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return { agent: agent as any, state, gateway, directory, pi, jobs };
}

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

test('a run past the step limit is aborted and fails as step_limit', async () => {
  const { agent, directory, pi } = hostedAgent();
  await agent.submit('loop', 'op-a');
  for (let request = 0; request < 24; request++) await agent.countStep();
  assert.deepEqual(pi.aborted, []);
  // The count is durable: an eviction mid-run does not reset it.
  agent.watching = new Set();
  await agent.countStep();
  assert.deepEqual(pi.aborted, ['op-a']);
  pi.settle('op-a', { status: 'unanswered', reason: 'aborted' });
  await agent.onStart();
  await flush();
  const [run] = directory.runs('a');
  assert.match(String(run.reason), /^step_limit/);
  assert.equal(directory.agent('a')?.state, 'failed');
});

test('an abort the user asked for stays an abort', async () => {
  const { agent, directory, pi } = hostedAgent();
  await agent.submit('one', 'op-a');
  await agent.countStep();
  pi.settle('op-a', { status: 'unanswered', reason: 'aborted' });
  await flush();
  assert.equal(directory.runs('a')[0].reason, 'aborted');
  assert.equal(directory.agent('a')?.state, 'idle');
});
