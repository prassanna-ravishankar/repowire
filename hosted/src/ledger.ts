// Pure rules the owner directory applies: agent state, spend admission, and
// the mesh loop guard. Kept free of Durable Object APIs so they are testable.
import { LIMITS } from './prices';

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
