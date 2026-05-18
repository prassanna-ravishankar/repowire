import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { PeerView } from "./PeerView";
import { __resetProtectionForTests, isProtected } from "../lib/protection";
import type { Event, Peer } from "../types";

const PEER: Peer = {
  peer_id: "peer-1",
  name: "alice",
  display_name: "alice",
  status: "online",
  machine: "host",
  path: "/tmp/alice",
  circle: "default",
};

function chatTurn(id: string, text: string, ts: string): Event {
  return {
    id,
    type: "chat_turn",
    timestamp: ts,
    peer_id: PEER.peer_id,
    role: "assistant",
    text,
  };
}

const scrollSpy = vi.fn();

beforeEach(() => {
  // jsdom doesn't implement scrollIntoView
  Element.prototype.scrollIntoView = scrollSpy;
  scrollSpy.mockClear();
  __resetProtectionForTests();
});

afterEach(() => {
  __resetProtectionForTests();
});

describe("PeerView session protection", () => {
  it("auto-scrolls on new events when not protected", () => {
    const initial: Event[] = [chatTurn("e1", "hello", "2025-01-01T00:00:00Z")];
    const { rerender } = render(
      <PeerView peer={PEER} events={initial} apiBase="" onClose={() => {}} onSent={() => {}} />
    );
    expect(scrollSpy).toHaveBeenCalled();
    scrollSpy.mockClear();

    const next = [...initial, chatTurn("e2", "world", "2025-01-01T00:00:01Z")];
    rerender(<PeerView peer={PEER} events={next} apiBase="" onClose={() => {}} onSent={() => {}} />);
    expect(scrollSpy).toHaveBeenCalled();
    expect(screen.getByText("world")).toBeInTheDocument();
  });

  it("freezes thread + suppresses scroll while compose is dirty, restores on clear", () => {
    const initial: Event[] = [chatTurn("e1", "first turn", "2025-01-01T00:00:00Z")];
    const { rerender } = render(
      <PeerView peer={PEER} events={initial} apiBase="" onClose={() => {}} onSent={() => {}} />
    );

    const textarea = screen.getByTestId("compose-textarea") as HTMLTextAreaElement;
    expect(textarea.dataset.dirty).toBe("false");
    expect(isProtected(PEER.peer_id)).toBe(false);

    // Dirty compose -> protection engages
    fireEvent.change(textarea, { target: { value: "drafting a reply" } });
    expect(textarea.dataset.dirty).toBe("true");
    expect(isProtected(PEER.peer_id)).toBe(true);
    expect(screen.getByTestId("compose-draft-pill")).toBeInTheDocument();

    scrollSpy.mockClear();

    // Synthetic SSE event arrives while protected -> must not appear, must not scroll
    const incoming = [...initial, chatTurn("e2", "incoming turn", "2025-01-01T00:00:05Z")];
    rerender(<PeerView peer={PEER} events={incoming} apiBase="" onClose={() => {}} onSent={() => {}} />);

    expect(scrollSpy).not.toHaveBeenCalled();
    expect(screen.queryByText("incoming turn")).not.toBeInTheDocument();
    expect((screen.getByTestId("compose-textarea") as HTMLTextAreaElement).value).toBe(
      "drafting a reply"
    );

    // Clear the draft -> protection releases, thread + scroll catch up
    fireEvent.change(screen.getByTestId("compose-textarea"), { target: { value: "" } });
    expect(isProtected(PEER.peer_id)).toBe(false);

    // Force a re-render so the now-unfrozen liveThread is rendered
    rerender(<PeerView peer={PEER} events={incoming} apiBase="" onClose={() => {}} onSent={() => {}} />);
    expect(screen.getByText("incoming turn")).toBeInTheDocument();
    expect(scrollSpy).toHaveBeenCalled();
  });

  it("clears protection when peer is switched", () => {
    const { rerender } = render(
      <PeerView peer={PEER} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />
    );
    fireEvent.change(screen.getByTestId("compose-textarea"), { target: { value: "drafting" } });
    expect(isProtected(PEER.peer_id)).toBe(true);

    const other: Peer = { ...PEER, peer_id: "peer-2", name: "bob", display_name: "bob" };
    rerender(<PeerView peer={other} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />);
    // Flush effect cleanup that releases the prior peer's protection.
    act(() => {});

    expect(isProtected(PEER.peer_id)).toBe(false);
  });
});
