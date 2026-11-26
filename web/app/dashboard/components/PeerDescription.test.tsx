import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PeerDescription } from "./PeerDescription";
import type { Peer } from "../types";

const PEER: Peer = { peer_id: "peer-1", name: "alice", display_name: "alice", status: "online", machine: "host", path: "/tmp/alice", circle: "default", description: "fixing #12" };

function stubFetch(status = 200, body: unknown = { ok: true }) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }));
  return calls;
}

describe("PeerDescription", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("edits the description through the daemon's route and refreshes the peer", async () => {
    const calls = stubFetch();
    const onSaved = vi.fn();
    render(<PeerDescription peer={PEER} apiBase="" onSaved={onSaved} />);
    expect(screen.getByText("fixing #12")).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("Edit peer description"));
    const input = screen.getByLabelText("Peer description");
    expect(input).toHaveValue("fixing #12");
    fireEvent.change(input, { target: { value: "  reviewing #13  " } });
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
    expect(calls[0].url).toBe("/peers/peer-1/description");
    expect(calls[0].init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ description: "reviewing #13" });
    expect(screen.queryByLabelText("Peer description")).not.toBeInTheDocument();
  });

  it("keeps the editor open with the daemon's error, and Escape cancels without a request", async () => {
    const calls = stubFetch(404, { detail: "Peer not found: peer-1" });
    const onSaved = vi.fn();
    render(<PeerDescription peer={PEER} apiBase="" onSaved={onSaved} />);
    fireEvent.click(screen.getByLabelText("Edit peer description"));
    fireEvent.click(screen.getByText("save"));
    expect(await screen.findByText("Peer not found: peer-1")).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
    fireEvent.keyDown(screen.getByLabelText("Peer description"), { key: "Escape" });
    expect(screen.queryByLabelText("Peer description")).not.toBeInTheDocument();
    expect(calls).toHaveLength(1);
  });

  it("opens the editor when the palette signals, and shows an empty description as such", () => {
    stubFetch();
    const { rerender } = render(<PeerDescription peer={{ ...PEER, description: undefined }} apiBase="" onSaved={() => {}} editSignal={0} />);
    expect(screen.getByText("no description")).toBeInTheDocument();
    expect(screen.queryByLabelText("Peer description")).not.toBeInTheDocument();
    rerender(<PeerDescription peer={{ ...PEER, description: undefined }} apiBase="" onSaved={() => {}} editSignal={1} />);
    expect(screen.getByLabelText("Peer description")).toHaveFocus();
    // A second signal while editing keeps the draft in progress.
    fireEvent.change(screen.getByLabelText("Peer description"), { target: { value: "half typed" } });
    rerender(<PeerDescription peer={{ ...PEER, description: undefined }} apiBase="" onSaved={() => {}} editSignal={2} />);
    expect(screen.getByLabelText("Peer description")).toHaveValue("half typed");
  });
});
