"use client";

import { useState, type ReactNode } from "react";
import { AlertTriangle, Check } from "lucide-react";
import { cn } from "./cn";

/** A server or network error, shown verbatim in mono beneath what caused it. */
export function InlineError({ error, onRetry, className }: { error: string; onRetry?: () => void; className?: string }) {
  return (
    <div role="alert" className={cn("flex items-start gap-2 rounded border border-danger/30 bg-danger/10 px-3 py-2 font-mono text-[11px] leading-5 text-danger", className)}>
      <AlertTriangle aria-hidden className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <span className="min-w-0 flex-1 break-words">{error}</span>
      {onRetry && (
        <button
          type="button"
          onClick={onRetry}
          className="-my-2 -mr-1 min-h-11 shrink-0 px-2 font-bold uppercase tracking-[0.12em] underline-offset-2 hover:underline md:m-0 md:min-h-0 md:px-0"
        >
          Retry
        </button>
      )}
    </div>
  );
}

/** A page-wide notice under the top bar: a degraded link, a caveat. */
export function Banner({ tone, children }: { tone: "warning" | "danger"; children: ReactNode }) {
  return (
    <div
      role="status"
      className={cn(
        "flex items-start gap-2 border-b px-4 py-2 font-mono text-[11px] leading-5",
        tone === "warning" ? "border-warning/30 bg-warning/10 text-warning" : "border-danger/30 bg-danger/10 text-danger",
      )}
    >
      <AlertTriangle aria-hidden className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  );
}

/** Placeholder rows while a list loads. */
export function SkeletonRows({ rows = 3 }: { rows?: number }) {
  return (
    <div aria-busy="true" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="border-b border-border-faint px-4 py-3">
          <div className="mb-2 h-3 w-2/5 rounded-sm bg-fg-faint/35" />
          <div className="h-2.5 w-3/5 rounded-sm bg-fg-faint/20" />
        </div>
      ))}
    </div>
  );
}

/** What an empty list or pane says, and what to do about it. */
export function EmptyState({ icon, title, children }: { icon: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex min-h-64 flex-col items-center justify-center px-6 py-12 text-center font-mono">
      <div aria-hidden className="mb-3 text-fg-subtle">{icon}</div>
      <div className="text-xs font-semibold uppercase tracking-[0.18em] text-fg-muted">{title}</div>
      {children && <div className="mt-2 max-w-sm text-xs leading-5 text-fg-subtle">{children}</div>}
    </div>
  );
}

/** A whole-page state: signed out, not authorised, not configured, failed to load. */
export function PageMessage({ icon, title, children, action }: { icon: ReactNode; title: string; children: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex min-h-dvh items-center justify-center bg-page p-6">
      <div className="w-full max-w-md rounded-[10px] border border-border bg-surface p-6 text-center shadow-[var(--shadow-md)]">
        <div aria-hidden className="mb-3 flex justify-center text-fg-subtle">{icon}</div>
        <h1 className="font-mono text-xs font-bold uppercase tracking-[0.2em] text-fg">{title}</h1>
        <div className="mt-3 text-sm leading-6 text-fg-muted [&_code]:font-mono [&_code]:text-[12px]">{children}</div>
        {action && <div className="mt-5 flex justify-center gap-2">{action}</div>}
      </div>
    </div>
  );
}

export interface ToastMessage {
  id: number;
  text: string;
  tone?: "ok" | "error";
}

/** At most two toasts; 1.5s, errors 4s. */
export function useToasts() {
  const [toasts, setToasts] = useState<ToastMessage[]>([]);
  const push = (text: string, tone: ToastMessage["tone"] = "ok") => {
    const id = Date.now() + Math.random();
    setToasts((current) => [...current.slice(-1), { id, text, tone }]);
    setTimeout(() => setToasts((current) => current.filter((t) => t.id !== id)), tone === "error" ? 4000 : 1500);
  };
  return { toasts, push };
}

/** Bottom centre, above a mobile tab bar. */
export function Toasts({ toasts }: { toasts: ToastMessage[] }) {
  return (
    <div aria-live="polite" className="pointer-events-none fixed inset-x-0 bottom-[calc(72px+env(safe-area-inset-bottom))] z-[60] flex flex-col items-center gap-2 px-4 md:bottom-24">
      {toasts.map((toast) => (
        <div
          key={toast.id}
          className={cn(
            "flex max-w-md items-center gap-2 rounded border px-3 py-2 font-mono text-[11px] shadow-[var(--shadow-md)]",
            toast.tone === "error" ? "border-danger/40 bg-danger-soft text-danger" : "border-border bg-overlay text-fg",
          )}
        >
          {toast.tone === "error" ? <AlertTriangle aria-hidden className="h-3.5 w-3.5" /> : <Check aria-hidden className="h-3.5 w-3.5 text-success" />}
          {toast.text}
        </div>
      ))}
    </div>
  );
}
