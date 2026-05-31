"use client";

import { useMemo, useState } from "react";
import { cn } from "../lib/utils";
import type { AskQuestion, Event } from "../types";

interface PendingQuestion {
  correlation_id: string;
  from: string;
  text: string;
  question: AskQuestion;
}

/** Derive still-open questions from the event stream.
 *
 * An `ask` event carrying a `question` opens one; a later `ack` event for the
 * same correlation_id (answered/acked/closed) removes it. This mirrors how the
 * mesh feed derives state from events — no extra polling. */
function derivePending(events: Event[]): PendingQuestion[] {
  const open = new Map<string, PendingQuestion>();
  for (const event of events) {
    const cid = event.correlation_id;
    if (!cid) continue;
    if (event.type === "ask" && event.question && event.question.kind !== "acknowledge") {
      open.set(cid, {
        correlation_id: cid,
        from: event.from ?? "?",
        text: event.text ?? "",
        question: event.question,
      });
    } else if (event.type === "ack") {
      open.delete(cid); // answered / acked / closed
    }
  }
  return Array.from(open.values());
}

export function PendingQuestions({
  events,
  apiBase,
}: {
  events: Event[];
  apiBase: string;
}) {
  const pending = useMemo(() => derivePending(events), [events]);
  const [answering, setAnswering] = useState<Record<string, boolean>>({});

  if (pending.length === 0) return null;

  async function answer(cid: string, body: Record<string, unknown>) {
    setAnswering((prev) => ({ ...prev, [cid]: true }));
    try {
      await fetch(`${apiBase}/answer`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ correlation_id: cid, ...body }),
      });
    } catch (error) {
      console.error("Failed to answer question:", error);
    } finally {
      setAnswering((prev) => ({ ...prev, [cid]: false }));
    }
  }

  return (
    <div className="col-span-full border-b border-tertiary/30 bg-tertiary-container px-3 py-2 text-on-tertiary-container md:px-5">
      <div className="font-mono text-[9px] font-bold uppercase tracking-[0.2em]">
        {pending.length} question{pending.length > 1 ? "s" : ""} awaiting answer
      </div>
      {pending.map((q) => (
        <div key={q.correlation_id} className="mt-1.5 flex flex-wrap items-center gap-2">
          <span className="font-mono text-[11px] text-on-tertiary-container/85">
            <span className="font-semibold">@{q.from}</span> {q.text}
          </span>
          {q.question.kind === "choice" ? (
            (q.question.options ?? []).map((opt) => (
              <button
                key={opt.id}
                disabled={answering[q.correlation_id]}
                onClick={() => answer(q.correlation_id, { option_id: opt.id })}
                title={opt.description ?? undefined}
                className={cn(
                  "rounded border border-tertiary/40 bg-tertiary/15 px-2 py-0.5",
                  "font-mono text-[11px] font-semibold hover:bg-tertiary/25",
                  "disabled:opacity-50",
                )}
              >
                {opt.title}
              </button>
            ))
          ) : (
            <button
              disabled={answering[q.correlation_id]}
              onClick={() => answer(q.correlation_id, { outcome: "acknowledged" })}
              className={cn(
                "rounded border border-tertiary/40 bg-tertiary/15 px-2 py-0.5",
                "font-mono text-[11px] font-semibold hover:bg-tertiary/25 disabled:opacity-50",
              )}
            >
              ✓ Acknowledge
            </button>
          )}
        </div>
      ))}
    </div>
  );
}
