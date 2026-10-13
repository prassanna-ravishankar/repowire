"use client";

import { useCallback, useEffect, useState } from "react";
import type { InboxItem, InboxReply } from "../types";

/** The dashboard's name as an inbox surface. */
export const DASHBOARD_SURFACE = "dashboard";

export interface InboxState {
  items: InboxItem[];
  unread: number;
  loading: boolean;
  /** null when the inbox loaded; "unavailable" when the daemon has no human inbox. */
  error: string | null;
  markRead: (id: string) => void;
  reply: (id: string, body: InboxReply) => Promise<{ action?: string; error?: string }>;
}

/**
 * The human's inbox, newest first. It is read once and again whenever
 * `signal` changes; the page bumps it on every inbox event from the event
 * stream, so the list follows every surface without polling.
 */
export function useInbox(apiBase: string, signal: number): InboxState {
  const [items, setItems] = useState<InboxItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await fetch(`${apiBase}/inbox?limit=100`);
      if (res.status === 503) {
        setError("unavailable");
        return;
      }
      if (!res.ok) {
        setError(`Error ${res.status}`);
        return;
      }
      const data = (await res.json()) as { items?: InboxItem[] };
      setItems(data.items ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load inbox");
    } finally {
      setLoading(false);
    }
  }, [apiBase]);

  useEffect(() => {
    void load();
  }, [load, signal]);

  const markRead = useCallback(
    (id: string) => {
      setItems((current) => current.map((item) => (item.id === id ? { ...item, read: true } : item)));
      void fetch(`${apiBase}/inbox/${encodeURIComponent(id)}/read`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ surface: DASHBOARD_SURFACE }),
      }).catch((err) => console.error("Failed to mark inbox item read:", err));
    },
    [apiBase],
  );

  const reply = useCallback(
    async (id: string, body: InboxReply) => {
      try {
        const res = await fetch(`${apiBase}/inbox/${encodeURIComponent(id)}/reply`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ surface: DASHBOARD_SURFACE, ...body }),
        });
        const data = await res.json().catch(() => null);
        if (!res.ok) {
          return { error: typeof data?.detail === "string" ? data.detail : `Error ${res.status}` };
        }
        await load();
        return { action: typeof data?.action === "string" ? data.action : undefined };
      } catch (err) {
        return { error: err instanceof Error ? err.message : "Reply failed" };
      }
    },
    [apiBase, load],
  );

  const unread = items.filter((item) => !item.read).length;
  return { items, unread, loading, error, markRead, reply };
}
