import Image from "next/image";
import { BriefcaseBusiness, ListChecks, Plus, RefreshCw, Settings } from "lucide-react";
import { cn } from "../lib/utils";
import type { ConnectionState } from "../lib/useEventStream";
import { ThemeToggle } from "./ThemeToggle";

export function TopBar({
  counts,
  connection,
  isRefreshing,
  activeView,
  onRefresh,
  onMesh,
  onJobs,
  onBeads,
  onSpawn,
  onSettings,
}: {
  counts: Record<"online" | "busy" | "offline", number>;
  connection: ConnectionState;
  isRefreshing: boolean;
  activeView: "mesh" | "jobs" | "beads";
  onRefresh: () => void;
  onMesh: () => void;
  onJobs: () => void;
  onBeads: () => void;
  onSpawn: () => void;
  onSettings: () => void;
}) {
  const badge = connectionBadge(connection);
  return (
    <header className="sticky top-0 z-30 col-span-full flex min-h-[calc(48px+env(safe-area-inset-top))] items-center gap-3 border-b border-border-faint bg-surface-dim px-3 pt-[env(safe-area-inset-top)] md:static md:h-[52px] md:min-h-0 md:px-5 md:pt-0">
      <div className="flex min-w-0 items-center gap-3 md:w-[397px]">
        <Image src="/brand/logo-mark.svg" alt="" width={22} height={24} priority />
        <span className="font-headline text-xs font-bold tracking-[0.16em] text-on-surface">REPOWIRE</span>
        <span className="hidden font-mono text-[10px] font-semibold tracking-[0.18em] text-outline md:inline">DASH</span>
      </div>

      <div className="hidden flex-1 items-center gap-5 md:flex">
        <CountPill label="ONLINE" value={counts.online} tone="online" />
        <CountPill label="BUSY" value={counts.busy} tone="busy" />
        <CountPill label="OFFLINE" value={counts.offline} tone="offline" />
        <span className="ml-auto font-mono text-[11px] text-outline">daemon {">"} 127.0.0.1:8377</span>
      </div>

      <div
        className={cn(
          "ml-auto flex items-center gap-2 border px-2.5 py-1 font-mono text-[10px] font-semibold tracking-[0.16em] md:ml-0",
          badge.className,
        )}
        title={badge.title}
      >
        <span className={cn("h-2 w-2 rounded-full", badge.dot)} />
        <span className="hidden sm:inline">{badge.long}</span>
        <span className="sm:hidden">{badge.short}</span>
      </div>

      <div className="hidden overflow-hidden rounded border border-border md:flex">
        <button
          onClick={onMesh}
          aria-pressed={activeView === "mesh"}
          className={cn(
            "h-8 px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
            activeView === "mesh" ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          Mesh
        </button>
        <button
          onClick={onJobs}
          aria-pressed={activeView === "jobs"}
          className={cn(
            "inline-flex h-8 items-center gap-1.5 border-l border-border px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
            activeView === "jobs" ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          <BriefcaseBusiness className="h-3.5 w-3.5" />
          Jobs
        </button>
        <button
          onClick={onBeads}
          aria-pressed={activeView === "beads"}
          className={cn(
            "inline-flex h-8 items-center gap-1.5 border-l border-border px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
            activeView === "beads" ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          <ListChecks className="h-3.5 w-3.5" />
          Beads
        </button>
      </div>
      <button
        onClick={onSpawn}
        className="inline-flex h-8 items-center gap-1.5 rounded bg-primary px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98] md:px-3"
      >
        <Plus className="h-3.5 w-3.5" />
        <span className="hidden md:inline">Spawn peer</span>
      </button>
      <button
        onClick={onRefresh}
        aria-label="Refresh"
        className="hidden h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface md:inline-flex"
      >
        <RefreshCw className={cn("h-3.5 w-3.5", isRefreshing && "animate-spin")} />
      </button>
      <ThemeToggle />
      <button
        onClick={onSettings}
        aria-label="Open settings"
        className="inline-flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
      >
        <Settings className="h-3.5 w-3.5" />
      </button>
    </header>
  );
}

interface BadgeSpec {
  className: string;
  dot: string;
  long: string;
  short: string;
  title: string;
}

function connectionBadge(state: ConnectionState): BadgeSpec {
  if (state.status === "live") {
    return {
      className: "border-secondary/25 bg-secondary/10 text-secondary",
      dot: "bg-secondary pulse-online",
      long: "MESH CONNECTED",
      short: "LIVE",
      title: "Connected to daemon event stream",
    };
  }
  if (state.status === "reconnecting") {
    return {
      className: "border-tertiary-fixed-dim/30 bg-tertiary-fixed-dim/10 text-tertiary-fixed-dim",
      dot: "bg-tertiary-fixed-dim glow-busy",
      long: `RECONNECTING ${state.retryInSec}s`,
      short: `${state.retryInSec}s`,
      title: `Connection dropped. Retrying in ${state.retryInSec}s.`,
    };
  }
  return {
    className: "border-error/25 bg-error/10 text-error",
    dot: "bg-error",
    long: "DISCONNECTED",
    short: "DOWN",
    title: "Not connected to daemon event stream",
  };
}

function CountPill({ label, value, tone }: { label: string; value: number; tone: "online" | "busy" | "offline" }) {
  const color = tone === "online" ? "text-secondary" : tone === "busy" ? "text-tertiary-fixed-dim" : "text-outline";
  const dot = tone === "online" ? "bg-secondary pulse-online" : tone === "busy" ? "bg-tertiary-fixed-dim glow-busy" : "bg-outline";
  return (
    <span className="inline-flex items-baseline gap-2">
      <span className={cn("h-2 w-2 rounded-full", dot)} />
      <span className={cn("font-mono text-sm font-bold tabular-nums", color)}>{value}</span>
      <span className="font-mono text-[9px] font-semibold tracking-[0.18em] text-outline">{label}</span>
    </span>
  );
}
