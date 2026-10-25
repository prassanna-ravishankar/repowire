// The step limit, as a pi `beforeRequest` hook over the agent's own storage
// and harness, so it runs against real pi in tests.

export interface StepGuard {
  // Operation ids in the run this request belongs to.
  running(): Promise<string[]>;
  // Counts one more model request for an operation and returns its total.
  count(operationId: string): number;
  // Ends an operation's run. Resolves once the run is idle, so never awaited
  // from inside that run.
  abort(operationId: string): Promise<unknown>;
}

export const stepLimitReason = (limit: number) => `step_limit: more than ${limit} model requests in one run`;

// pi reports a hook's throw and sends the request anyway, unless the run's
// signal has aborted. So past the limit the hook starts the abort, holds this
// request until the abort reaches the run, and then throws: the request is
// never sent, and the run settles aborted. It fails closed: while the abort
// does not land, it is retried with backoff and the request stays held, so
// the run shows as working until it ends (an eviction re-runs this hook).
export function stepLimit(guard: StepGuard, limit: number, { retryMs = 1000, maxRetryMs = 60_000 } = {}) {
  return async (_request: unknown, api: object) => {
    const over = (await guard.running()).filter((operationId) => guard.count(operationId) > limit);
    if (!over.length) return undefined;
    // The hook API is pi's invocation runtime, whose signal is the run's.
    const signal = (api as { signal?: AbortSignal }).signal;
    if (!(signal instanceof AbortSignal)) throw new Error('step_limit cannot hold the request: this pi version exposes no run signal to hooks');
    for (let attempt = 0; !signal.aborted; attempt++) {
      for (const operationId of over) {
        guard.abort(operationId).catch((error) => console.error('Hosted step limit abort failed; retrying', operationId, error));
      }
      await whenAborted(signal, Math.min(retryMs * 2 ** attempt, maxRetryMs));
    }
    throw new Error(stepLimitReason(limit));
  };
}

// Resolves when signal aborts, or after ms.
function whenAborted(signal: AbortSignal, ms: number): Promise<void> {
  return new Promise((resolve) => {
    const done = () => { clearTimeout(timer); resolve(); };
    const timer = setTimeout(() => { signal.removeEventListener('abort', done); resolve(); }, ms);
    signal.addEventListener('abort', done, { once: true });
  });
}
