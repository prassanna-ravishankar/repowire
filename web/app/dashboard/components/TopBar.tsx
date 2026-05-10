import Image from "next/image";
import { Plus, RefreshCw, Settings } from "lucide-react";
import { cn } from "../lib/utils";

export function TopBar({
  counts,
  isConnected,
  isRefreshing,
  onRefresh,
  onSpawn,
  onSettings,
}: {
  counts: Record<"online" | "busy" | "offline", number>;
  isConnected: boolean;
  isRefreshing: boolean;
  onRefresh: () => void;
  onSpawn: () => void;
  onSettings: () => void;
}) {
  return (
    <header className="col-span-full flex h-12 items-center gap-3 border-b border-border-faint bg-surface-dim px-3 md:h-[52px] md:px-5">
      <div className="flex min-w-0 items-center gap-3 md:w-[397px]">
        <Image src="/brand/logo-mark-copper.svg" alt="" width={22} height={24} priority />
        <span className="font-headline text-xs font-bold tracking-[0.2em] text-on-surface">REPOWIRE</span>
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
          isConnected
            ? "border-secondary/25 bg-secondary/10 text-secondary"
            : "border-error/25 bg-error/10 text-error"
        )}
      >
        <span className={cn("h-2 w-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
        <span className="hidden sm:inline">{isConnected ? "MESH CONNECTED" : "DISCONNECTED"}</span>
        <span className="sm:hidden">{isConnected ? "LIVE" : "DOWN"}</span>
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
