import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import Dashboard from "./page";

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

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

describe("Dashboard inbox", () => {
  afterEach(() => {
    FakeEventSource.instances = [];
    vi.unstubAllGlobals();
  });

  it("rereads the inbox after a reconnect even when no inbox event is replayed", async () => {
    // The item is read on another surface while the dashboard is
    // disconnected, and the read's event is outside the replay window.
    Element.prototype.scrollIntoView = () => {}; // jsdom has no layout
    let readElsewhere = false;
    const inboxFetches: string[] = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url.startsWith("/inbox?")) {
          inboxFetches.push(url);
          return jsonResponse({
            items: [{ id: "inb-1", direction: "in", kind: "notify", from: "orchestrator", text: "deployed",
              created_at: "2026-10-13T10:00:00Z", read: readElsewhere, replied: false }],
          });
        }
        if (url === "/peers") return jsonResponse({ peers: [] });
        if (url.startsWith("/events")) return jsonResponse([{ id: "ev-1", type: "peer_online", timestamp: "2026-10-13T10:00:00Z" }]);
        return jsonResponse({});
      }),
    );

    render(<Dashboard />);
    expect(await screen.findByRole("button", { name: "Inbox, 1 unread" })).toBeInTheDocument();
    act(() => FakeEventSource.instances[0].onopen?.());
    const before = inboxFetches.length;

    act(() => FakeEventSource.instances[0].onerror?.());
    readElsewhere = true;
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2), { timeout: 3000 });
    act(() => FakeEventSource.instances[1].onopen?.());

    await waitFor(() => expect(inboxFetches.length).toBeGreaterThan(before));
    expect(await screen.findByRole("button", { name: "Inbox" })).toBeInTheDocument();
  });
});
