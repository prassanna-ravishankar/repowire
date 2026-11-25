import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PeerMessages } from "./PeerMessages";
import type { Peer } from "../types";

const PEER: Peer = {
  peer_id: "repow-dev-bbbb",
  name: "beta",
  display_name: "beta",
  status: "online",
  machine: "host",
  path: "/tmp/beta",
  circle: "dev",
};

const LISTED = {
  message_id: "msg-1a2b3c4d",
  kind: "broadcast",
  from: "alpha",
  preview: "deploy at noon",
  chars: 40,
  created_at: "2026-11-25T12:00:00Z",
  read_at: null,
};

function json(body: unknown, status = 200) {
  return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
}

afterEach(() => vi.unstubAllGlobals());

describe("PeerMessages", () => {
  it("lists the peer's messages and opens one without marking it read", async () => {
    const fetchMock = vi.fn((url: string, _init?: RequestInit) => {
      void _init;
      if (url.startsWith("/api/messages?")) return json({ delivery: "pull", messages: [LISTED] });
      if (url.startsWith("/api/messages/msg-1a2b3c4d?")) return json({ ...LISTED, text: "deploy at noon\nfreeze merges" });
      return json({}, 404);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<PeerMessages peer={PEER} apiBase="/api" />);

    expect(await screen.findByText("deploy at noon")).toBeTruthy();
    expect(screen.getByText("pull")).toBeTruthy();
    expect(screen.getByLabelText("unread")).toBeTruthy();
    const listURL = fetchMock.mock.calls[0][0] as string;
    expect(listURL).toContain("peer=repow-dev-bbbb");
    expect(listURL).not.toContain("unread=");

    fireEvent.click(screen.getByRole("button", { expanded: false }));
    expect(await screen.findByText(/freeze merges/)).toBeTruthy();
    const opened = fetchMock.mock.calls.map((c) => c[0] as string).find((u) => u.startsWith("/api/messages/msg-1a2b3c4d"));
    // A GET, never POST /messages/{id}/read: the dashboard only looks.
    expect(opened).toContain("peer=repow-dev-bbbb");
    for (const call of fetchMock.mock.calls) {
      expect((call[1] as RequestInit | undefined)?.method ?? "GET").toBe("GET");
      expect(call[0] as string).not.toContain("/read");
    }
  });

  it("filters to unread and says when nothing matches", async () => {
    const fetchMock = vi.fn<(url: string) => Promise<Response>>(() => json({ delivery: "push", messages: [] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<PeerMessages peer={PEER} apiBase="/api" />);
    expect(await screen.findByText(/no messages for/)).toBeTruthy();

    fireEvent.click(screen.getByLabelText("unread only"));
    await waitFor(() => expect(fetchMock.mock.calls.some((c) => (c[0] as string).includes("unread=1"))).toBe(true));
    expect(await screen.findByText(/no unread messages for/)).toBeTruthy();
  });

  it("explains a daemon without a message record", async () => {
    vi.stubGlobal("fetch", vi.fn(() => json({ detail: "unavailable" }, 503)));
    render(<PeerMessages peer={PEER} apiBase="/api" />);
    expect(await screen.findByText("messages are not recorded on this daemon")).toBeTruthy();
  });
});
