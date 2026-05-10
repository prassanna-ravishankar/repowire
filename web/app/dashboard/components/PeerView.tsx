import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { AlertCircle, Check, Copy, Paperclip, RefreshCw, Send, X } from "lucide-react";
import { cn, shortPath, statusDot } from "../lib/utils";
import type { Event, Peer } from "../types";
import { peerLabel } from "../types";
import { formatTime, StatusLabel } from "./status";

type ComposeMode = "notify" | "ask";

export function PeerView({
  peer,
  events,
  apiBase,
  onClose,
  onSent,
}: {
  peer: Peer;
  events: Event[];
  apiBase: string;
  onClose: () => void;
  onSent: () => void;
}) {
  const scrollerRef = useRef<HTMLDivElement>(null);
  const thread = useMemo(() => {
    const id = peer.peer_id;
    return events
      .filter((event) => {
        if (event.type === "chat_turn") return event.peer_id === id;
        return event.from_peer_id === id || event.to_peer_id === id;
      })
      .sort((a, b) => a.timestamp.localeCompare(b.timestamp));
  }, [events, peer.peer_id]);

  useEffect(() => {
    const scroller = scrollerRef.current;
    if (scroller) scroller.scrollTop = scroller.scrollHeight;
  }, [thread.length]);

  const { folder, parent } = peer.path ? shortPath(peer.path) : { folder: "", parent: "" };

  return (
    <>
      <div className="flex items-center gap-3 border-b border-border-faint px-4 py-3 md:px-6">
        <span className={cn("h-2.5 w-2.5 rounded-full", statusDot(peer.status))} />
        <div className="min-w-0 flex-1">
          <h1 className="truncate font-headline text-lg font-bold text-on-surface">{peerLabel(peer)}</h1>
          <div className="mt-1 truncate font-mono text-[11px] text-outline">
            {peer.backend || "agent"} · {peer.metadata?.branch ? String(peer.metadata.branch) : peer.circle}
            {peer.path ? <> · {parent}<span className="text-on-surface-variant">{folder}</span></> : null}
          </div>
        </div>
        <StatusLabel status={peer.status} />
        <CopyPeerName peer={peer} />
        <button
          onClick={onClose}
          aria-label="Close peer"
          className="flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {peer.description && (
        <div className="border-b border-border-faint px-4 py-2 font-mono text-xs text-outline md:px-6">
          <span className="text-primary/70">&gt;</span> {peer.description}
        </div>
      )}

      <div ref={scrollerRef} className="min-h-0 flex-1 overflow-y-auto px-4 py-4 md:px-6">
        {thread.length === 0 ? (
          <div className="py-10 font-mono text-xs leading-6 text-outline">
            &gt; no messages with {peerLabel(peer)}.<br />
            <span>send one to begin a query.</span>
          </div>
        ) : (
          thread.map((event) => <ThreadItem key={event.id} event={event} peer={peer} />)
        )}
      </div>

      <ComposeBar peer={peer} apiBase={apiBase} onSent={onSent} />
    </>
  );
}

function ThreadItem({ event, peer }: { event: Event; peer: Peer }) {
  if (event.type === "chat_turn") {
    const isUser = event.role === "user";
    return (
      <div className={cn("mb-4 flex flex-col", isUser ? "items-end" : "items-start")}>
        <div className="mb-1 font-mono text-[10px] font-medium uppercase tracking-[0.14em] text-outline">
          {isUser ? "@dashboard" : peerLabel(peer)} · {formatTime(event.timestamp)}
        </div>
        <div
          className={cn(
            "max-w-[82%] rounded p-3 font-mono text-[13px] leading-6 text-on-surface",
            isUser
              ? "border-r-2 border-primary bg-primary/10"
              : "border-l-2 border-primary/70 bg-surface-container-high"
          )}
        >
          {isUser ? (
            <p className="whitespace-pre-wrap">{event.text}</p>
          ) : (
            <div className="prose prose-invert prose-sm max-w-none">
              <ReactMarkdown remarkPlugins={[remarkGfm]}>{event.text}</ReactMarkdown>
            </div>
          )}
        </div>
        {!isUser && event.tool_calls && event.tool_calls.length > 0 && (
          <ToolCallBlock toolCalls={event.tool_calls} />
        )}
      </div>
    );
  }

  const label =
    event.type === "query"
      ? `query ${event.from} -> ${event.to}`
      : event.type === "response"
      ? `response ${event.from} -> ${event.to}`
      : event.type === "notification"
      ? `notify ${event.from} -> ${event.to}`
      : `broadcast from ${event.from}`;

  return (
    <div className="mb-2 flex items-start gap-2 font-mono text-xs text-outline">
      <span className="shrink-0 tabular-nums">{formatTime(event.timestamp)}</span>
      <span className="text-on-surface-variant">{label}</span>
      <span className="truncate">{event.text}</span>
    </div>
  );
}

