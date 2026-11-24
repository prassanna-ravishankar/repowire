"use client";

import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Lock, RefreshCw } from "lucide-react";
import { cn } from "../dashboard/lib/utils";
import { ThemeToggle } from "../dashboard/components/ThemeToggle";
import { GateScreen } from "../managed/components/Gate";
import { Brand, CopyId, InlineError, PageMessage, SkeletonRows, Toasts, buttonClass, labelClass, useToasts } from "../managed/components/ui";
import { ApiError, adminApi, type AdminOwner, type AdminSandbox, type AdminSummary, type Costs } from "../managed/lib/api";
import { formatAgo, formatClock, formatDuration, formatUsd, formatWhen, shortId } from "../managed/lib/state";
import { useMe } from "../managed/lib/useMe";
import { CostChart } from "./components/CostChart";
import { alertLevel } from "./lib/costs";

const SECTIONS = ["overview", "sandboxes", "owners", "costs", "connections"] as const;

type Load<T> = { data: T | null; error: string | null };
const empty = <T,>(): Load<T> => ({ data: null, error: null });

export default function AdminPage() {
  const [gate, retry] = useMe();
  if (gate.kind === "ready" && !gate.me.admin) return <Forbidden />;
  if (gate.kind !== "ready") return <GateScreen gate={gate} onRetry={retry} />;
  return <AdminApp />;
}

function Forbidden() {
  return (
    <PageMessage icon={<Lock className="h-6 w-6" />} title="Not authorised" action={<a href="/managed" className={buttonClass}>Your agents</a>}>
      This page is for Repowire Managed operators. Your account is not in <code>REPOWIRE_ADMIN_USERS</code>.
    </PageMessage>
  );
}

function settle<T>(promise: Promise<T>, set: (load: Load<T>) => void, onForbidden: () => void) {
  return promise.then(
    (data) => set({ data, error: null }),
    (err: Error) => {
      if (err instanceof ApiError && err.status === 403) onForbidden();
      set({ data: null, error: err.message });
    },
  );
}

