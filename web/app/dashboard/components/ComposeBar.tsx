"use client";

import { useState, useRef, useEffect, KeyboardEvent } from "react";
import { RefreshCw, Send } from "lucide-react";
import { cn } from "../lib/utils";
import type { Peer } from "../types";

interface ComposeBarProps {
  peer: Peer;
  apiBase: string;
}

export function ComposeBar({ peer, apiBase }: ComposeBarProps) {
  const [text, setText] = useState("");
  const [mode, setMode] = useState<"notify" | "ask">("notify");
  const [isPending, setIsPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [response, setResponse] = useState<string | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  // Auto-resize textarea
  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 120)}px`;
  }, [text]);

  const submit = async () => {
    if (!text.trim() || isPending) return;
    setError(null);
    setResponse(null);
    setIsPending(true);

    try {
      if (mode === "notify") {
        const res = await fetch(`${apiBase}/notify`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ from_peer: "dashboard", to_peer: peer.name, text: text.trim(), bypass_circle: true }),
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          setError(body.detail || `Error ${res.status}`);
        } else {
          setText("");
        }
      } else {
        const res = await fetch(`${apiBase}/query`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ from_peer: "dashboard", to_peer: peer.name, text: text.trim(), bypass_circle: true }),
        });
        const data = await res.json();
        if (data.error) {
          setError(data.error);
        } else {
          setResponse(data.text ?? null);
          setText("");
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
    <div className="border-t border-zinc-800 bg-zinc-950 p-3 flex flex-col gap-2">
      {/* Controls row */}
      <div className="flex items-center gap-2">
        <span className="text-xs text-zinc-500 font-mono">
          → {peer.name}
        </span>

        <div className="flex rounded-md overflow-hidden border border-zinc-700">
          {(["notify", "ask"] as const).map((m) => (
            <button
              key={m}
              onClick={() => setMode(m)}
              className={cn(
                "px-2.5 py-1 text-xs transition-colors",
                mode === m ? "bg-zinc-700 text-zinc-200" : "bg-zinc-900 text-zinc-500 hover:text-zinc-300"
              )}
            >
              {m}
            </button>
          ))}
        </div>

        <span className="ml-auto text-[10px] text-zinc-600">⌘↵ to send</span>
      </div>

      {/* Textarea + send button */}
      <div className="flex gap-2 items-end">
        <textarea
          ref={textareaRef}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder={mode === "notify" ? "Send a notification..." : "Ask a question..."}
          rows={1}
          className="flex-1 bg-zinc-900 border border-zinc-700 rounded-lg px-3 py-2 text-sm text-zinc-300 placeholder-zinc-600 resize-none focus:outline-none focus:ring-1 focus:ring-zinc-500"
        />
        <button
          onClick={submit}
          disabled={!text.trim() || isPending}
          className="p-2 rounded-lg bg-zinc-700 hover:bg-zinc-600 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          {isPending ? <RefreshCw className="w-4 h-4 text-zinc-300 animate-spin" /> : <Send className="w-4 h-4 text-zinc-300" />}
        </button>
      </div>

      {error && <p className="text-xs text-red-400 font-mono">{error}</p>}
      {response && (
        <div className="text-xs text-zinc-400 bg-zinc-900 border border-zinc-800 rounded-lg p-2 max-h-24 overflow-y-auto font-mono whitespace-pre-wrap">
          {response}
        </div>
      )}
    </div>
  );
}
