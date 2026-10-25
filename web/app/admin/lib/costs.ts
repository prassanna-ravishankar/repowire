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
