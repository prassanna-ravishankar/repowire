import { useEffect, useMemo, useRef } from "react";
import { Paperclip } from "lucide-react";
import { cn } from "../lib/utils";
import type { AttachmentRef, Event, Peer } from "../types";
import { peerLabel } from "../types";
import { formatTime } from "./status";

export function MeshFeed({
  events,
  peers,
  apiBase,
  onPickPeer,
}: {
  events: Event[];
  peers: Peer[];
  apiBase: string;
  onPickPeer: (peer: Peer) => void;
}) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const feedEvents = useMemo(
    () =>
      events
        .filter((event) => event.type !== "chat_turn" && event.type !== "chat_turn_delta")
        .sort((a, b) => a.timestamp.localeCompare(b.timestamp)),
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
            <EventRow
              key={event.id}
              event={event}
              apiBase={apiBase}
              onPickPeer={pickPeerByName}
            />
          ))
        )}
        <div ref={bottomRef} />
      </div>
    </>
  );
}

function EventRow({
  event,
  apiBase,
  onPickPeer,
}: {
  event: Event;
  apiBase: string;
  onPickPeer: (name?: string) => void;
}) {
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
        <AttachmentChips attachments={event.attachments} apiBase={apiBase} />
      </span>
    </div>
  );
}

function AttachmentChips({
  attachments,
  apiBase,
}: {
  attachments?: AttachmentRef[];
  apiBase: string;
}) {
  if (!attachments || attachments.length === 0) return null;
  return (
    <span className="mt-1 flex flex-wrap gap-1.5">
      {attachments.map((attachment, index) => {
        const label = attachment.filename || attachment.path?.split("/").pop() || attachment.id || "attachment";
        const href = attachment.id ? `${apiBase}/attachments/${encodeURIComponent(attachment.id)}` : undefined;
        const chip = (
          <span className="inline-flex max-w-56 items-center gap-1 truncate border border-border-faint bg-surface-container-low px-1.5 py-0.5 text-[10px] text-outline">
            <Paperclip className="h-3 w-3 shrink-0" aria-hidden="true" />
            <span className="truncate">{label}</span>
          </span>
        );
        return href ? (
          <a key={`${attachment.id}-${index}`} href={href} className="hover:text-on-surface">
            {chip}
          </a>
        ) : (
          <span key={`${label}-${index}`}>{chip}</span>
        );
      })}
    </span>
  );
}
