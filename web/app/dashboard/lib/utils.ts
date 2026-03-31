import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function timeAgo(dateStr?: string | null): string | null {
  if (!dateStr) return null;
  const diffMs = Date.now() - new Date(dateStr).getTime();
  const diffMins = Math.floor(diffMs / 60000);
  if (diffMins < 1) return "just now";
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return new Date(dateStr).toLocaleDateString();
}

export function statusDot(status: "online" | "busy" | "offline"): string {
  return status === "online"
    ? "bg-secondary pulse-online"
    : status === "busy"
    ? "bg-tertiary-fixed-dim glow-busy"
    : "bg-outline";
}

export function statusBorderColor(status: "online" | "busy" | "offline"): string {
  return status === "online"
    ? "border-secondary/20"
    : status === "busy"
    ? "border-tertiary-fixed-dim/20"
    : "border-outline-variant/20";
}

export function statusTopStrip(status: "online" | "busy" | "offline"): string {
  return status === "online"
    ? "bg-secondary"
    : status === "busy"
    ? "bg-tertiary-fixed-dim"
    : "bg-outline-variant";
}

export function statusTextColor(status: "online" | "busy" | "offline"): string {
  return status === "online"
    ? "text-secondary"
    : status === "busy"
    ? "text-tertiary-fixed-dim"
    : "text-outline";
}

export function roleBadgeClass(role?: string): string | null {
  switch (role) {
    case "service": return "bg-primary/10 text-primary";
    case "orchestrator": return "bg-tertiary-fixed-dim/10 text-tertiary-fixed-dim";
    case "human": return "bg-secondary/10 text-secondary";
    default: return null;
  }
}

export function backendIcon(backend?: string): string {
  if (backend?.includes("telegram")) return "send";
  if (backend?.includes("slack")) return "forum";
  return "smart_toy";
}

/** Format path with folder name prominent: "myproject" or "…/parent/myproject" */
export function shortPath(path: string): { folder: string; parent: string } {
  const parts = path.split("/").filter(Boolean);
  const folder = parts.pop() || path;
  const parent = parts.length > 1 ? `…/${parts.slice(-1)[0]}/` : parts.length === 1 ? `${parts[0]}/` : "";
  return { folder, parent };
}
