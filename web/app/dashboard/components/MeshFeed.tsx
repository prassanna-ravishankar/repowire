import { useEffect, useMemo, useRef } from "react";
import { cn } from "../lib/utils";
import type { Event, Peer } from "../types";
import { peerLabel } from "../types";
import { formatTime } from "./status";

export function MeshFeed({ events, peers, onPickPeer }: { events: Event[]; peers: Peer[]; onPickPeer: (peer: Peer) => void }) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const feedEvents = useMemo(
    () => events.filter((event) => event.type !== "chat_turn").sort((a, b) => a.timestamp.localeCompare(b.timestamp)),
    [events]
  );

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [feedEvents.length]);

  const pickPeerByName = (name?: string) => {
    if (!name) return;
    const normalized = name.replace(/^@/, "");
    const peer = peers.find((item) => item.name === normalized || peerLabel(item) === normalized || `@${item.name}` === name);
    if (peer) onPickPeer(peer);
  };

  return (
    <>
      <div className="sticky top-[var(--topbar-offset)] z-10 flex items-baseline justify-between border-b border-border-faint bg-surface-dim px-4 py-3 md:static md:px-6">
        <div>
          <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.22em] text-primary">LIVE / mesh.log</div>
          <h1 className="mt-1 font-headline text-2xl font-bold text-on-surface">tail -f</h1>
        </div>
        <div className="text-right font-mono text-[11px] leading-5 text-outline">
          {feedEvents.length} events<br />
          <span className="text-outline">select a peer to chat ↳</span>
        </div>
      </div>
      <div className="min-h-0 flex-1 bg-surface-dim px-4 py-3 md:overflow-y-auto md:px-5">
        {feedEvents.length === 0 ? (
          <div className="py-14 text-center font-mono text-xs leading-6 text-outline">
            <div className="text-on-surface-variant">&gt; no mesh events yet</div>
            send a message to start the log
          </div>
        ) : (
          feedEvents.map((event) => (
            <EventRow key={event.id} event={event} onPickPeer={pickPeerByName} />
          ))
        )}
        <div ref={bottomRef} />
      </div>
    </>
  );
}

function EventRow({ event, onPickPeer }: { event: Event; onPickPeer: (name?: string) => void }) {
  const route = event.type === "broadcast" ? "=>" : event.type === "response" ? "↳" : "->";
  const color =
    event.status === "error"
      ? "text-error"
      : event.type === "query"
      ? "text-primary-fixed"
      : event.type === "response"
      ? "text-secondary"
      : event.type === "notification"
      ? "text-tertiary-fixed-dim"
      : "text-accent";
  const to = event.type === "broadcast" ? "* (all)" : event.to || "—";

  if (event.type === "status_change") {
    return (
      <div className="grid grid-cols-[62px_1fr] gap-3 border-b border-border-faint/70 py-1.5 font-mono text-xs leading-5">
        <span className="text-outline tabular-nums">{formatTime(event.timestamp)}</span>
        <span className="truncate text-outline">
          status {event.peer || event.peer_id || "peer"} {">"} <span className="text-on-surface-variant">{event.new_status}</span>
        </span>
      </div>
    );
  }

  return (
    <div className="border-b border-border-faint/70 py-1.5 font-mono text-xs leading-5 md:grid md:grid-cols-[62px_minmax(70px,120px)_18px_minmax(70px,120px)_1fr] md:gap-3">
      <div className="flex items-center gap-2 md:contents">
        <span className="shrink-0 text-outline tabular-nums">{formatTime(event.timestamp)}</span>
        <button onClick={() => onPickPeer(event.from)} className={cn("min-w-0 truncate text-left", color)}>
          {event.from || "unknown"}
        </button>
        <span className="shrink-0 text-center text-outline">{route}</span>
        <button onClick={() => onPickPeer(event.to)} className="min-w-0 truncate text-left text-primary-fixed">
          {to}
        </button>
      </div>
      <span className={cn("mt-0.5 block min-w-0 break-words [overflow-wrap:anywhere] md:mt-0", event.status === "error" ? "text-error" : "text-on-surface-variant")}>
        {event.text}
      </span>
    </div>
  );
}
