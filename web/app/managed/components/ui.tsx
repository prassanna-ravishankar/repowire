import Link from "next/link";
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { AlertTriangle, Check, X } from "lucide-react";
import { cn } from "../../dashboard/lib/utils";
import type { AgentState } from "../lib/api";
import { STATE_META } from "../lib/state";
import type { LiveConnection } from "../lib/useManagedEvents";

export const buttonClass =
  "inline-flex h-8 items-center justify-center gap-1.5 rounded border border-border px-2.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-on-surface-variant transition-colors hover:bg-surface-container-high hover:text-on-surface disabled:opacity-50 disabled:hover:bg-transparent";
export const primaryButtonClass =
  "inline-flex h-8 items-center justify-center gap-1.5 rounded bg-primary px-3 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98] disabled:opacity-50 disabled:hover:brightness-100";
export const dangerButtonClass =
  "inline-flex h-8 items-center justify-center gap-1.5 rounded bg-error px-3 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-on-error transition-[filter] hover:brightness-110 disabled:opacity-40 disabled:hover:brightness-100";
export const inputClass =
  "w-full rounded border border-border bg-surface-container-lowest px-3 py-2 font-mono text-base text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary md:text-sm";
export const labelClass = "font-mono text-[9px] font-semibold uppercase tracking-[0.2em] text-outline";

export function StateDot({ state, className }: { state: AgentState; className?: string }) {
  return <span aria-hidden className={cn("inline-block h-2 w-2 shrink-0 rounded-full", STATE_META[state].dot, className)} />;
}

export function StateLabel({ state }: { state: AgentState }) {
  const meta = STATE_META[state];
  return (
    <span title={meta.hint} className={cn("font-mono text-[9px] font-semibold uppercase tracking-[0.16em]", meta.text)}>
      {meta.label}
    </span>
  );
}

export function SectionLabel({ children, right }: { children: ReactNode; right?: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 px-4 pb-2 pt-4">
      <h2 className={labelClass}>{children}</h2>
      {right}
    </div>
  );
}

/** A server or network error, shown verbatim in mono beneath what caused it. */
export function InlineError({ error, onRetry, className }: { error: string; onRetry?: () => void; className?: string }) {
  return (
    <div role="alert" className={cn("flex items-start gap-2 border border-error/30 bg-error/10 px-3 py-2 font-mono text-[11px] leading-5 text-error", className)}>
      <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <span className="min-w-0 flex-1 break-words">{error}</span>
      {onRetry && (
        <button onClick={onRetry} className="shrink-0 font-bold uppercase tracking-[0.12em] underline-offset-2 hover:underline">
          Retry
        </button>
      )}
    </div>
  );
}

export function Banner({ tone, children }: { tone: "warning" | "danger"; children: ReactNode }) {
  return (
    <div
      role="status"
      className={cn(
        "flex items-start gap-2 border-b px-4 py-2 font-mono text-[11px] leading-5",
        tone === "warning" ? "border-tertiary-fixed-dim/30 bg-tertiary-fixed-dim/10 text-on-tertiary-container" : "border-error/30 bg-error/10 text-error",
      )}
    >
      <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  );
}

export function SkeletonRows({ rows = 3 }: { rows?: number }) {
  return (
    <div aria-busy="true" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="border-b border-border-faint px-4 py-3">
          <div className="mb-2 h-3 w-2/5 rounded-sm bg-outline-variant/35" />
          <div className="h-2.5 w-3/5 rounded-sm bg-outline-variant/20" />
        </div>
      ))}
    </div>
  );
}

export function EmptyState({ icon, title, children }: { icon: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="flex min-h-64 flex-col items-center justify-center px-6 py-12 text-center font-mono">
      <div className="mb-3 text-outline">{icon}</div>
      <div className="text-xs font-semibold uppercase tracking-[0.18em] text-on-surface-variant">{title}</div>
      {children && <div className="mt-2 max-w-sm text-xs leading-5 text-outline">{children}</div>}
    </div>
  );
}

/**
 * Centered dialog on desktop, bottom sheet under 640px. A native <dialog>
 * opened with showModal(): the browser makes the page behind it inert and
 * keeps keyboard focus inside. Escape and a backdrop click close it, and
 * focus returns to whatever opened it.
 */
export function Modal({ title, onClose, children, footer }: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  // Captured at first render: autoFocus inside the dialog moves focus before effects run.
  const [opener] = useState(() => (typeof document !== "undefined" && document.activeElement instanceof HTMLElement ? document.activeElement : null));
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);
  useLayoutEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (typeof dialog.showModal === "function") dialog.showModal();
    else dialog.setAttribute("open", ""); // jsdom
    const onCancel = (event: Event) => {
      event.preventDefault();
      onCloseRef.current();
    };
    dialog.addEventListener("cancel", onCancel);
    return () => {
      dialog.removeEventListener("cancel", onCancel);
      if (dialog.open && typeof dialog.close === "function") dialog.close();
      opener?.focus();
    };
  }, [opener]);
  return (
    <dialog
      ref={ref}
      aria-label={title}
      onClick={(event) => event.target === event.currentTarget && onClose()}
      className="fixed inset-0 z-50 m-0 h-full max-h-none w-full max-w-none items-end justify-center border-0 bg-transparent p-0 backdrop:bg-black/50 open:flex sm:items-center sm:p-4"
    >
      <div className="max-h-[90dvh] w-full overflow-y-auto rounded-t-[10px] border border-border bg-surface-container-low pb-[env(safe-area-inset-bottom)] shadow-[var(--shadow-xl)] sm:max-w-md sm:rounded-[10px] sm:pb-0">
        <div className="flex items-center justify-between border-b border-border-faint px-5 py-4">
          <h2 className="font-mono text-xs font-bold uppercase tracking-[0.2em] text-on-surface">{title}</h2>
          <button onClick={onClose} className="text-outline transition-colors hover:text-on-surface" aria-label="Close">
            <X className="h-4 w-4" />
          </button>
        </div>
        <div className="space-y-4 p-5">{children}</div>
        {footer && <div className="flex justify-end gap-2 border-t border-border-faint px-5 py-3">{footer}</div>}
      </div>
    </dialog>
  );
}

