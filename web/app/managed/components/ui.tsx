import type { ReactNode } from "react";
import { buttonClass as sharedButtonClass, cn, labelClass, StatusBadge } from "../../ui";
import type { AgentState } from "../lib/api";
import { STATE_META } from "../lib/state";
import type { LiveConnection } from "../lib/useManagedEvents";
import type { Upstream } from "../lib/useManagedLive";

// The managed views take their components from the shared design system
// (app/ui); only what is specific to managed agents lives here.
export {
  Banner,
  Brand,
  CopyId,
  EmptyState,
  InlineError,
  Modal,
  PageMessage,
  SkeletonRows,
  Toasts,
  inputClass,
  labelClass,
  useToasts,
  type ToastMessage,
} from "../../ui";

export const buttonClass = sharedButtonClass("secondary");
export const primaryButtonClass = sharedButtonClass("primary");
export const dangerButtonClass = sharedButtonClass("danger");

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

export function ConnectionBadge({ connection, upstream }: { connection: LiveConnection; upstream?: Upstream }) {
  if (connection.status === "live" && upstream && !upstream.connected) {
    return (
      <StatusBadge tone="warning" title={`The relay lost its link to the agent runtime${upstream.error ? `: ${upstream.error}` : ""}. Showing the last known state; actions still work.`}>
        DEGRADED
      </StatusBadge>
    );
  }
  if (connection.status === "live") return <StatusBadge tone="success" live title="Receiving live updates">LIVE</StatusBadge>;
  if (connection.status === "reconnecting") {
    return <StatusBadge tone="warning" title="Live updates dropped; reconnecting">{`RECONNECTING ${connection.retryInSec}s`}</StatusBadge>;
  }
  if (connection.status === "offline") {
    return <StatusBadge tone="neutral" title={`Live updates unavailable; retrying in ${connection.retryInSec}s. Actions still work.`}>OFFLINE</StatusBadge>;
  }
  return <StatusBadge tone="neutral" title="Opening live updates">CONNECTING</StatusBadge>;
}
