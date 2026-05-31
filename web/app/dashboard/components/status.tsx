import { cn } from "../lib/utils";
import type { Peer } from "../types";

export function StatusLabel({ status }: { status: Peer["status"] }) {
  const text = status === "online" ? "text-secondary" : status === "busy" ? "text-tertiary-fixed-dim" : "text-outline";
  return <span className={cn("font-mono text-[9px] font-semibold uppercase tracking-[0.16em]", text)}>{status}</span>;
}

// Per-turn progress hint, orthogonal to status. Surfaced especially for
// pending_first_turn (a seeded peer that never received its first prompt) and
// awaiting_input (waiting on the user mid-turn) so operators can act. idle and
// null render nothing to keep the roster quiet.
const TURN_STATE_HINTS: Record<string, { label: string; className: string }> = {
  pending_first_turn: { label: "needs prompt", className: "text-tertiary-fixed-dim" },
  awaiting_input: { label: "awaiting input", className: "text-tertiary-fixed-dim" },
  working: { label: "working", className: "text-outline" },
};

export function TurnStateHint({ turnState }: { turnState?: Peer["turn_state"] }) {
  const hint = turnState ? TURN_STATE_HINTS[turnState] : undefined;
  if (!hint) return null;
  return (
    <span className={cn("font-mono text-[9px] font-medium uppercase tracking-[0.12em]", hint.className)}>
      {hint.label}
    </span>
  );
}

export function statusRank(status: Peer["status"]) {
  if (status === "online") return 0;
  if (status === "busy") return 1;
  return 2;
}

export function formatTime(timestamp: string) {
  return new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
