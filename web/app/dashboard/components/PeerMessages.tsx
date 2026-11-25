import { useEffect, useState } from "react";
import { AlertCircle, Paperclip, RefreshCw } from "lucide-react";
import { cn } from "../lib/utils";
import type { Peer } from "../types";
import { peerLabel } from "../types";
import { formatTime } from "./status";

// MessageView mirrors hub.MessageView: a listing carries the preview, a
// fetched message also its text.
export interface MessageView {
  message_id: string;
  kind: string;
  from: string;
  from_role?: string;
  correlation_id?: string;
  preview: string;
  chars: number;
  text?: string;
  attachments?: { id?: string; filename?: string; path?: string }[];
  created_at: string;
  read_at: string | null;
}

interface MessageList {
  delivery: string;
  messages: MessageView[];
}

const PAGE = 50;

// PeerMessages lists the messages a peer received (GET /messages?peer=) and
// opens one with GET /messages/{id}, which leaves it unread: looking from the
// dashboard never changes what the peer itself sees as new.
export function PeerMessages({ peer, apiBase }: { peer: Peer; apiBase: string }) {
  const [unreadOnly, setUnreadOnly] = useState(false);
  const [refreshes, setRefreshes] = useState(0);
  // The latest answer, tagged with the request it answers; the previous list
  // stays on screen while a new request is in flight.
  const [result, setResult] = useState<{ key: string; list: MessageList | null; error: string | null } | null>(null);
  const requestKey = `${unreadOnly}|${refreshes}`;
  const loading = result?.key !== requestKey;

  useEffect(() => {
    let live = true;
    const params = new URLSearchParams({ peer: peer.peer_id, limit: String(PAGE) });
    if (unreadOnly) params.set("unread", "1");
    const answer = (list: MessageList | null, error: string | null) => {
      if (live) setResult({ key: requestKey, list, error });
    };
    fetch(`${apiBase}/messages?${params}`, { credentials: "include" })
      .then(async (res) => {
        if (!res.ok) {
          answer(null, res.status === 503 ? "messages are not recorded on this daemon" : `Error ${res.status}`);
          return;
        }
        answer((await res.json()) as MessageList, null);
      })
      .catch((e: unknown) => answer(null, e instanceof Error ? e.message : "Request failed"));
    return () => {
      live = false;
    };
  }, [apiBase, peer.peer_id, unreadOnly, requestKey]);

  const list = result?.list ?? null;
  const error = result?.error ?? null;
  const messages = list?.messages ?? [];
  return (
    <div className="min-h-0 flex-1 px-4 py-4 md:overflow-y-auto md:px-6">
      <div className="mb-3 flex items-center gap-3 font-mono text-[11px] text-outline">
        {list && (
          <span>
            delivery: <span className="text-on-surface">{list.delivery}</span>
          </span>
        )}
        <label className="flex cursor-pointer items-center gap-1.5">
          <input type="checkbox" checked={unreadOnly} onChange={(e) => setUnreadOnly(e.target.checked)} />
          unread only
        </label>
        <button
          type="button"
          onClick={() => setRefreshes((n) => n + 1)}
          disabled={loading}
          aria-label="Refresh messages"
          className="ml-auto flex items-center gap-1 hover:text-on-surface disabled:opacity-50"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", loading && "animate-spin")} />
          refresh
        </button>
      </div>
      {error && (
        <div className="mb-2 flex items-center gap-2 font-mono text-xs text-error">
          <AlertCircle className="h-3.5 w-3.5" />
          <span>{error}</span>
        </div>
      )}
      {list && messages.length === 0 && (
        <div className="py-10 font-mono text-xs text-outline">
          &gt; no {unreadOnly ? "unread " : ""}messages for {peerLabel(peer)}.
        </div>
      )}
      {!list && loading && <div className="py-10 font-mono text-xs text-outline">&gt; loading…</div>}
      <ul className="flex flex-col gap-1.5">
        {messages.map((m) => (
          <MessageRow key={m.message_id} message={m} peer={peer} apiBase={apiBase} />
        ))}
      </ul>
      {messages.length === PAGE && (
        <div className="py-2 font-mono text-[10px] text-outline">showing the newest {PAGE}</div>
      )}
    </div>
  );
}

function MessageRow({ message, peer, apiBase }: { message: MessageView; peer: Peer; apiBase: string }) {
  const [open, setOpen] = useState(false);
  const [body, setBody] = useState<MessageView | null>(null);
  const [error, setError] = useState<string | null>(null);

  const toggle = async () => {
    const next = !open;
    setOpen(next);
    if (!next || body) return;
    setError(null);
    try {
      const params = new URLSearchParams({ peer: peer.peer_id });
      const res = await fetch(`${apiBase}/messages/${encodeURIComponent(message.message_id)}?${params}`, {
        credentials: "include",
      });
      if (!res.ok) {
        setError(res.status === 404 ? "this message has expired" : `Error ${res.status}`);
        return;
      }
      setBody((await res.json()) as MessageView);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    }
  };

  const unread = message.read_at === null;
  const attachments = body?.attachments ?? message.attachments ?? [];
  return (
    <li className="rounded border border-border-faint bg-surface-container-high">
      <button
        type="button"
        onClick={() => void toggle()}
        aria-expanded={open}
        className="flex w-full items-baseline gap-2 px-3 py-2 text-left font-mono text-xs"
      >
        <span
          aria-label={unread ? "unread" : "read"}
          className={cn("h-1.5 w-1.5 shrink-0 self-center rounded-full", unread ? "bg-primary" : "bg-transparent")}
        />
        <span className="shrink-0 uppercase tracking-[0.12em] text-outline">{message.kind}</span>
        <span className="shrink-0 text-on-surface">@{message.from.replace(/^@/, "")}</span>
        <span className="min-w-0 flex-1 truncate text-outline">{message.preview}</span>
        {(message.attachments?.length ?? 0) > 0 && <Paperclip className="h-3 w-3 shrink-0 text-outline" />}
        <span className="shrink-0 tabular-nums text-outline">{formatTime(message.created_at)}</span>
      </button>
      {open && (
        <div className="border-t border-border-faint px-3 py-2 font-mono text-[12px] leading-5">
          <div className="mb-1 text-[10px] text-outline">
            {message.message_id}
            {message.correlation_id ? ` · ${message.correlation_id}` : ""} · {message.chars} chars
          </div>
          {error && <div className="text-error">{error}</div>}
          {!error && !body && <div className="text-outline">loading…</div>}
          {body && <p className="whitespace-pre-wrap break-words text-on-surface [overflow-wrap:anywhere]">{body.text}</p>}
          {body && attachments.length > 0 && (
            <ul className="mt-2 text-[11px] text-outline">
              {attachments.map((a, i) => (
                <li key={a.id ?? a.path ?? i} className="flex items-center gap-1">
                  <Paperclip className="h-3 w-3" />
                  {a.filename ?? a.path ?? a.id}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  );
}
