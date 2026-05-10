import { cn } from "../lib/utils";
import type { Peer } from "../types";

export function StatusLabel({ status }: { status: Peer["status"] }) {
  const text = status === "online" ? "text-secondary" : status === "busy" ? "text-tertiary-fixed-dim" : "text-outline";
  return <span className={cn("font-mono text-[9px] font-semibold uppercase tracking-[0.16em]", text)}>{status}</span>;
}

export function statusRank(status: Peer["status"]) {
  if (status === "online") return 0;
  if (status === "busy") return 1;
  return 2;
}

export function formatTime(timestamp: string) {
  return new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
