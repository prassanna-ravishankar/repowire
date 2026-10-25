// List prices for estimating what a run or a command cost. Read 2026-10-25
// from developers.cloudflare.com: Workers AI pricing (glm-5.3-flash),
// Containers pricing (2026-10-05). Estimates are frozen when recorded.

const PER_MILLION = 1e6;

// @cf/zai-org/glm-5.3-flash, dollars per million tokens.
export const MODEL = { input: 0.15, cached: 0.03, output: 0.5 };

// Container `lite` instance (1/16 vCPU, 256 MiB, 2 GB disk), dollars per second
// of wall time. vCPU is billed on active use, so this is an upper bound.
// Durable Object containers do not accept instance_type in wrangler.jsonc;
// the size is assumed, not configured, until the Analytics API confirms it.
const LITE = { vcpu: 1 / 16, memoryGiB: 0.25, diskGB: 2 };
const CONTAINER = { vcpuSecond: 0.00002, gibSecond: 0.0000025, gbSecond: 0.00000007 };

export interface TokenUsage { input: number; cacheRead: number; output: number; cost?: { total?: number } }

// Pi reports each response's cost at the catalog's list price; fall back to the
// table above when a provider leaves it out.
export function modelCost(usage: TokenUsage): number {
  if (typeof usage.cost?.total === 'number') return usage.cost.total;
  return (usage.input * MODEL.input + usage.cacheRead * MODEL.cached + usage.output * MODEL.output) / PER_MILLION;
}

export function execCost(wallMs: number): number {
  const seconds = wallMs / 1000;
  return seconds * (LITE.vcpu * CONTAINER.vcpuSecond + LITE.memoryGiB * CONTAINER.gibSecond + LITE.diskGB * CONTAINER.gbSecond);
}

// Spend guardrails. Cloudflare budget alerts notify; these refuse.
export const LIMITS = {
  ownerUsdPerDay: 2,
  ownerUsdPerMonth: 20,
  agentUsdPerDay: 1,
  stepsPerRun: 24,
  meshMessagesPerWindow: 30,
  meshWindowMs: 10 * 60_000,
};

// Admin alert lines, matching the account's Cloudflare budget alerts.
export const ALERTS_USD = [15, 30];
