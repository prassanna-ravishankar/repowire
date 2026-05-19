import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { PeerView } from "./PeerView";
import { __resetProtectionForTests, isProtected } from "../lib/protection";
import { __resetDraftsForTests, getDraftText, setDraftText } from "../lib/drafts";
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

const OTHER: Peer = {
  ...PEER,
  peer_id: "peer-2",
  name: "bob",
  display_name: "bob",
  path: "/tmp/bob",
};

function chatTurn(id: string, text: string, ts: string, peerId = PEER.peer_id): Event {
  return {
    id,
    type: "chat_turn",
    timestamp: ts,
    peer_id: peerId,
    role: "assistant",
    text,
  };
}

const scrollSpy = vi.fn();

beforeEach(() => {
  Element.prototype.scrollIntoView = scrollSpy;
  scrollSpy.mockClear();
  __resetProtectionForTests();
  __resetDraftsForTests();
});

afterEach(() => {
  __resetProtectionForTests();
  __resetDraftsForTests();
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

    fireEvent.change(textarea, { target: { value: "drafting a reply" } });
    expect((screen.getByTestId("compose-textarea") as HTMLTextAreaElement).dataset.dirty).toBe(
      "true"
    );
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

    rerender(<PeerView peer={PEER} events={incoming} apiBase="" onClose={() => {}} onSent={() => {}} />);
    expect(screen.getByText("incoming turn")).toBeInTheDocument();
    expect(scrollSpy).toHaveBeenCalled();
  });

  it("preserves draft and protection across peer switch", () => {
    const { rerender } = render(
      <PeerView peer={PEER} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />
    );
    fireEvent.change(screen.getByTestId("compose-textarea"), {
      target: { value: "half-written draft" },
    });
    expect(isProtected(PEER.peer_id)).toBe(true);
    expect(getDraftText(PEER.peer_id)).toBe("half-written draft");

    // Switch to a different peer.
    rerender(<PeerView peer={OTHER} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />);
    act(() => {});

    // Other peer is not protected and has no draft.
    expect(isProtected(OTHER.peer_id)).toBe(false);
    expect((screen.getByTestId("compose-textarea") as HTMLTextAreaElement).value).toBe("");

    // A's draft and protection survive the switch.
    expect(getDraftText(PEER.peer_id)).toBe("half-written draft");
    expect(isProtected(PEER.peer_id)).toBe(true);

    // Switch back to A: draft is restored, protection still engaged.
    rerender(<PeerView peer={PEER} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />);
    act(() => {});
    expect((screen.getByTestId("compose-textarea") as HTMLTextAreaElement).value).toBe(
      "half-written draft"
    );
    expect(isProtected(PEER.peer_id)).toBe(true);
  });

  it("freezes A's thread even when A events arrive while A is offscreen", () => {
    // The leak this guards against: A is protected, user switches to B, an
    // SSE event for A arrives (added to parent events), user switches back to
    // A. If the frozen snapshot weren't kept per-peer in the store it would
    // be lost during the B render and the new A event would render despite
    // A still being protected.
    const initialA = [chatTurn("a1", "alice original", "2025-01-01T00:00:00Z", PEER.peer_id)];
    const { rerender } = render(
      <PeerView peer={PEER} events={initialA} apiBase="" onClose={() => {}} onSent={() => {}} />
    );

    // Dirty A.
    fireEvent.change(screen.getByTestId("compose-textarea"), { target: { value: "drafting" } });
    expect(isProtected(PEER.peer_id)).toBe(true);
    expect(screen.getByText("alice original")).toBeInTheDocument();

    // Switch to B.
    rerender(<PeerView peer={OTHER} events={initialA} apiBase="" onClose={() => {}} onSent={() => {}} />);
    act(() => {});
    expect(isProtected(PEER.peer_id)).toBe(true); // A's protection survives

    // SSE event for A arrives while user is on B.
    const withNewA = [
      ...initialA,
      chatTurn("a2", "alice arrived while offscreen", "2025-01-01T00:00:30Z", PEER.peer_id),
    ];
    rerender(<PeerView peer={OTHER} events={withNewA} apiBase="" onClose={() => {}} onSent={() => {}} />);

    // Switch back to A while still dirty.
    rerender(<PeerView peer={PEER} events={withNewA} apiBase="" onClose={() => {}} onSent={() => {}} />);
    act(() => {});

    expect(isProtected(PEER.peer_id)).toBe(true);
    expect(screen.getByText("alice original")).toBeInTheDocument();
    expect(screen.queryByText("alice arrived while offscreen")).not.toBeInTheDocument();

    // Clear A's draft -> the new event finally appears.
    fireEvent.change(screen.getByTestId("compose-textarea"), { target: { value: "" } });
    expect(isProtected(PEER.peer_id)).toBe(false);
    rerender(<PeerView peer={PEER} events={withNewA} apiBase="" onClose={() => {}} onSent={() => {}} />);
    expect(screen.getByText("alice arrived while offscreen")).toBeInTheDocument();
  });

  it("flips protection synchronously with the keystroke (no passive-effect race)", () => {
    // The failure mode this guards against: an SSE update arrives in the
    // window between the user keystroke and the dirty-flag effect running.
    // If protection flips inside a passive useEffect, the new event renders
    // before the freeze engages. With the store's synchronous mark, the
    // very next render — even one triggered by an SSE update — must already
    // see protected=true and freeze the thread.
    const initial: Event[] = [chatTurn("e1", "before", "2025-01-01T00:00:00Z")];
    const { rerender } = render(
      <PeerView peer={PEER} events={initial} apiBase="" onClose={() => {}} onSent={() => {}} />
    );

    // Synchronously dirty the draft (this is what onChange does internally).
    // Importantly we do NOT flush effects between setDraftText and the next
    // render — the rerender below simulates a parent re-render carrying a new
    // events list, racing the still-unflushed dirty effect from the old model.
    act(() => {
      setDraftText(PEER.peer_id, "x");
    });

    const racingEvents = [...initial, chatTurn("e2", "racing event", "2025-01-01T00:00:01Z")];
    rerender(
      <PeerView peer={PEER} events={racingEvents} apiBase="" onClose={() => {}} onSent={() => {}} />
    );

    expect(isProtected(PEER.peer_id)).toBe(true);
    expect(screen.queryByText("racing event")).not.toBeInTheDocument();
  });
});
