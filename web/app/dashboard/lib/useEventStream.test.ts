import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useEventStream } from "./useEventStream";

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  close() {}
}

describe("useEventStream", () => {
  afterEach(() => {
    FakeEventSource.instances = [];
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("opens the stream as the dashboard surface and reports reconnects", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("EventSource", FakeEventSource);
    vi.stubGlobal("fetch", vi.fn(async () => new Response("[]")));
    const onReconnected = vi.fn();
    renderHook(() => useEventStream({ apiBase: "", onEvent: () => {}, onReconnected, getLastSeenId: () => null }));

    expect(FakeEventSource.instances[0].url).toBe("/events/stream?surface=dashboard");
    act(() => FakeEventSource.instances[0].onopen?.());
    expect(onReconnected).not.toHaveBeenCalled();

    // Drop and reconnect: even with nothing to replay, the caller hears it.
    act(() => FakeEventSource.instances[0].onerror?.());
    await act(async () => {
      vi.advanceTimersByTime(2000);
    });
    act(() => FakeEventSource.instances[1].onopen?.());
    await act(async () => {
      await Promise.resolve();
    });
    expect(onReconnected).toHaveBeenCalledTimes(1);
  });
});
