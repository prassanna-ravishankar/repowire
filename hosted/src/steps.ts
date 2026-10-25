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

// How long a request is held for its abort to arrive before going ahead.
const ABORT_WAIT_MS = 10_000;

export const stepLimitReason = (limit: number) => `step_limit: more than ${limit} model requests in one run`;

// pi reports a hook's throw and sends the request anyway, unless the run's
// signal has aborted. So past the limit the hook starts the abort, holds this
// request until the abort reaches the run, and then throws: the request is
// never sent, and the run settles aborted.
export function stepLimit(guard: StepGuard, limit: number) {
  return async (_request: unknown, api: object) => {
    const over = (await guard.running()).filter((operationId) => guard.count(operationId) > limit);
    if (!over.length) return undefined;
    for (const operationId of over) {
      guard.abort(operationId).catch((error) => console.error('Hosted step limit abort failed', operationId, error));
    }
    // The hook API is pi's invocation runtime, whose signal is the run's.
    await whenAborted((api as { signal?: AbortSignal }).signal, ABORT_WAIT_MS);
    throw new Error(stepLimitReason(limit));
  };
}

function whenAborted(signal: AbortSignal | undefined, ms: number): Promise<void> {
  if (!signal || signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      console.error('Hosted step limit: the abort did not reach the run in time; this request goes ahead');
      resolve();
    }, ms);
    signal.addEventListener('abort', () => { clearTimeout(timer); resolve(); }, { once: true });
  });
}