function AdminApp() {
  const [summary, setSummary] = useState<Load<AdminSummary>>(empty);
  const [owners, setOwners] = useState<Load<AdminOwner[]>>(empty);
  const [sandboxes, setSandboxes] = useState<Load<AdminSandbox[]>>(empty);
  const [costs, setCosts] = useState<Load<Costs>>(empty);
  const [forbidden, setForbidden] = useState(false);
  const [refreshing, setRefreshing] = useState(true);
  const [asOf, setAsOf] = useState<number | null>(null);
  const { toasts, push } = useToasts();
  const copied = () => push("Copied");
  const now = asOf ?? 0;

  // Load on open and on Refresh only; admin data has no push.
  const load = useCallback((refresh: boolean) => {
    const deny = () => setForbidden(true);
    return Promise.all([
      settle(adminApi.summary(), setSummary, deny),
      settle(adminApi.owners(), setOwners, deny),
      settle(adminApi.sandboxes(), setSandboxes, deny),
      settle(adminApi.costs(refresh), setCosts, deny),
    ]).then(() => {
      setAsOf(Date.now());
      setRefreshing(false);
    });
  }, []);

  useEffect(() => {
    void load(false);
  }, [load]);

  if (forbidden) return <Forbidden />;

  return (
    <div className="min-h-dvh bg-background text-on-surface">
      <header className="sticky top-0 z-30 border-b border-border-faint bg-surface-dim pt-[env(safe-area-inset-top)]">
        <div className="flex h-12 items-center gap-3 px-3 md:h-[52px] md:px-5">
          <Brand suffix="ADMIN" />
          <nav className="ml-6 hidden gap-4 md:flex">
            {SECTIONS.map((s) => (
              <a key={s} href={`#${s}`} className="font-mono text-[10px] font-bold uppercase tracking-[0.14em] text-outline hover:text-on-surface">
                {s}
              </a>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-2">
            {asOf && <span className="font-mono text-[10px] text-outline">as of {formatClock(new Date(asOf).toISOString())}</span>}
            <button onClick={() => {
                setRefreshing(true);
                void load(true);
              }} disabled={refreshing} className={buttonClass} title="Reload, and refresh Cloudflare spend">
              <RefreshCw className={cn("h-3.5 w-3.5", refreshing && "animate-spin")} />
              <span className="hidden sm:inline">Refresh</span>
            </button>
            <ThemeToggle />
          </div>
        </div>
        <nav className="flex gap-2 overflow-x-auto px-3 pb-2 md:hidden">
          {SECTIONS.map((s) => (
            <a key={s} href={`#${s}`} className="shrink-0 rounded-full border border-border px-3 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] text-outline">
              {s}
            </a>
          ))}
        </nav>
      </header>

      <main className="mx-auto max-w-6xl space-y-8 px-4 py-6 md:px-6">
        <Section id="overview" title="Overview" error={summary.error}>
          {summary.data ? <Tiles summary={summary.data} sandboxes={sandboxes.data} costs={costs.data} /> : !summary.error && <SkeletonRows rows={2} />}
        </Section>

        <Section id="sandboxes" title="Sandboxes" error={sandboxes.error} count={sandboxes.data?.length}>
          {sandboxes.data ? <SandboxTable rows={sandboxes.data} now={now} onCopied={copied} /> : !sandboxes.error && <SkeletonRows />}
        </Section>

        <Section id="owners" title="Owners" error={owners.error} count={owners.data?.length}>
          {owners.data ? <OwnerTable rows={owners.data} now={now} onCopied={copied} /> : !owners.error && <SkeletonRows />}
        </Section>

        <Section id="costs" title="Costs" note="daily, last 30 days, estimates from metered units" error={costs.error}>
          {costs.data ? (
            <Card className="p-4">
              <CostChart costs={costs.data} />
              <p className="mt-3 border-t border-border-faint pt-3 font-mono text-[11px] leading-5 text-outline">
                {costs.data.actual
                  ? `Actual spend from Cloudflare Analytics, as of ${formatWhen(costs.data.as_of, now)}. `
                  : "Actual spend unavailable: set REPOWIRE_CF_ACCOUNT_ID and REPOWIRE_CF_ANALYTICS_TOKEN on the relay. "}
                Alerts are email notifications from Cloudflare billing, not hard caps.
              </p>
            </Card>
          ) : (
            !costs.error && <SkeletonRows rows={4} />
          )}
        </Section>

        <Section id="connections" title="Connections" error={summary.error}>
          {summary.data && <Connections summary={summary.data} />}
        </Section>
      </main>
      <Toasts toasts={toasts} />
    </div>
  );
}

function Section({ id, title, note, count, error, children }: { id: string; title: string; note?: string; count?: number; error: string | null; children: ReactNode }) {
  return (
    <section id={id} className="scroll-mt-28 md:scroll-mt-20">
      <div className="mb-2 flex items-baseline gap-3">
        <h2 className={labelClass}>{title}</h2>
        {count !== undefined && <span className="font-mono text-[10px] tabular-nums text-on-surface-variant">{count}</span>}
        {note && <span className="font-mono text-[10px] text-outline">{note}</span>}
      </div>
      {error && <InlineError error={error} className="mb-2" />}
      {children}
    </section>
  );
}

function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("overflow-hidden rounded-[8px] border border-border bg-surface-container-low", className)}>{children}</div>;
}

function Tile({ label, value, sub, children }: { label: string; value: ReactNode; sub: ReactNode; children?: ReactNode }) {
  return (
    <Card className="p-4">
      <div className={labelClass}>{label}</div>
      <div className="mt-2 font-mono text-2xl font-semibold tabular-nums text-on-surface">{value}</div>
      {children}
      <div className="mt-1 font-mono text-[11px] leading-5 text-outline">{sub}</div>
    </Card>
  );
}

function Tiles({ summary, sandboxes, costs }: { summary: AdminSummary; sandboxes: AdminSandbox[] | null; costs: Costs | null }) {
  const s = summary.agents_by_state;
  const live = s.idle + s.working + s.parked + s.failed;
  const repair = sandboxes?.filter((w) => w.state === "needs_repair").length ?? 0;
  const alerts = [...(costs?.alerts ?? [15, 30])].sort((a, b) => a - b);
  const month = summary.month.cost_usd;
  const level = alertLevel(month, alerts);
  const nextAlert = alerts.find((a) => a > month) ?? alerts[alerts.length - 1];
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-5">
      <Tile
        label="Month to date"
        value={formatUsd(month)}
        sub={[`est. · next alert $${nextAlert}`, costs?.estimate.projected_usd !== undefined && `proj. ${formatUsd(costs.estimate.projected_usd)}`, costs?.actual && `actual ${formatUsd(costs.actual.month.total_usd)}`].filter(Boolean).join(" · ")}
      >
        <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-outline-variant/30" role="meter" aria-valuenow={month} aria-valuemin={0} aria-valuemax={nextAlert} aria-label="Month to date against the next alert">
          <div
            className={cn("h-full rounded-full", level === "danger" ? "bg-error" : level === "warning" ? "bg-tertiary-fixed-dim" : "bg-primary")}
            style={{ width: `${Math.min(100, (month / nextAlert) * 100)}%` }}
          />
        </div>
      </Tile>
      <Tile label="Today" value={formatUsd(summary.today.cost_usd)} sub={`est. · ${summary.today.runs} runs`} />
      <Tile
        label="Agents"
        value={live + s.retired}
        sub={[`${live} active`, `${s.working} working`, s.failed ? `${s.failed} failed` : null, `${s.retired} retired`].filter(Boolean).join(" · ")}
      />
      <Tile label="Sandboxes" value={summary.workspaces} sub={[`${summary.running_containers} running`, repair ? `${repair} need repair` : null].filter(Boolean).join(" · ")} />
      <Tile label="Owners" value={summary.owners} sub={`${summary.connected_daemons} gateway daemon${summary.connected_daemons === 1 ? "" : "s"} connected`} />
    </div>
  );
}

