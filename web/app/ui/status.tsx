import type { ReactNode } from "react";
import { cn } from "./cn";

// Status reads the same on every surface: success (online, answered), warning
// (busy, degraded), danger (failed), accent (active, selected), neutral
// (offline, idle). Colour is never the only signal: a dot comes with a word.

export type Tone = "success" | "warning" | "danger" | "accent" | "neutral";

const dot: Record<Tone, string> = {
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-danger",
  accent: "bg-accent",
  neutral: "bg-fg-subtle",
};
const text: Record<Tone, string> = {
  success: "text-success",
  warning: "text-warning",
  danger: "text-danger",
  accent: "text-accent",
  neutral: "text-fg-subtle",
};
const badge: Record<Tone, string> = {
  success: "border-success/25 bg-success-soft text-success",
  warning: "border-warning/30 bg-warning-soft text-warning",
  danger: "border-danger/30 bg-danger-soft text-danger",
  accent: "border-accent/30 bg-accent-soft text-accent",
  neutral: "border-border text-fg-subtle",
};

/** A status dot. `live` pulses it (only for a connection that is live). */
export function Dot({ tone, live, className }: { tone: Tone; live?: boolean; className?: string }) {
  return <span aria-hidden className={cn("inline-block h-2 w-2 shrink-0 rounded-full", dot[tone], live && "pulse-online", className)} />;
}

/** A status word in the mono chrome, coloured by tone. */
export function StatusText({ tone, children, title }: { tone: Tone; children: ReactNode; title?: string }) {
  return (
    <span title={title} className={cn("font-mono text-[10px] font-semibold uppercase tracking-[0.16em]", text[tone])}>
      {children}
    </span>
  );
}

/** A bordered status pill: a dot and a word, such as a connection state. */
export function StatusBadge({ tone, live, children, title }: { tone: Tone; live?: boolean; children: ReactNode; title?: string }) {
  return (
    <span title={title} className={cn("inline-flex items-center gap-2 rounded border px-2.5 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.16em]", badge[tone])}>
      <Dot tone={tone} live={live} />
      {children}
    </span>
  );
}
