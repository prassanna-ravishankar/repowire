import { Gateway } from '../../src/gateway';
import { HostedAgent } from '../../src/index';
import { serial } from '../../src/serial';
import { fakeState } from './state';

// pi as the agent sees it: operations settle when the test says so.
export function fakePi() {
  const settled = new Map<string, { status: string; reason?: string; text?: string }>();
  const waiters = new Map<string, ((value: unknown) => void)[]>();
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
      return new Promise((resolve) => waiters.set(operationId, [...(waiters.get(operationId) ?? []), (value) => resolve(result(value as never))]));
    },
    async abort(options: { operationId?: string }) { pi.aborted.push(options.operationId); return true; },
    settle(operationId: string, value: { status: string; reason?: string; text?: string } = { status: 'done', text: 'ok' }) {
      settled.set(operationId, value);
      for (const waiter of waiters.get(operationId) ?? []) waiter(value);
    },
  };
  return pi;
}

// The real agent code over fake storage, pi, Lifecycle jobs, and a real
// Gateway. The constructor wires Workers AI and Lifecycle, so it is skipped.
export function hostedAgent(directory = new Gateway(fakeState() as never, {}), createdBy = 'daemon') {
  const state = fakeState();
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
  directory.addAgent({ id: 'a', workspace: null, created_by: createdBy });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return { agent: agent as any, state, gateway, directory, pi, jobs };
}