function ToolCallBlock({ toolCalls }: { toolCalls: { name: string; input: string }[] }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="mt-2 w-full max-w-[82%]">
      <button
        onClick={() => setExpanded((value) => !value)}
        className="border border-border-faint bg-surface-container-low px-2.5 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-primary"
      >
        {toolCalls.length} tool call{toolCalls.length === 1 ? "" : "s"}
      </button>
      {expanded && (
        <div className="mt-1 space-y-2 border border-border-faint bg-surface-dim p-3 font-mono text-xs">
          {toolCalls.map((toolCall, index) => (
            <div key={`${toolCall.name}-${index}`}>
              <div><span className="text-secondary">invoke</span> <span className="text-primary-fixed">{toolCall.name}</span></div>
              <div className="truncate text-outline">{toolCall.input}</div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ComposeBar({ peer, apiBase, onSent }: { peer: Peer; apiBase: string; onSent?: () => void }) {
  const [text, setText] = useState("");
  const [mode, setMode] = useState<ComposeMode>("notify");
  const [isPending, setIsPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [response, setResponse] = useState<string | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 120)}px`;
  }, [text]);

  const uploadFile = async (upload: File): Promise<string | null> => {
    const formData = new FormData();
    formData.append("file", upload);
    try {
      const res = await fetch(`${apiBase}/attachments`, {
        method: "POST",
        body: formData,
        credentials: "include",
      });
      if (!res.ok) return null;
      const data = await res.json();
      return data.path as string;
    } catch {
      return null;
    }
  };

  const submit = async () => {
    if ((!text.trim() && !file) || isPending) return;
    setError(null);
    setResponse(null);
    setIsPending(true);

    try {
      let msg = text.trim();
      const hint = "\n(from @dashboard - reply naturally, dashboard sees your response automatically)";
      if (file) {
        const path = await uploadFile(file);
        if (!path) {
          setError("Failed to upload file");
          return;
        }
        msg = msg ? `${msg}\n[Attachment: ${path}]` : `[Attachment: ${path}]`;
      }

      const endpoint = mode === "notify" ? "notify" : "query";
      const res = await fetch(`${apiBase}/${endpoint}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          from_peer: "dashboard",
          to_peer: peer.name,
          text: msg + hint,
          bypass_circle: true,
        }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.error) {
        setError(data.detail || data.error || `Error ${res.status}`);
        return;
      }

      if (mode === "ask") setResponse(data.text ?? null);
      setText("");
      setFile(null);
      onSent?.();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setIsPending(false);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      submit();
    }
  };

  return (
    <div className="border-t border-border-faint bg-surface-dim p-3 md:p-4">
      <div className="mb-2 flex items-center gap-2">
        <div className="inline-flex border border-border-faint bg-surface-container-lowest p-1">
          {(["notify", "ask"] as const).map((item) => (
            <button
              key={item}
              onClick={() => setMode(item)}
              className={cn(
                "px-3 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] transition-colors",
                mode === item ? "bg-primary text-on-primary" : "text-outline hover:text-on-surface"
              )}
            >
              {item === "notify" ? "Notify" : "Query"}
            </button>
          ))}
        </div>
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-outline">to {peerLabel(peer)}</span>
      </div>

      {file && (
        <div className="mb-2 flex items-center gap-2 border border-border-faint bg-surface-container-lowest px-2 py-1.5 font-mono text-xs text-on-surface-variant">
          <Paperclip className="h-3.5 w-3.5" />
          <span className="min-w-0 flex-1 truncate">{file.name}</span>
          <span className="text-outline">{(file.size / 1024).toFixed(0)}KB</span>
          <button onClick={() => setFile(null)} aria-label="Remove attachment" className="text-outline hover:text-on-surface">
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      )}

      <div className="flex items-end gap-3">
        <button
          onClick={() => fileRef.current?.click()}
          aria-label="Attach file"
          className="flex h-10 w-10 shrink-0 items-center justify-center rounded text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
        >
          <Paperclip className="h-4 w-4" />
        </button>
        <input
          ref={fileRef}
          type="file"
          accept="image/*,.pdf,.txt,.json,.csv,.md"
          className="hidden"
          onChange={(event) => {
            if (event.target.files?.[0]) setFile(event.target.files[0]);
            event.target.value = "";
          }}
        />
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(event) => setText(event.target.value)}
          onKeyDown={onKeyDown}
          placeholder={`ask ${peerLabel(peer)} something...`}
          rows={1}
          className="max-h-32 min-h-10 flex-1 resize-none rounded border border-border-faint bg-surface-container-lowest px-3 py-2.5 font-mono text-sm text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary"
        />
        <button
          onClick={submit}
          disabled={(!text.trim() && !file) || isPending}
          aria-label={mode === "notify" ? "Send message" : "Ask peer"}
          className={cn(
            "flex h-10 w-10 shrink-0 items-center justify-center rounded transition-[filter,transform] active:scale-[0.98]",
            text.trim() || file ? "bg-primary text-on-primary hover:brightness-110" : "bg-surface-container-high text-outline"
          )}
        >
          {isPending ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
        </button>
      </div>

      {error && (
        <div className="mt-2 flex items-center gap-2 px-1 font-mono text-xs text-error">
          <AlertCircle className="h-3.5 w-3.5 shrink-0" />
          <span className="flex-1">{error}</span>
          <button onClick={submit} className="border border-error/30 px-2 py-0.5 text-[10px] uppercase">Retry</button>
        </div>
      )}
      {response && (
        <div className="mt-2 max-h-24 overflow-y-auto border border-border-faint bg-surface-container-lowest p-2 font-mono text-xs whitespace-pre-wrap text-on-surface-variant">
          {response}
        </div>
      )}
    </div>
  );
}

function CopyPeerName({ peer }: { peer: Peer }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        navigator.clipboard.writeText(peer.name);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      title="Copy peer name"
      className="hidden h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface sm:inline-flex"
    >
      {copied ? <Check className="h-3.5 w-3.5 text-secondary" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  );
}
