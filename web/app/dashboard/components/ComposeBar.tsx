"use client";

import { useState, useRef, useEffect, KeyboardEvent } from "react";
import { Paperclip, RefreshCw, Send, X } from "lucide-react";
import { cn } from "../lib/utils";
import type { Peer } from "../types";
import { peerLabel } from "../types";

interface ComposeBarProps {
  peer: Peer;
  apiBase: string;
  onSent?: () => void;
}

export function ComposeBar({ peer, apiBase, onSent }: ComposeBarProps) {
  const [text, setText] = useState("");
  const [mode, setMode] = useState<"notify" | "ask">("notify");
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

      if (mode === "notify") {
        const res = await fetch(`${apiBase}/notify`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ from_peer: "dashboard", to_peer: peer.name, text: msg + hint, bypass_circle: true }),
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          setError(body.detail || `Error ${res.status}`);
        } else {
          setText("");
          setFile(null);
          if (onSent) setTimeout(onSent, 1000);
        }
      } else {
        const res = await fetch(`${apiBase}/query`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ from_peer: "dashboard", to_peer: peer.name, text: msg + hint, bypass_circle: true }),
        });
        const data = await res.json();
        if (data.error) {
          setError(data.error);
        } else {
          setResponse(data.text ?? null);
          setText("");
          setFile(null);
          if (onSent) setTimeout(onSent, 1000);
        }
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
        {/* Mode toggle + scope */}
        <div className="flex items-center gap-2 mb-3">
          <div className="flex bg-surface-container-lowest p-1 rounded-lg border border-outline-variant/10">
            {(["notify", "ask"] as const).map((m) => (
              <button
                key={m}
                onClick={() => setMode(m)}
                className={cn(
                  "px-3 py-1 text-[9px] font-bold uppercase tracking-wider rounded transition-colors",
                  mode === m
                    ? "bg-cyan-400 text-on-primary-fixed-variant"
                    : "text-slate-500 hover:text-slate-300"
                )}
              >
                {m === "notify" ? "Notify" : "Query"}
              </button>
            ))}
          </div>
          <div className="h-4 w-[1px] bg-outline-variant/30 mx-1" />
          <span className="text-[9px] font-mono text-outline uppercase tracking-tighter">
            → {peerLabel(peer)}
          </span>
        </div>

        {/* File preview */}
        {file && (
          <div className="flex items-center gap-2 px-2 py-1.5 bg-surface-container-lowest border border-outline-variant/20 rounded mb-2 text-xs text-on-surface-variant">
            <Paperclip className="w-3 h-3 shrink-0" />
            <span className="truncate flex-1">{file.name}</span>
            <span className="text-outline shrink-0">{(file.size / 1024).toFixed(0)}KB</span>
            <button onClick={() => setFile(null)} className="p-0.5 hover:text-on-surface">
              <X className="w-3 h-3" />
            </button>
          </div>
        )}

        {/* Textarea + actions */}
        <div className="relative flex items-end gap-3">
          <button
            onClick={() => fileRef.current?.click()}
            className="p-2 text-outline hover:text-on-surface-variant transition-colors shrink-0"
            title="Attach file"
          >
            <Paperclip className="w-4 h-4" />
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
            className={cn(
              "w-11 h-11 rounded-lg flex items-center justify-center shadow-lg active:scale-90 transition-transform shrink-0",
              (text.trim() || file)
                ? "bg-gradient-to-br from-primary to-primary-container text-on-primary shadow-cyan-400/20"
                : "bg-surface-container-highest text-outline"
            )}
          >
            {isPending ? <RefreshCw className="w-4 h-4 animate-spin" /> : <Send className="w-4 h-4" />}
          </button>
        </div>
      </div>

      {/* Error / Response */}
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
      {response && (
        <div className="text-xs text-on-surface-variant bg-surface-container-lowest border border-outline-variant/20 rounded-lg p-2 mt-2 max-h-24 overflow-y-auto font-mono whitespace-pre-wrap">
          {response}
        </div>
      )}
    </div>
  );
}
