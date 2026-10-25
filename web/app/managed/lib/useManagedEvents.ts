import { useEffect, useRef, useState } from "react";
import { INITIAL_BACKOFF_MS, nextBackoff } from "../../dashboard/lib/useEventStream";
import { MANAGED_EVENTS_URL, type ManagedFrame } from "./api";
import { isFrame } from "./state";

/** `offline` after three consecutive failed attempts; it keeps retrying. */
export type LiveConnection =
  | { status: "connecting" }
  | { status: "live" }
  | { status: "reconnecting"; retryInSec: number }
  | { status: "offline"; retryInSec: number };

const OFFLINE_AFTER = 3;

/**
 * Subscribes to the relay's managed SSE bridge. Each `data:` line is one
 * Worker frame; a fresh snapshot arrives on every (re)connect, so no gap
 * recovery is needed. `event: relay` frames (relay-side daemon connect and
 * disconnect) are passed to `onRelay`. Reconnecting uses the dashboard's
 * backoff; it is not polling.
 */
export function useManagedEvents({
  enabled,
  onFrame,
  onRelay,
}: {
  enabled: boolean;
  onFrame: (frame: ManagedFrame) => void;
  onRelay?: (data: unknown) => void;
}): LiveConnection {
  const [state, setState] = useState<LiveConnection>({ status: "connecting" });
  const onFrameRef = useRef(onFrame);
  const onRelayRef = useRef(onRelay);
  useEffect(() => {
    onFrameRef.current = onFrame;
    onRelayRef.current = onRelay;
  }, [onFrame, onRelay]);

  useEffect(() => {
    if (!enabled) return;
    let disposed = false;
    let source: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let countdown: ReturnType<typeof setInterval> | null = null;
    let backoffMs = INITIAL_BACKOFF_MS;
    let failures = 0;

    const clearTimers = () => {
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (countdown) clearInterval(countdown);
      reconnectTimer = null;
      countdown = null;
    };

    const parse = (data: string): unknown => {
      try {
        return JSON.parse(data);
      } catch {
        return null;
      }
    };

    const connect = () => {
      if (disposed) return;
      clearTimers();
      source = new EventSource(MANAGED_EVENTS_URL, { withCredentials: true });
      source.onopen = () => {
        backoffMs = INITIAL_BACKOFF_MS;
        failures = 0;
        setState({ status: "live" });
      };
      source.onmessage = (event) => {
        const frame = parse(event.data);
        if (isFrame(frame)) onFrameRef.current(frame);
      };
      source.addEventListener("relay", (event) => {
        onRelayRef.current?.(parse((event as MessageEvent<string>).data));
      });
      source.onerror = () => {
        if (disposed) return;
        source?.close();
        source = null;
        failures += 1;
        const delay = backoffMs;
        backoffMs = nextBackoff(backoffMs);
        const startedAt = Date.now();
        const status = failures >= OFFLINE_AFTER ? "offline" : "reconnecting";
        const tick = () => setState({ status, retryInSec: Math.ceil(Math.max(0, delay - (Date.now() - startedAt)) / 1000) });
        tick();
        countdown = setInterval(tick, 500);
        reconnectTimer = setTimeout(connect, delay);
      };
    };

    connect();
    return () => {
      disposed = true;
      clearTimers();
      source?.close();
    };
  }, [enabled]);

  return state;
}
