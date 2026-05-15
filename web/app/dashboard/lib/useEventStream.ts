"use client";

import { useEffect, useRef, useState } from "react";
import type { Event } from "../types";

/**
 * Connection state for the SSE event stream.
 *
 *   live          - EventSource is open
 *   reconnecting  - dropped; waiting `retryInSec` before next attempt
 *   disconnected  - initial state, or never connected
 */
export type ConnectionState =
  | { status: "live" }
  | { status: "reconnecting"; retryInSec: number }
  | { status: "disconnected" };

interface UseEventStreamOptions {
  apiBase: string;
  /** Called for every newly-seen event (already deduped by id). */
  onEvent: (event: Event) => void;
  /** Called after a successful reconnect once gap recovery completes. */
  onReconnected?: () => void;
  /** Last-seen event id getter (a ref-like accessor, so the manager always reads fresh). */
  getLastSeenId: () => string | null;
}

const INITIAL_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 30_000;
const JITTER_RATIO = 0.2;

function nextBackoff(prev: number): number {
  const grown = Math.min(prev * 2, MAX_BACKOFF_MS);
  const jitter = grown * JITTER_RATIO * (Math.random() * 2 - 1);
  return Math.max(500, grown + jitter);
}

function isValidEvent(value: unknown): value is Event {
  if (typeof value !== "object" || value === null) return false;
  const v = value as Record<string, unknown>;
  return (
    typeof v.id === "string" &&
    typeof v.type === "string" &&
    typeof v.timestamp === "string"
  );
}

/**
 * Subscribe to /events/stream with exponential backoff reconnection and
 * gap recovery via /events?since=<last_seen_id> on each reconnect.
 *
 * Returns the current connection state for UI rendering.
 */
export function useEventStream({
  apiBase,
  onEvent,
  onReconnected,
  getLastSeenId,
}: UseEventStreamOptions): ConnectionState {
  const [state, setState] = useState<ConnectionState>({ status: "disconnected" });
  const onEventRef = useRef(onEvent);
  const onReconnectedRef = useRef(onReconnected);
  const getLastSeenIdRef = useRef(getLastSeenId);

  // Keep callback refs current without re-running the effect (which would
  // close and re-open the SSE connection on every render).
  useEffect(() => {
    onEventRef.current = onEvent;
  }, [onEvent]);
  useEffect(() => {
    onReconnectedRef.current = onReconnected;
  }, [onReconnected]);
  useEffect(() => {
    getLastSeenIdRef.current = getLastSeenId;
  }, [getLastSeenId]);

  useEffect(() => {
    let disposed = false;
    let source: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let countdownTimer: ReturnType<typeof setInterval> | null = null;
    let backoffMs = INITIAL_BACKOFF_MS;
    let hasConnected = false;

    const clearTimers = () => {
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
      }
      if (countdownTimer) {
        clearInterval(countdownTimer);
        countdownTimer = null;
      }
    };

    const recoverGap = async () => {
      const lastSeen = getLastSeenIdRef.current();
      if (!lastSeen) return;
      try {
        const res = await fetch(`${apiBase}/events?since=${encodeURIComponent(lastSeen)}`);
        if (!res.ok) return;
        const missed: unknown = await res.json();
        if (!Array.isArray(missed)) return;
        for (const item of missed) {
          if (isValidEvent(item)) onEventRef.current(item);
        }
      } catch {
        // Network errors during gap fetch are non-fatal — the live stream
        // will resume; we just miss whatever happened during the outage.
      }
    };

    const connect = () => {
      if (disposed) return;
      clearTimers();
      source = new EventSource(`${apiBase}/events/stream`);

      source.onopen = () => {
        backoffMs = INITIAL_BACKOFF_MS;
        setState({ status: "live" });
        if (hasConnected) {
          void recoverGap().then(() => onReconnectedRef.current?.());
        }
        hasConnected = true;
      };

      source.onmessage = (e) => {
        try {
          const parsed: unknown = JSON.parse(e.data);
          if (isValidEvent(parsed)) onEventRef.current(parsed);
        } catch (err) {
          console.error("Failed to parse SSE event:", err);
        }
      };

      source.onerror = () => {
        if (disposed) return;
        source?.close();
        source = null;

        const delay = backoffMs;
        backoffMs = nextBackoff(backoffMs);

        const startedAt = Date.now();
        const tick = () => {
          const remaining = Math.max(0, delay - (Date.now() - startedAt));
          setState({ status: "reconnecting", retryInSec: Math.ceil(remaining / 1000) });
        };
        tick();
        countdownTimer = setInterval(tick, 500);
        reconnectTimer = setTimeout(() => {
          clearTimers();
          connect();
        }, delay);
      };
    };

    connect();

    return () => {
      disposed = true;
      clearTimers();
      source?.close();
    };
  }, [apiBase]);

  return state;
}
