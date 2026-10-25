import type { CostDay } from "../../managed/lib/api";

export const SERVICES = [
  { key: "model_usd", label: "Model" },
  { key: "container_usd", label: "Containers" },
  { key: "do_usd", label: "Durable Objects" },
] as const;

export type ServiceKey = (typeof SERVICES)[number]["key"];

/**
 * Month-to-date running total per day. Resets on the first day of each
 * calendar month (UTC), so a 30-day window that crosses a month boundary
 * shows the new month's climb toward the alert lines.
 */
export function cumulativeMonth(days: Pick<CostDay, "day" | "total_usd">[]): number[] {
  let month = "";
  let sum = 0;
  return days.map(({ day, total_usd }) => {
    const m = day.slice(0, 7);
    if (m !== month) {
      month = m;
      sum = 0;
    }
    sum += total_usd;
    return round2(sum);
  });
}

export function round2(value: number): number {
  return Math.round(value * 100) / 100;
}

/** Smallest 1, 2 or 5 times a power of ten that is >= value (and > 0). */
export function niceCeil(value: number): number {
  if (!(value > 0)) return 1;
  const power = 10 ** Math.floor(Math.log10(value));
  for (const step of [1, 2, 5, 10]) {
    const candidate = Number((step * power).toPrecision(12));
    if (candidate >= value - 1e-9) return candidate;
  }
  return 10 * power;
}

/** Axis ticks from 0 to a nice maximum, in `count` equal steps. */
export function ticks(max: number, count = 4): number[] {
  const top = niceCeil(max);
  return Array.from({ length: count + 1 }, (_, i) => round2((top * i) / count));
}

export type AlertLevel = "ok" | "warning" | "danger";

/** Which alert band a month-to-date total sits in, given ascending thresholds. */
export function alertLevel(total: number, alerts: number[]): AlertLevel {
  const [warn = Infinity, danger = Infinity] = [...alerts].sort((a, b) => a - b);
  if (total >= danger) return "danger";
  if (total >= warn) return "warning";
  return "ok";
}

export interface Segment {
  key: ServiceKey;
  y: number;
  height: number;
  value: number;
}

/**
 * Stacked bar segments for one day, bottom-up in SERVICES order, in a plot of
 * `height` px scaled to `max`. Every segment above the first gives up `gap` px
 * at its base so stacked fills stay separated by the surface; zero and
 * too-thin segments are dropped.
 */
export function stackSegments(day: CostDay, max: number, height: number, gap = 2): Segment[] {
  const scale = max > 0 ? height / max : 0;
  const out: Segment[] = [];
  let base = height;
  for (const { key } of SERVICES) {
    const value = day[key];
    const full = value * scale;
    if (full <= 0) continue;
    const trimmed = out.length > 0 ? full - gap : full;
    if (trimmed >= 1) out.push({ key, y: base - full, height: trimmed, value });
    base -= full;
  }
  return out;
}

/** SVG path through points, x evenly spaced across `width`. */
export function linePath(values: number[], max: number, width: number, height: number): string {
  if (values.length === 0) return "";
  const step = values.length > 1 ? width / (values.length - 1) : 0;
  const y = (v: number) => (max > 0 ? height - (v / max) * height : height);
  return values.map((v, i) => `${i === 0 ? "M" : "L"}${round2(i * step)},${round2(y(v))}`).join(" ");
}

/** A bar rectangle whose top corners are rounded by `r` (clamped) and whose base stays square. */
export function barPath(x: number, y: number, w: number, h: number, r: number): string {
  const k = Math.max(0, Math.min(r, w / 2, h));
  const n = (v: number) => round2(v);
  if (k === 0) return `M${n(x)},${n(y)}h${n(w)}v${n(h)}h${n(-w)}Z`;
  return `M${n(x)},${n(y + h)}V${n(y + k)}Q${n(x)},${n(y)} ${n(x + k)},${n(y)}H${n(x + w - k)}Q${n(x + w)},${n(y)} ${n(x + w)},${n(y + k)}V${n(y + h)}Z`;
}

/** Cloudflare GraphQL dataset names, shortened to the product they meter. */
export function datasetLabel(dataset: string): string {
  const d = dataset.toLowerCase();
  if (d.includes("container")) return "Containers";
  if (d.includes("durable")) return "Durable Objects";
  if (/workersai|workers_ai|\bai\b/.test(d)) return "Workers AI";
  if (d.includes("worker")) return "Workers";
  return dataset;
}

const DAY_MS = 86_400_000;
const isoDay = (ms: number) => new Date(ms).toISOString().slice(0, 10);

export interface CostTimeline {
  /** Every calendar day in the window, zero-filled where the estimate has no row. */
  days: CostDay[];
  /** Actual daily totals on the same calendar; null where Cloudflare reported nothing. */
  actualDays: (number | null)[] | null;
  cum: number[];
  actualCum: number[] | null;
  /** Authoritative month-to-date totals for the month of `as_of`. */
  mtd: number;
  actualMtd: number | null;
  month: string;
}

/**
 * Lays estimate and actual rows on one calendar. Both series are sparse (a
 * day appears only when something was metered), so the window is the union
 * of the last `span` days up to `as_of`, the whole current month, and any
 * day either series reports. Each series is summed on its own; headline
 * totals come from the server's month fields, anchored to `as_of`.
 */
export function costTimeline(
  costs: {
    estimate: { days: CostDay[]; month_usd: number };
    actual: { days: { day: string; total_usd: number }[]; month: { total_usd: number } } | null;
    as_of: string;
  },
  span = 30,
): CostTimeline {
  const asOf = Date.parse(costs.as_of);
  const today = isoDay(asOf);
  const month = today.slice(0, 7);
  const set = new Set<string>();
  for (let i = span - 1; i >= 0; i--) set.add(isoDay(asOf - i * DAY_MS));
  for (let d = Date.parse(`${month}-01T00:00:00Z`); isoDay(d) <= today; d += DAY_MS) set.add(isoDay(d));
  for (const row of costs.estimate.days) if (row.day <= today) set.add(row.day);
  for (const row of costs.actual?.days ?? []) if (row.day <= today) set.add(row.day);
  const calendar = [...set].sort();
  const first = calendar[0];
  // Fill gaps so x spacing is one slot per calendar day.
  const all: string[] = [];
  for (let d = Date.parse(`${first}T00:00:00Z`); isoDay(d) <= today; d += DAY_MS) all.push(isoDay(d));

  const estimateByDay = new Map(costs.estimate.days.map((row) => [row.day, row]));
  const days = all.map((day) => estimateByDay.get(day) ?? { day, model_usd: 0, container_usd: 0, do_usd: 0, total_usd: 0 });
  const actualByDay = costs.actual ? new Map(costs.actual.days.map((row) => [row.day, row.total_usd])) : null;
  const actualDays = actualByDay ? all.map((day) => actualByDay.get(day) ?? null) : null;
  return {
    days,
    actualDays,
    cum: cumulativeMonth(days),
    actualCum: actualDays ? cumulativeMonth(actualDays.map((total, i) => ({ day: all[i], total_usd: total ?? 0 }))) : null,
    mtd: costs.estimate.month_usd,
    actualMtd: costs.actual ? costs.actual.month.total_usd : null,
    month,
  };
}
