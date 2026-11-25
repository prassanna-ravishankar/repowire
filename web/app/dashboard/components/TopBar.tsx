import Image from "next/image";
import { BriefcaseBusiness, Inbox, Megaphone, Plus, RefreshCw, Settings } from "lucide-react";
import { cn } from "../lib/utils";
import type { ConnectionState } from "../lib/useEventStream";
import { Button, IconButton, ThemeToggle } from "../../ui";

export function TopBar({
  counts,
  connection,
  isRefreshing,
  activeView,
  unread,
  onRefresh,
  onMesh,
  onInbox,
  onJobs,
  onSpawn,
  onBroadcast,
  onSettings,
}: {
  counts: Record<"online" | "busy" | "offline", number>;
  connection: ConnectionState;
  isRefreshing: boolean;
  activeView: "mesh" | "inbox" | "jobs";
  unread: number;
  onRefresh: () => void;
  onMesh: () => void;
  onInbox: () => void;
  onJobs: () => void;
  onSpawn: () => void;
  onBroadcast: () => void;
  onSettings: () => void;
}) {
  const badge = connectionBadge(connection);
  return (
    <header className="sticky top-0 z-30 col-span-full flex min-h-[calc(48px+env(safe-area-inset-top))] items-center gap-2 border-b border-border-faint bg-surface-dim px-3 pt-[env(safe-area-inset-top)] md:static md:h-[52px] md:min-h-0 md:gap-3 md:px-5 md:pt-0">
      {/* Lines up with the peer list from 2xl; narrower, the bar needs the room. */}
      <div className="flex shrink-0 items-center gap-3 2xl:w-[397px]">
        <Image src="/brand/logo-mark.svg" alt="" width={22} height={24} priority />
        <span className="font-headline text-xs font-bold tracking-[0.16em] text-on-surface">REPOWIRE</span>
        <span className="hidden font-mono text-[10px] font-semibold tracking-[0.18em] text-outline md:inline">DASH</span>
      </div>

      {/* Lowest priority first: the daemon address shows from 2xl, the counts
          from xl. Nothing in the bar wraps. */}
      <div className="hidden min-w-0 flex-1 items-center gap-5 xl:flex">
        <CountPill label="ONLINE" value={counts.online} tone="online" />
        <CountPill label="BUSY" value={counts.busy} tone="busy" />
        <CountPill label="OFFLINE" value={counts.offline} tone="offline" />
        <span className="ml-auto hidden whitespace-nowrap font-mono text-[11px] text-outline 2xl:inline">daemon {">"} 127.0.0.1:8377</span>
      </div>

      <div
        className={cn(
          "ml-auto flex shrink-0 items-center gap-2 whitespace-nowrap rounded border px-2.5 py-1 font-mono text-[10px] font-semibold tracking-[0.16em] xl:ml-0",
          badge.className,
        )}
        title={badge.title}
      >
        <span className={cn("h-2 w-2 rounded-full", badge.dot)} />
        <span className="hidden sm:inline">{badge.long}</span>
        <span className="sm:hidden">{badge.short}</span>
      </div>

      <div className="hidden shrink-0 overflow-hidden rounded border border-border md:flex">
        <button
          onClick={onMesh}
          aria-pressed={activeView === "mesh"}
          className={cn(
            "h-8 whitespace-nowrap px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
            activeView === "mesh" ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          Mesh
        </button>
        <button
          onClick={onInbox}
          aria-pressed={activeView === "inbox"}
          aria-label={unread > 0 ? `Inbox, ${unread} unread` : "Inbox"}
          className={cn(
            "inline-flex h-8 items-center gap-1.5 whitespace-nowrap border-l border-border px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
            activeView === "inbox" ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          <Inbox className="h-3.5 w-3.5" />
          Inbox
          {unread > 0 && (
            <span className="rounded-sm bg-primary px-1 text-[9px] tabular-nums text-on-primary">{unread}</span>
          )}
        </button>
        <button
          onClick={onJobs}
          aria-pressed={activeView === "jobs"}
          className={cn(
            "inline-flex h-8 items-center gap-1.5 whitespace-nowrap border-l border-border px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
            activeView === "jobs" ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          <BriefcaseBusiness className="h-3.5 w-3.5" />
          Jobs
        </button>
      </div>
      <Button variant="primary" onClick={onSpawn} aria-label="Spawn peer" className="shrink-0 whitespace-nowrap max-md:w-11 max-md:px-0">
        <Plus aria-hidden className="h-3.5 w-3.5" />
        <span className="hidden md:inline">Spawn peer</span>
      </Button>
      {/* Mesh-wide, so it lives here and never in a peer's composer (#453). */}
      <IconButton label="Broadcast to the mesh" variant="secondary" onClick={onBroadcast} className="shrink-0">
        <Megaphone aria-hidden className="h-3.5 w-3.5" />
      </IconButton>
      <IconButton label="Refresh" variant="secondary" onClick={onRefresh} className="hidden shrink-0 md:inline-flex">
        <RefreshCw aria-hidden className={cn("h-3.5 w-3.5", isRefreshing && "animate-spin")} />
      </IconButton>
      <ThemeToggle className="shrink-0" />
      <IconButton label="Open settings" variant="secondary" onClick={onSettings} className="shrink-0">
        <Settings aria-hidden className="h-3.5 w-3.5" />
      </IconButton>
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
