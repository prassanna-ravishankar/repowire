import { useLayoutEffect, useRef, useState } from "react";
import { cn } from "../../dashboard/lib/utils";
import type { Costs } from "../../managed/lib/api";
import { formatUsd } from "../../managed/lib/state";
import { SERVICES, alertLevel, datasetLabel, type AlertLevel, barPath, cumulativeMonth, linePath, round2, stackSegments, ticks } from "../lib/costs";

// Categorical slots 1-3 of the dataviz reference palette (validated all-pairs
// in both modes), plus slot 7 for actual spend. Text never wears these.
const VIZ_STYLE = `
.cost-viz { --s-model: #2a78d6; --s-container_usd: #eb6834; --s-do_usd: #1baf7a; --s-actual: #6250d6; }
[data-theme="dark"] .cost-viz { --s-model: #3987e5; --s-container_usd: #d95926; --s-do_usd: #199e70; --s-actual: #9085e9; }
`;
const COLOR: Record<string, string> = { model_usd: "var(--s-model)", container_usd: "var(--s-container_usd)", do_usd: "var(--s-do_usd)" };

const PAD = { left: 44, right: 92, top: 8 };
const BARS_H = 150;
const CUM_H = 120;
const GAP_BETWEEN = 36;
const AXIS_H = 20;

function useWidth() {
  const ref = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(640);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => setWidth(Math.max(280, Math.floor(entry.contentRect.width))));
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

const shortDay = (day: string) => new Date(`${day}T00:00:00Z`).toLocaleDateString([], { month: "short", day: "numeric", timeZone: "UTC" });

