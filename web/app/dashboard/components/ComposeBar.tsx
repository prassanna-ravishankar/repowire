"use client";

import { useState, useRef, useEffect, useMemo, KeyboardEvent } from "react";
import { Paperclip, RefreshCw, Send, X, Check, Clock } from "lucide-react";
import { cn } from "../lib/utils";
import type { Event, Peer } from "../types";
import { peerLabel } from "../types";

interface ComposeBarProps {
  peer: Peer;
  apiBase: string;
  events: Event[];
  onSent?: () => void;
}

interface PendingAsk {
  correlation_id: string;
  to_peer: string;
  preview: string;
  sent_at: number;
  state: "pending" | "delivered" | "timed_out";
  reply?: string;
  reply_from?: string;
}

const ACK_FRAME_RE = /^\[ack #([^\]\s]+) from @([^\]\s]+)\]\s?([\s\S]*)$/;
const BARE_ACK_TIMEOUT_MS = 120_000;

export function ComposeBar({ peer, apiBase, events, onSent }: ComposeBarProps) {
  const [text, setText] = useState("");
  const [isPending, setIsPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pendingAsks, setPendingAsks] = useState<PendingAsk[]>([]);
  const [file, setFile] = useState<File | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 120)}px`;
  }, [text]);

  // Match incoming notification events to pending asks via [ack #cid from @peer] framing.
  const openCids = useMemo(
    () => pendingAsks.filter((a) => a.state === "pending").map((a) => a.correlation_id),
    [pendingAsks]
  );
  useEffect(() => {
    if (openCids.length === 0) return;
    for (const ev of events) {
      if (ev.type !== "notification" || !ev.text) continue;
      const m = ev.text.match(ACK_FRAME_RE);
      if (!m) continue;
      const [, cid, from, body] = m;
      if (!openCids.includes(cid)) continue;
      setPendingAsks((prev) =>
        prev.map((a) =>
          a.correlation_id === cid && a.state === "pending"
            ? { ...a, state: "delivered", reply: body, reply_from: from }
            : a
        )
      );
    }
  }, [events, openCids]);

  // Bare-ack soft timeout: flip pending → timed_out after 120s.
  useEffect(() => {
    if (openCids.length === 0) return;
    const timers = openCids.map((cid) => {
      const ask = pendingAsks.find((a) => a.correlation_id === cid);
      if (!ask) return null;
      const elapsed = Date.now() - ask.sent_at;
      const remaining = Math.max(0, BARE_ACK_TIMEOUT_MS - elapsed);
      return window.setTimeout(() => {
        setPendingAsks((prev) =>
          prev.map((a) =>
            a.correlation_id === cid && a.state === "pending"
              ? { ...a, state: "timed_out" }
              : a
          )
        );
      }, remaining);
    });
    return () => {
      for (const t of timers) if (t !== null) window.clearTimeout(t);
    };
  }, [openCids, pendingAsks]);

  const dismissAsk = (cid: string) =>
    setPendingAsks((prev) => prev.filter((a) => a.correlation_id !== cid));

  const uploadFile = async (f: File): Promise<string | null> => {
    const formData = new FormData();
    formData.append("file", f);
    try {
      const res = await fetch(`${apiBase}/attachments`, {
        method: "POST",
        body: formData,
        credentials: "include",
      });
      if (!res.ok) {
        console.error("Upload failed:", res.status, await res.text().catch(() => ""));
        return null;
      }
      const data = await res.json();
      return data.path as string;
    } catch {
      return null;
    }
  };

  const submit = async () => {
    if ((!text.trim() && !file) || isPending) return;
    setError(null);
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

      const res = await fetch(`${apiBase}/ask`, {
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
        setError(data.error || data.detail || `Error ${res.status}`);
      } else if (data.correlation_id) {
        const preview = msg.length > 60 ? msg.slice(0, 60) + "…" : msg;
        setPendingAsks((prev) => [
          ...prev,
          {
            correlation_id: data.correlation_id,
            to_peer: peer.name,
            preview,
            sent_at: Date.now(),
            state: "pending",
          },
        ]);
        setText("");
        setFile(null);
        if (onSent) setTimeout(onSent, 1000);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setIsPending(false);
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      submit();
    }
  };

  return (
    <div className="shrink-0 px-4 pb-4">
      <div className="bg-surface-container-low/95 backdrop-blur-xl border border-outline-variant/30 rounded-xl p-3 shadow-2xl">
        {/* Scope label */}
        <div className="flex items-center gap-2 mb-3">
          <span className="text-[9px] font-mono text-outline uppercase tracking-tighter">
            ask → {peerLabel(peer)}
          </span>
        </div>

        {/* File preview */}
        {file && (
          <div className="flex items-center gap-2 px-2 py-1.5 bg-surface-container-lowest border border-outline-variant/20 rounded mb-2 text-xs text-on-surface-variant">
            <Paperclip className="w-3 h-3 shrink-0" />
            <span className="truncate flex-1">{file.name}</span>
            <span className="text-outline shrink-0">{(file.size / 1024).toFixed(0)}KB</span>
            <button onClick={() => setFile(null)} aria-label="Remove attachment" className="p-0.5 hover:text-on-surface">
              <X className="w-3 h-3" aria-hidden="true" />
            </button>
          </div>
        )}

        {/* Textarea + actions */}
        <div className="relative flex items-end gap-3">
          <button
            onClick={() => fileRef.current?.click()}
            className="p-2 text-outline hover:text-on-surface-variant transition-colors shrink-0"
            title="Attach file"
            aria-label="Attach file"
          >
            <Paperclip className="w-4 h-4" aria-hidden="true" />
          </button>
          <input
            ref={fileRef}
            type="file"
            accept="image/*,.pdf,.txt,.json,.csv,.md"
            className="hidden"
            onChange={(e) => { if (e.target.files?.[0]) setFile(e.target.files[0]); e.target.value = ""; }}
          />
          <textarea
            ref={textareaRef}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder="Send instruction to peer..."
            rows={1}
            className="flex-1 bg-surface-container-lowest border-none focus:ring-1 focus:ring-cyan-400/50 rounded-lg text-sm font-mono py-3 px-4 placeholder:text-slate-600 resize-none max-h-32 text-on-surface outline-none"
          />
          <button
            onClick={submit}
            disabled={(!text.trim() && !file) || isPending}
            aria-label="Ask peer"
            aria-busy={isPending}
            className={cn(
              "w-11 h-11 rounded-lg flex items-center justify-center shadow-lg active:scale-90 transition-transform shrink-0",
              (text.trim() || file)
                ? "bg-gradient-to-br from-primary to-primary-container text-on-primary shadow-cyan-400/20"
                : "bg-surface-container-highest text-outline"
            )}
          >
            {isPending ? <RefreshCw className="w-4 h-4 animate-spin" aria-hidden="true" /> : <Send className="w-4 h-4" aria-hidden="true" />}
          </button>
        </div>
      </div>

      {/* Error */}
      {error && (
        <div className="flex items-center gap-2 mt-2 px-3">
          <p className="text-xs text-error font-mono flex-1">{error}</p>
          <button
            onClick={submit}
            className="text-[10px] px-2 py-0.5 rounded bg-surface-container-highest text-on-surface-variant hover:text-on-surface transition-colors shrink-0"
          >
            Retry
          </button>
        </div>
      )}

      {/* Pending / delivered asks for this peer */}
      {pendingAsks.filter((a) => a.to_peer === peer.name).length > 0 && (
        <div className="flex flex-col gap-1.5 mt-2">
          {pendingAsks
            .filter((a) => a.to_peer === peer.name)
            .map((a) => (
              <div
                key={a.correlation_id}
                className={cn(
                  "border rounded-lg px-3 py-2 text-xs font-mono",
                  a.state === "delivered"
                    ? "bg-surface-container-lowest border-cyan-400/30"
                    : a.state === "timed_out"
                      ? "bg-surface-container-lowest border-outline-variant/20 text-outline"
                      : "bg-surface-container-lowest border-outline-variant/30"
                )}
              >
                <div className="flex items-center gap-2">
                  {a.state === "delivered" ? (
                    <Check className="w-3 h-3 text-cyan-400 shrink-0" aria-hidden="true" />
                  ) : a.state === "timed_out" ? (
                    <Check className="w-3 h-3 text-outline shrink-0" aria-hidden="true" />
                  ) : (
                    <Clock className="w-3 h-3 text-outline shrink-0 animate-pulse" aria-hidden="true" />
                  )}
                  <span className="text-[10px] uppercase tracking-wider text-outline shrink-0">
                    #{a.correlation_id.slice(0, 8)}
                  </span>
                  <span className="text-on-surface-variant truncate flex-1">{a.preview}</span>
                  <span className="text-[10px] text-outline shrink-0">
                    {a.state === "pending"
                      ? "pending"
                      : a.state === "delivered"
                        ? `reply from @${a.reply_from}`
                        : "acked (no reply)"}
                  </span>
                  <button
                    onClick={() => dismissAsk(a.correlation_id)}
                    aria-label="Dismiss"
                    className="p-0.5 text-outline hover:text-on-surface shrink-0"
                  >
                    <X className="w-3 h-3" aria-hidden="true" />
                  </button>
                </div>
                {a.state === "delivered" && a.reply && (
                  <div className="mt-1.5 pl-5 text-on-surface-variant whitespace-pre-wrap max-h-24 overflow-y-auto">
                    {a.reply}
                  </div>
                )}
              </div>
            ))}
        </div>
      )}
    </div>
  );
}
