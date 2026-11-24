// Pure rules the owner directory applies: agent state, spend admission, and
// the mesh loop guard. Kept free of Durable Object APIs so they are testable.
import { LIMITS, type TokenUsage } from './prices';

export type AgentState = 'idle' | 'working' | 'parked' | 'failed' | 'retired';

export const utcDay = (ms: number) => new Date(ms).toISOString().slice(0, 10);

// Retired wins; any unsettled run means working; otherwise the last settled
// run decides between failed and idle. An abort the user asked for is not a
// failure.
export function deriveState(retired: boolean, working: number, last?: { status: string; reason: string | null }): AgentState {
  if (retired) return 'retired';
  if (working > 0) return 'working';
  if (last?.status === 'unanswered' && last.reason !== 'aborted') return 'failed';
  return 'idle';
}

export interface Spent { ownerDay: number; ownerMonth: number; agentDay: number }

// The refusal reason for a new submission, or null when it may run. A run in
// flight may still overshoot by its remaining requests.
export function admission(spent: Spent, limits = LIMITS): string | null {
  const over = (label: string, used: number, cap: number, resets: string) =>
    used >= cap ? `budget_exceeded: ${label} spent $${used.toFixed(2)} of $${cap.toFixed(2)}; resets ${resets}` : null;
  return over('owner', spent.ownerDay, limits.ownerUsdPerDay, '00:00 UTC')
    ?? over('owner this month', spent.ownerMonth, limits.ownerUsdPerMonth, 'on the 1st (UTC)')
    ?? over('agent', spent.agentDay, limits.agentUsdPerDay, '00:00 UTC');
}

// Sliding-window loop guard for agent-originated mesh messages. Returns the
// kept timestamps including this one, or null when over the limit.
export function meshWindow(recent: number[], now: number, limits = LIMITS): number[] | null {
  const kept = recent.filter((at) => now - at < limits.meshWindowMs);
  if (kept.length >= limits.meshMessagesPerWindow) return null;
  return [...kept, now];
}

// An agent's running model usage, and the share of it already reported.
export interface Cumulative { input: number; cacheRead: number; output: number; cost: number }

export const zeroUsage = (): Cumulative => ({ input: 0, cacheRead: 0, output: 0, cost: 0 });

export function addResponse(total: Cumulative, usage: { input: number; output: number; cacheRead: number; cost?: { total?: number } }): Cumulative {
  return {
    input: total.input + usage.input, cacheRead: total.cacheRead + usage.cacheRead,
    output: total.output + usage.output, cost: total.cost + (usage.cost?.total ?? 0),
  };
}

// The usage accrued since the last report, allocated to the run that settles.
export function usageSince(total: Cumulative, reported: Cumulative): TokenUsage {
  return {
    input: total.input - reported.input, cacheRead: total.cacheRead - reported.cacheRead,
    output: total.output - reported.output, cost: { total: total.cost - reported.cost },
  };
}

// When the report job runs next: a minute while runs are in flight, doubling
// for each failed attempt in a row, up to ten minutes.
export function reportDelay(failures: number): number {
  return Math.min(60_000 * 2 ** failures, 10 * 60_000);
}