export function ConnectionBadge({ connection }: { connection: LiveConnection }) {
  const spec =
    connection.status === "live"
      ? { cls: "border-secondary/25 bg-secondary/10 text-secondary", dot: "bg-secondary pulse-online", text: "LIVE", title: "Receiving live updates" }
      : connection.status === "reconnecting"
        ? { cls: "border-tertiary-fixed-dim/30 bg-tertiary-fixed-dim/10 text-tertiary-fixed-dim", dot: "bg-tertiary-fixed-dim", text: `RECONNECTING ${connection.retryInSec}s`, title: "Live updates dropped; reconnecting" }
        : connection.status === "offline"
          ? { cls: "border-border text-outline", dot: "bg-outline", text: "OFFLINE", title: `Live updates unavailable; retrying in ${connection.retryInSec}s. Actions still work.` }
          : { cls: "border-border text-outline", dot: "bg-outline", text: "CONNECTING", title: "Opening live updates" };
  return (
    <span title={spec.title} className={cn("inline-flex items-center gap-2 border px-2.5 py-1 font-mono text-[10px] font-semibold tracking-[0.16em]", spec.cls)}>
      <span className={cn("h-2 w-2 rounded-full", spec.dot)} />
      {spec.text}
    </span>
  );
}

/** A mono id that copies itself on click. */
export function CopyId({ value, display, className, onCopied }: { value: string; display?: string; className?: string; onCopied?: () => void }) {
  return (
    <button
      type="button"
      title={`Copy ${value}`}
      onClick={(event) => {
        event.stopPropagation();
        void navigator.clipboard?.writeText(value).then(() => onCopied?.(), () => undefined);
      }}
      className={cn("font-mono hover:text-primary", className)}
    >
      {display ?? value}
    </button>
  );
}

export interface ToastMessage {
  id: number;
  text: string;
  tone?: "ok" | "error";
}

/** At most two toasts, bottom center, 1.5s (errors 4s). */
export function useToasts() {
  const [toasts, setToasts] = useState<ToastMessage[]>([]);
  const push = (text: string, tone: ToastMessage["tone"] = "ok") => {
    const id = Date.now() + Math.random();
    setToasts((current) => [...current.slice(-1), { id, text, tone }]);
    setTimeout(() => setToasts((current) => current.filter((t) => t.id !== id)), tone === "error" ? 4000 : 1500);
  };
  return { toasts, push };
}

export function Toasts({ toasts }: { toasts: ToastMessage[] }) {
  return (
    <div aria-live="polite" className="pointer-events-none fixed inset-x-0 bottom-[calc(72px+env(safe-area-inset-bottom))] z-[60] flex flex-col items-center gap-2 px-4 md:bottom-24">
      {toasts.map((toast) => (
        <div
          key={toast.id}
          className={cn(
            "flex max-w-md items-center gap-2 border px-3 py-2 font-mono text-[11px] shadow-[var(--shadow-md)]",
            toast.tone === "error" ? "border-error/40 bg-error-container text-on-error-container" : "border-border bg-surface-container-highest text-on-surface",
          )}
        >
          {toast.tone === "error" ? <AlertTriangle className="h-3.5 w-3.5" /> : <Check className="h-3.5 w-3.5 text-secondary" />}
          {toast.text}
        </div>
      ))}
    </div>
  );
}

export function Brand({ suffix }: { suffix: string }) {
  return (
    <Link href="/" className="flex shrink-0 items-center gap-2.5">
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img src="/brand/logo-mark.svg" alt="" width={20} height={22} />
      <span className="font-headline text-xs font-bold tracking-[0.16em] text-on-surface">REPOWIRE</span>
      <span className="font-mono text-[10px] font-semibold tracking-[0.18em] text-outline">{suffix}</span>
    </Link>
  );
}

/** A whole-page state: signed out, not authorised, not configured, failed to load. */
export function PageMessage({ icon, title, children, action }: { icon: ReactNode; title: string; children: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex min-h-dvh items-center justify-center bg-background p-6">
      <div className="w-full max-w-md rounded-[10px] border border-border bg-surface-container-low p-6 text-center shadow-[var(--shadow-md)]">
        <div className="mb-3 flex justify-center text-outline">{icon}</div>
        <h1 className="font-mono text-xs font-bold uppercase tracking-[0.2em] text-on-surface">{title}</h1>
        <div className="mt-3 text-sm leading-6 text-on-surface-variant [&_code]:font-mono [&_code]:text-[12px]">{children}</div>
        {action && <div className="mt-5 flex justify-center gap-2">{action}</div>}
      </div>
    </div>
  );
}