/** A long relay owner id, shortened, with the full id on hover and click-to-copy. */
function OwnerId({ id, onCopied, className }: { id: string; onCopied: () => void; className?: string }) {
  return <CopyId value={id} display={shortId(id)} onCopied={onCopied} className={className} />;
}

const SANDBOX_STATE: Record<AdminSandbox["state"], { label: string; cls: string }> = {
  idle: { label: "STOPPED", cls: "text-outline" },
  running: { label: "RUNNING", cls: "text-secondary" },
  needs_repair: { label: "NEEDS REPAIR", cls: "text-error" },
};

function snapshotLabel(row: AdminSandbox, now: number) {
  if (!row.snapshot_at) return { text: "none", warn: row.state === "needs_repair" };
  const days = (now - Date.parse(row.snapshot_at)) / 86_400_000;
  return { text: formatAgo(row.snapshot_at, now), warn: days > 25 };
}

function DataTable({ head, rows, numeric }: { head: string[]; rows: { key: string; cells: ReactNode[] }[]; numeric: number[] }) {
  return (
    <table className="hidden w-full font-mono text-[11px] md:table">
      <thead>
        <tr>
          {head.map((h, i) => (
            <th key={h} className={cn(labelClass, "border-b border-border-faint px-3 py-2 first:pl-4 last:pr-4", numeric.includes(i) ? "text-right" : "text-left")}>{h}</th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => (
          <tr key={row.key} className="text-on-surface-variant">
            {row.cells.map((cell, i) => (
              <td key={i} className={cn("border-b border-border-faint px-3 py-2 first:pl-4 last:pr-4", numeric.includes(i) && "text-right tabular-nums")}>{cell}</td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function PhoneCards({ rows }: { rows: { key: string; title: ReactNode; state: ReactNode; sub: ReactNode }[] }) {
  return (
    <ul className="md:hidden">
      {rows.map((row) => (
        <li key={row.key} className="border-b border-border-faint px-4 py-2.5 font-mono text-[11px] last:border-b-0">
          <div className="flex items-baseline gap-2">
            <span className="min-w-0 flex-1 truncate text-[13px] font-semibold text-on-surface">{row.title}</span>
            {row.state}
          </div>
          <div className="mt-0.5 text-outline">{row.sub}</div>
        </li>
      ))}
    </ul>
  );
}

function SandboxTable({ rows, now, onCopied }: { rows: AdminSandbox[]; now: number; onCopied: () => void }) {
  if (rows.length === 0) return <Card className="p-4 font-mono text-[11px] text-outline">No sandboxes yet.</Card>;
  const order = { needs_repair: 0, running: 1, idle: 2 };
  const sorted = [...rows].sort((a, b) => order[a.state] - order[b.state] || (b.last_exec_at ?? "").localeCompare(a.last_exec_at ?? ""));
  const state = (row: AdminSandbox) => <span className={cn("font-semibold tracking-[0.12em]", SANDBOX_STATE[row.state].cls)}>{SANDBOX_STATE[row.state].label}</span>;
  const snap = (row: AdminSandbox) => {
    const s = snapshotLabel(row, now);
    return <span className={cn(s.warn && "text-tertiary-fixed-dim")} title={s.warn ? "Snapshots expire after 30 days" : undefined}>{s.text}{s.warn ? " !" : ""}</span>;
  };
  return (
    <Card>
      <DataTable
        head={["Workspace", "Owner", "State", "Snapshot", "Agents", "Commands", "Container", "Last exec"]}
        numeric={[4, 5, 6]}
        rows={sorted.map((row) => ({
          key: `${row.owner}/${row.id}`,
          cells: [<span key="id" className="font-semibold text-on-surface">{row.id}</span>, <OwnerId key="o" id={row.owner} onCopied={onCopied} />, state(row), snap(row), row.agents.length, row.exec_count, formatDuration(row.container_ms), formatWhen(row.last_exec_at, now)],
        }))}
      />
      <PhoneCards
        rows={sorted.map((row) => ({
          key: `${row.owner}/${row.id}`,
          title: row.id,
          state: state(row),
          sub: <><OwnerId id={row.owner} onCopied={onCopied} /> · {row.agents.length} agents · snapshot {snap(row)}</>,
        }))}
      />
    </Card>
  );
}

function OwnerTable({ rows, now, onCopied }: { rows: AdminOwner[]; now: number; onCopied: () => void }) {
  if (rows.length === 0) return <Card className="p-4 font-mono text-[11px] text-outline">No owners yet.</Card>;
  const sorted = [...rows].sort((a, b) => b.month.cost_usd - a.month.cost_usd);
  const live = (row: AdminOwner) => row.agents - row.agents_by_state.retired;
  const daemons = (row: AdminOwner) => (
    <span className={cn("inline-flex items-center gap-1.5", row.connected_daemons ? "text-on-surface-variant" : "text-outline")}>
      <span className={cn("h-1.5 w-1.5 rounded-full", row.connected_daemons ? "bg-secondary" : "bg-outline")} />
      {row.connected_daemons ? row.connected_daemons : "none"}
    </span>
  );
  return (
    <Card>
      <DataTable
        head={["Owner", "Agents", "Working", "Failed", "Workspaces", "Running", "Gateway", "Today est.", "MTD est.", "Last active"]}
        numeric={[1, 2, 3, 4, 5, 7, 8]}
        rows={sorted.map((row) => ({
          key: row.owner,
          cells: [
            <OwnerId key="o" id={row.owner} onCopied={onCopied} className="font-semibold text-on-surface" />,
            live(row),
            row.agents_by_state.working,
            <span key="f" className={cn(row.agents_by_state.failed > 0 && "text-error")}>{row.agents_by_state.failed}</span>,
            row.workspaces,
            row.running_containers,
            daemons(row),
            formatUsd(row.today.cost_usd),
            <span key="c" className="text-on-surface">{formatUsd(row.month.cost_usd)}</span>,
            formatAgo(row.last_active_at, now),
          ],
        }))}
      />
      <PhoneCards
        rows={sorted.map((row) => ({
          key: row.owner,
          title: <OwnerId id={row.owner} onCopied={onCopied} />,
          state: <span className="tabular-nums text-on-surface">{formatUsd(row.month.cost_usd)}</span>,
          sub: (
            <>
              {live(row)} agents{row.agents_by_state.failed ? <span className="text-error"> · {row.agents_by_state.failed} failed</span> : null} · gateway {daemons(row)} · active {formatAgo(row.last_active_at, now)}
            </>
          ),
        }))}
      />
    </Card>
  );
}

function Connections({ summary }: { summary: AdminSummary }) {
  const rows: [string, ReactNode, string][] = [
    ["Gateway daemons", summary.connected_daemons, "Daemons holding a Worker gateway socket (mesh tools for hosted agents)"],
    ["Relay daemons", summary.relay?.daemons_connected ?? "-", "Daemons connected to this relay"],
    ["Users with daemons", summary.relay?.users_with_daemons ?? "-", "Relay accounts with at least one daemon online"],
    ["OAuth grants", summary.relay?.oauth_grants_active ?? "-", "Active app authorisations"],
  ];
  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      {rows.map(([label, value, sub]) => (
        <Tile key={label} label={label} value={value} sub={sub} />
      ))}
    </div>
  );
}
