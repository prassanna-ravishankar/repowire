import { useCallback, useEffect, useState } from "react";
import { Clock, X } from "lucide-react";
import { cn } from "../lib/utils";
import type { Peer } from "../types";

// When a composer send goes out: now, once at a time, or on a cron repeat.
// Scheduling plugs into the daemon's /schedules API (#433): the composer's
// ask/notify mode is the schedule's kind.
export type SendWhen = "now" | "at" | "repeat";

export interface ScheduleChoice {
  when: SendWhen;
  at: string; // datetime-local value, the user's local time
  cron: string; // five fields, UTC (the daemon evaluates cron in UTC)
}

export const CRON_PRESETS: { label: string; cron: string }[] = [
  { label: "every hour", cron: "0 * * * *" },
  { label: "every 15 min", cron: "*/15 * * * *" },
  { label: "daily 09:00 UTC", cron: "0 9 * * *" },
  { label: "weekdays 09:00 UTC", cron: "0 9 * * 1-5" },
];

export interface Schedule {
  schedule_id: string;
  from_peer: string;
  to_peer: string;
  text: string;
  kind: string;
  fire_at: string;
  cron: string | null;
}

/** The /schedules body for this send, or an error to show instead. */
export function scheduleBody(
  choice: ScheduleChoice,
  base: { to_peer: string; text: string; kind: "ask" | "notify" },
  now: Date = new Date(),
): { body: Record<string, unknown> } | { error: string } {
  const body: Record<string, unknown> = { from_peer: "dashboard", ...base };
  if (choice.when === "at") {
    const at = new Date(choice.at);
    if (!choice.at || Number.isNaN(at.getTime())) return { error: "Pick a date and time" };
    if (at.getTime() <= now.getTime()) return { error: "That time has passed" };
    body.fire_at = at.toISOString();
  } else {
    const cron = choice.cron.trim();
    if (cron.split(/\s+/).length !== 5) return { error: "Cron needs five fields: minute hour day month weekday (UTC)" };
    body.cron = cron;
  }
  return { body };
}

export function formatFire(iso: string): string {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return iso;
  return t.toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit", month: "short", day: "numeric" });
}

/** The "when" row under the mode toggle: now, at a time, or a cron repeat. */
export function ScheduleControls({
  choice,
  onChange,
  mode,
  peer,
}: {
  choice: ScheduleChoice;
  onChange: (c: ScheduleChoice) => void;
  mode: "ask" | "notify";
  peer: Peer;
}) {
  return (
    <div className="mb-2 flex flex-wrap items-center gap-2 font-mono text-[10px] text-outline">
      <div className="inline-flex overflow-hidden rounded border border-border-faint" role="group" aria-label="send when">
        {(["now", "at", "repeat"] as const).map((w) => (
          <button
            key={w}
            data-testid={`when-${w}`}
            onClick={() => onChange({ ...choice, when: w })}
            aria-pressed={choice.when === w}
            className={cn(
              "px-2 py-0.5 uppercase tracking-[0.14em] transition-colors",
              choice.when === w ? "bg-surface-container-high text-on-surface" : "text-outline hover:text-on-surface",
            )}
          >
            {w === "at" ? "later" : w}
          </button>
        ))}
      </div>
      {choice.when === "at" && (
        <input
          type="datetime-local"
          aria-label="send at (your local time)"
          value={choice.at}
          onChange={(e) => onChange({ ...choice, at: e.target.value })}
          className="rounded border border-border-faint bg-surface-container-lowest px-1.5 py-0.5 text-on-surface"
        />
      )}
      {choice.when === "repeat" && (
        <>
          <select
            aria-label="repeat preset"
            value={CRON_PRESETS.some((p) => p.cron === choice.cron) ? choice.cron : ""}
            onChange={(e) => e.target.value && onChange({ ...choice, cron: e.target.value })}
            className="rounded border border-border-faint bg-surface-container-lowest px-1 py-0.5 text-on-surface"
          >
            <option value="">preset…</option>
            {CRON_PRESETS.map((p) => (
              <option key={p.cron} value={p.cron}>
                {p.label}
              </option>
            ))}
          </select>
          <input
            aria-label="cron (UTC)"
            placeholder="m h dom mon dow"
            value={choice.cron}
            onChange={(e) => onChange({ ...choice, cron: e.target.value })}
            className="w-32 rounded border border-border-faint bg-surface-container-lowest px-1.5 py-0.5 text-on-surface"
          />
          <span>cron runs in UTC</span>
        </>
      )}
      {choice.when !== "now" && mode === "ask" && (
        <span data-testid="scheduled-ask-note">
          a scheduled ask is not queued: if {peer.display_name || peer.name} is offline when it fires, it fails
        </span>
      )}
    </div>
  );
}

/** This peer's pending schedules from the dashboard, with cancel. */
export function PeerSchedules({ peer, apiBase, refresh }: { peer: Peer; apiBase: string; refresh: number }) {
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [reloads, setReloads] = useState(0);

  useEffect(() => {
    let live = true;
    fetch(`${apiBase}/schedules?from_peer=dashboard`, { credentials: "include" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`Error ${res.status}`);
        const data = (await res.json()) as { schedules: Schedule[] };
        // A one-shot whose time has passed has fired (the daemon drops it
        // then): read as of this fetch, so the list never shows a send that
        // already happened.
        const now = Date.now();
        const mine = (data.schedules ?? []).filter(
          (s) => (s.to_peer === peer.peer_id || s.to_peer === peer.name) && (s.cron || new Date(s.fire_at).getTime() > now),
        );
        if (live) {
          setSchedules(mine);
          setError(null);
        }
      })
      .catch((e: unknown) => live && setError(e instanceof Error ? e.message : "Request failed"));
    return () => {
      live = false;
    };
  }, [apiBase, peer.peer_id, peer.name, refresh, reloads]);

  const cancel = useCallback(
    async (id: string) => {
      const res = await fetch(`${apiBase}/schedules/${encodeURIComponent(id)}`, { method: "DELETE", credentials: "include" });
      // 404: a one-shot that fired since the list was read (the daemon drops
      // it then); the reload removes it, which is all a cancel could do.
      if (!res.ok && res.status !== 404) setError(`Cancel failed: ${res.status}`);
      setReloads((n) => n + 1);
    },
    [apiBase],
  );

  if (error) return <div className="mt-2 font-mono text-[10px] text-error">schedules: {error}</div>;
  if (schedules.length === 0) return null;
  return (
    <ul className="mt-2 flex flex-col gap-1" aria-label="scheduled sends">
      {schedules.map((s) => (
        <li
          key={s.schedule_id}
          className="flex items-center gap-2 border border-border-faint bg-surface-container-lowest px-2 py-1 font-mono text-[11px] text-on-surface-variant"
        >
          <Clock className="h-3 w-3 shrink-0 text-outline" aria-hidden />
          <span className="shrink-0 uppercase tracking-[0.12em] text-outline">{s.kind}</span>
          <span className="min-w-0 flex-1 truncate">{s.text}</span>
          <span className="shrink-0 text-outline">{s.cron ? `${s.cron} UTC · next ${formatFire(s.fire_at)}` : formatFire(s.fire_at)}</span>
          <button
            onClick={() => void cancel(s.schedule_id)}
            aria-label={`Cancel scheduled ${s.kind}`}
            className="shrink-0 text-outline hover:text-error"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </li>
      ))}
    </ul>
  );
}