export function CostChart({ costs }: { costs: Costs }) {
  const [ref, width] = useWidth();
  const [hover, setHover] = useState<number | null>(null);
  const [table, setTable] = useState(false);
  const days = costs.estimate.days;
  const alerts = [...costs.alerts].sort((a, b) => a - b);
  const cum = cumulativeMonth(days);
  const actual = costs.actual;
  const actualByDay = new Map((actual?.days ?? []).map((d) => [d.day, d.total_usd]));
  const actualDays = days.map((d) => actualByDay.get(d.day) ?? null);
  const actualCum = actual ? cumulativeMonth(actualDays.map((total, i) => ({ day: days[i].day, total_usd: total ?? 0 }))) : null;
  const projected = costs.estimate.projected_usd ?? null;

  const plotW = width - PAD.left - PAD.right;
  const band = days.length ? plotW / days.length : plotW;
  const barW = Math.max(2, Math.min(24, band - 2));
  const barTicks = ticks(Math.max(0, ...days.map((d) => d.total_usd)), 4);
  const barMax = barTicks[barTicks.length - 1];

  const cumTop = Math.max(...cum, ...(actualCum ?? []), projected ?? 0, alerts[alerts.length - 1] ?? 0) * 1.1 || 1;
  const cumY0 = PAD.top + BARS_H + AXIS_H + GAP_BETWEEN;
  const cumY = (v: number) => cumY0 + CUM_H - (v / cumTop) * CUM_H;
  const totalH = cumY0 + CUM_H + AXIS_H;
  const x = (i: number) => PAD.left + i * band + band / 2;
  const mtd = cum[cum.length - 1] ?? 0;
  // Cloudflare's billing alerts fire on actual spend, so that wins when known.
  const billed = actual ? actual.month.total_usd : mtd;
  const level = alertLevel(billed, alerts);
  const billedProjection = actual ? actual.projected_usd : projected;
  const projectedLevel = billedProjection === null ? null : alertLevel(billedProjection, alerts);
  const labelDays = days.length > 2 ? [0, Math.floor(days.length / 2), days.length - 1] : days.map((_, i) => i);
  const hovered = hover !== null ? days[hover] : null;

  return (
    <div className="cost-viz">
      <style>{VIZ_STYLE}</style>
      <div className="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-[11px] text-on-surface-variant">
        {SERVICES.map((s) => (
          <span key={s.key} className="inline-flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-[2px]" style={{ background: COLOR[s.key] }} />
            {s.label}
          </span>
        ))}
        <span className="text-outline">daily estimate</span>
        <button onClick={() => setTable(!table)} className="ml-auto rounded border border-border px-2 py-0.5 text-[10px] uppercase tracking-[0.12em] text-outline hover:text-on-surface" aria-pressed={table}>
          {table ? "Chart" : "Table"}
        </button>
      </div>

      {table ? (
        <CostTable days={days} cum={cum} actualDays={actualDays} />
      ) : (
        <div ref={ref} className="relative" onMouseLeave={() => setHover(null)}>
          <svg width={width} height={totalH} role="img" aria-label={`Daily estimated cost for ${days.length} days; month to date ${formatUsd(mtd)}`} className="block select-none">
            {/* Daily stacked bars */}
            {barTicks.map((t) => {
              const y = PAD.top + BARS_H - (barMax ? (t / barMax) * BARS_H : 0);
              return (
                <g key={`bt${t}`}>
                  <line x1={PAD.left} x2={width - PAD.right} y1={y} y2={y} stroke="var(--rds-border)" />
                  <text x={PAD.left - 6} y={y + 3} textAnchor="end" className="fill-[var(--fg-subtle)] font-mono text-[10px]">${t.toFixed(t < 1 ? 2 : 0)}</text>
                </g>
              );
            })}
            {days.map((day, i) => (
              <g key={day.day} opacity={hover === null || hover === i ? 1 : 0.45}>
                {stackSegments(day, barMax, BARS_H).map((seg, j, all) => (
                  <path
                    key={seg.key}
                    d={barPath(x(i) - barW / 2, PAD.top + seg.y, barW, seg.height, j === all.length - 1 ? 4 : 0)}
                    fill={COLOR[seg.key]}
                  />
                ))}
              </g>
            ))}
            <AxisDays days={days} indices={labelDays} x={x} y={PAD.top + BARS_H + 14} />

            {/* Month-to-date cumulative with alert rules */}
            <text x={PAD.left} y={cumY0 - 10} className="fill-[var(--fg-subtle)] font-mono text-[9px] uppercase tracking-[0.16em]">
              Cumulative month to date
            </text>
            <line x1={PAD.left} x2={width - PAD.right} y1={cumY(0)} y2={cumY(0)} stroke="var(--rds-border-strong)" />
            {alerts.map((a, i) => (
              <g key={`alert${a}`}>
                <line x1={PAD.left} x2={width - PAD.right} y1={cumY(a)} y2={cumY(a)} stroke={i === 0 ? "var(--warning)" : "var(--danger)"} strokeDasharray="4 4" strokeWidth={1.5} />
                <text x={width - PAD.right + 6} y={cumY(a) + 3} className="fill-[var(--fg-muted)] font-mono text-[10px]">${a} alert</text>
              </g>
            ))}
            <text x={PAD.left - 6} y={cumY(0) + 3} textAnchor="end" className="fill-[var(--fg-subtle)] font-mono text-[10px]">$0</text>
            {actualCum && <path d={linePath(actualCum, cumTop, plotW - band, CUM_H)} transform={`translate(${x(0)},${cumY0})`} fill="none" stroke="var(--s-actual)" strokeWidth={2} strokeLinejoin="round" />}
            <path d={linePath(cum, cumTop, plotW - band, CUM_H)} transform={`translate(${x(0)},${cumY0})`} fill="none" stroke="var(--fg)" strokeWidth={2} strokeLinejoin="round" />
            {projected !== null && cum.length > 0 && (
              <g>
                <line x1={x(cum.length - 1)} y1={cumY(mtd)} x2={width - PAD.right} y2={cumY(projected)} stroke="var(--fg-muted)" strokeWidth={1.5} strokeDasharray="2 4" strokeLinecap="round" />
                <text x={width - PAD.right + 6} y={clearOf(cumY(projected), alerts.map(cumY)) + 3} className="fill-[var(--fg-muted)] font-mono text-[10px]">
                  proj. {formatUsd(projected)}
                </text>
              </g>
            )}
            {cum.length > 0 && (
              <g>
                <circle cx={x(cum.length - 1)} cy={cumY(mtd)} r={4} fill="var(--fg)" stroke="var(--surface)" strokeWidth={2} />
                <text x={x(cum.length - 1) - 8} y={cumY(mtd) - 8} textAnchor="end" className="fill-[var(--fg)] font-mono text-[10px] font-semibold">
                  {formatUsd(mtd)} est.
                </text>
              </g>
            )}
            <AxisDays days={days} indices={labelDays} x={x} y={cumY0 + CUM_H + 14} />

            {hover !== null && (
              <g pointerEvents="none">
                <line x1={x(hover)} x2={x(hover)} y1={PAD.top} y2={PAD.top + BARS_H} stroke="var(--fg-faint)" />
                <line x1={x(hover)} x2={x(hover)} y1={cumY0} y2={cumY0 + CUM_H} stroke="var(--fg-faint)" />
                <circle cx={x(hover)} cy={cumY(cum[hover])} r={4} fill="var(--fg)" stroke="var(--surface)" strokeWidth={2} />
                {actualCum && <circle cx={x(hover)} cy={cumY(actualCum[hover])} r={4} fill="var(--s-actual)" stroke="var(--surface)" strokeWidth={2} />}
              </g>
            )}
            {days.map((day, i) => (
              <rect
                key={`hit${day.day}`}
                x={PAD.left + i * band}
                y={0}
                width={band}
                height={totalH}
                fill="transparent"
                onMouseEnter={() => setHover(i)}
                onClick={() => setHover(i)}
              />
            ))}
          </svg>
          {hovered && hover !== null && (
            <div
              className="pointer-events-none absolute top-2 z-10 min-w-44 rounded-[6px] border border-border bg-surface-container-highest px-3 py-2 font-mono text-[11px] shadow-[var(--shadow-md)]"
              style={x(hover) > width / 2 ? { right: width - x(hover) + 12 } : { left: x(hover) + 12 }}
            >
              <div className="mb-1 font-semibold text-on-surface">{shortDay(hovered.day)}</div>
              {SERVICES.map((s) => (
                <Line key={s.key} swatch={COLOR[s.key]} label={s.label} value={formatUsd(hovered[s.key])} />
              ))}
              <Line label="Day total" value={formatUsd(hovered.total_usd)} strong />
              <Line swatch="var(--fg)" label="Month to date" value={formatUsd(cum[hover])} />
              {actualCum && <Line swatch="var(--s-actual)" label="Actual (Cloudflare)" value={actualDays[hover] !== null ? formatUsd(actualDays[hover]!) : "-"} />}
            </div>
          )}
        </div>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-[11px] text-on-surface-variant">
        <span className="inline-flex items-center gap-1.5">
          <span className="h-0.5 w-4 bg-[var(--fg)]" /> Estimate MTD
        </span>
        {actual && (
          <span className="inline-flex items-center gap-1.5" title={`Includes the ${formatUsd(actual.month.base_usd)} plan base; the line sums daily usage only`}>
            <span className="h-0.5 w-4" style={{ background: "var(--s-actual)" }} /> Actual MTD {formatUsd(actual.month.total_usd)} (Cloudflare)
          </span>
        )}
        {projected !== null && (
          <span className="inline-flex items-center gap-1.5">
            <span className="w-4 border-t-[1.5px] border-dotted border-[var(--fg-muted)]" /> Projected month end {formatUsd(projected)} est.
          </span>
        )}
        <span className={cn("ml-auto", tone(projectedLevel && projectedLevel !== "ok" ? projectedLevel : level))}>
          {actual ? "actual " : "estimate "}
          {alertText(level, alerts)}
          {projectedLevel && projectedLevel !== level && `; projected ${alertText(projectedLevel, alerts).replace("under", "to stay under").replace("past", "to pass")}`}
        </span>
      </div>
      {actual?.unavailable && Object.keys(actual.unavailable).length > 0 && (
        <ul className="mt-2 space-y-0.5 font-mono text-[11px] text-outline">
          {Object.entries(actual.unavailable).map(([dataset, error]) => (
            <li key={dataset}>
              {datasetLabel(dataset)} analytics unavailable: {error}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Nudges a right-gutter label off any alert label it would overlap. */
function clearOf(y: number, taken: number[], gap = 12): number {
  const hit = taken.find((t) => Math.abs(t - y) < gap);
  if (hit === undefined) return y;
  return y >= hit ? hit + gap : hit - gap;
}

const tone = (level: AlertLevel) => (level === "danger" ? "text-error" : level === "warning" ? "text-tertiary-fixed-dim" : "text-outline");

function alertText(level: AlertLevel, alerts: number[]) {
  if (level === "ok") return `under the $${alerts[0]} alert`;
  return `past the $${level === "warning" ? alerts[0] : alerts[1]} alert`;
}

function AxisDays({ days, indices, x, y }: { days: { day: string }[]; indices: number[]; x: (i: number) => number; y: number }) {
  return (
    <>
      {indices.map((i, k) => (
        <text key={days[i].day} x={x(i)} y={y} textAnchor={k === 0 ? "start" : k === indices.length - 1 ? "end" : "middle"} className="fill-[var(--fg-subtle)] font-mono text-[10px]">
          {shortDay(days[i].day)}
        </text>
      ))}
    </>
  );
}

function Line({ swatch, label, value, strong }: { swatch?: string; label: string; value: string; strong?: boolean }) {
  return (
    <div className={cn("flex items-center gap-2 leading-5", strong ? "font-semibold text-on-surface" : "text-on-surface-variant")}>
      <span className="h-2 w-2 rounded-[2px]" style={{ background: swatch ?? "transparent" }} />
      <span className="flex-1">{label}</span>
      <span className="tabular-nums">{value}</span>
    </div>
  );
}

function CostTable({ days, cum, actualDays }: { days: Costs["estimate"]["days"]; cum: number[]; actualDays: (number | null)[] }) {
  const head = ["Day", ...SERVICES.map((s) => s.label), "Total", "MTD", "Actual"];
  return (
    <div className="max-h-80 overflow-auto">
      <table className="w-full font-mono text-[11px] tabular-nums">
        <thead className="sticky top-0 bg-surface-container-low">
          <tr>
            {head.map((h, i) => (
              <th key={h} className={cn("border-b border-border-faint px-2 py-1.5 font-mono text-[9px] font-semibold uppercase tracking-[0.16em] text-outline", i === 0 ? "text-left" : "text-right")}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {[...days].reverse().map((d, ri) => {
            const i = days.length - 1 - ri;
            return (
              <tr key={d.day} className="text-on-surface-variant">
                <td className="border-b border-border-faint px-2 py-1">{d.day}</td>
                {SERVICES.map((s) => (
                  <td key={s.key} className="border-b border-border-faint px-2 py-1 text-right">{formatUsd(d[s.key])}</td>
                ))}
                <td className="border-b border-border-faint px-2 py-1 text-right text-on-surface">{formatUsd(round2(d.total_usd))}</td>
                <td className="border-b border-border-faint px-2 py-1 text-right">{formatUsd(cum[i])}</td>
                <td className="border-b border-border-faint px-2 py-1 text-right">{actualDays[i] !== null ? formatUsd(actualDays[i]!) : "-"}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
