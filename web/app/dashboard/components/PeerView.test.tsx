import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PeerView } from "./PeerView";
import { __resetProtectionForTests, getFrozenThread, isProtected } from "../lib/protection";
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
  vi.stubGlobal(
    "fetch",
    vi.fn(() => new Promise<Response>(() => {})),
  );
});

afterEach(() => {
  __resetProtectionForTests();
  __resetDraftsForTests();
  vi.unstubAllGlobals();
});

describe("PeerView session protection", () => {
  it("renders persisted transcript turns in the primary chat timeline", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).includes("/spawn/config")) return new Promise<Response>(() => {});
      return new Response(
        JSON.stringify({
          turns: [
            {
              role: "assistant",
              text: "persisted answer",
              timestamp: "2025-01-01T00:00:00Z",
              session_id: "session-a",
              turn_id: "turn-a",
              tool_calls: [],
            },
          ],
          next_before: null,
        }),
        { status: 200 },
      );
    });
    vi.stubGlobal("fetch", fetchMock);

    render(
      <PeerView
        peer={{ ...PEER, metadata: { hook_session_id: "session-a" } }}
        events={[]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    expect(await screen.findByText("persisted answer")).toBeInTheDocument();
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining("session_id=session-a"),
        expect.any(Object),
      );
    });
  });

  it("keeps active session sticky after choosing newest realtime session", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      if (String(input).includes("/spawn/config")) return new Promise<Response>(() => {});
      return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);
    const liveS1 = {
      ...chatTurn("s1-final", "session one", "2025-01-01T00:00:02Z"),
      session_id: "s1",
      turn_id: "t1",
    };
    const lateOlder = {
      ...chatTurn("s0-final", "older session", "2025-01-01T00:00:01Z"),
      session_id: "s0",
      turn_id: "t0",
    };

    const { rerender } = render(
      <PeerView peer={PEER} events={[liveS1]} apiBase="" onClose={() => {}} onSent={() => {}} />,
    );
    expect(await screen.findByText("session one")).toBeInTheDocument();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some((call) => String(call[0]).includes("session_id=s1"))).toBe(true);
    });

    rerender(<PeerView peer={PEER} events={[lateOlder, liveS1]} apiBase="" onClose={() => {}} onSent={() => {}} />);
    expect(screen.getByText("session one")).toBeInTheDocument();
    expect(screen.queryByText("older session")).not.toBeInTheDocument();
  });

  it("does not let delayed transcript discovery overwrite realtime session fallback", async () => {
    let resolveDiscovery: (response: Response) => void = () => {};
    const discovery = new Promise<Response>((resolve) => {
      resolveDiscovery = resolve;
    });
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/spawn/config")) return new Promise<Response>(() => {});
      if (url.includes("session_id=session-new")) {
        return Promise.resolve(
          new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 }),
        );
      }
      return discovery;
    });
    vi.stubGlobal("fetch", fetchMock);
    const liveNew = {
      ...chatTurn("new-final", "new realtime session", "2025-01-01T00:00:02Z"),
      session_id: "session-new",
      turn_id: "turn-new",
    };

    render(<PeerView peer={PEER} events={[liveNew]} apiBase="" onClose={() => {}} onSent={() => {}} />);
    expect(await screen.findByText("new realtime session")).toBeInTheDocument();

    await act(async () => {
      resolveDiscovery(
        new Response(
          JSON.stringify({
            turns: [
              {
                role: "assistant",
                text: "stale transcript session",
                timestamp: "2025-01-01T00:00:01Z",
                session_id: "session-old",
                turn_id: "turn-old",
                tool_calls: [],
              },
            ],
            next_before: null,
          }),
          { status: 200 },
        ),
      );
      await discovery;
    });

    expect(screen.getByText("new realtime session")).toBeInTheDocument();
    expect(screen.queryByText("stale transcript session")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some((call) => String(call[0]).includes("session_id=session-new"))).toBe(true);
    });
  });

  it("collapses persisted assistant rows when matching realtime final arrives", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).includes("/spawn/config")) return new Promise<Response>(() => {});
        return new Response(
          JSON.stringify({
            turns: [
              {
                role: "assistant",
                text: "persisted duplicate",
                timestamp: "2025-01-01T00:00:00Z",
                session_id: "session-a",
                turn_id: "turn-a",
                tool_calls: [],
              },
            ],
            next_before: null,
          }),
          { status: 200 },
        );
      }),
    );
    const live = {
      ...chatTurn("live-a", "live final wins", "2025-01-01T00:00:01Z"),
      session_id: "session-a",
      turn_id: "turn-a",
    };

    render(
      <PeerView
        peer={{ ...PEER, metadata: { hook_session_id: "session-a" } }}
        events={[live]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    expect(await screen.findByText("live final wins")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText("persisted duplicate")).not.toBeInTheDocument());
  });

  it("renders streaming deltas only for the active session", async () => {
    const deltaActive: Event = {
      id: "delta-active",
      type: "chat_turn_delta",
      timestamp: "2025-01-01T00:00:00Z",
      peer_id: PEER.peer_id,
      session_id: "session-a",
      turn_id: "turn-a",
      chunk_index: 0,
      kind: "text",
      text: "active stream",
    };
    const deltaOther: Event = {
      ...deltaActive,
      id: "delta-other",
      session_id: "session-b",
      text: "other stream",
    };

    render(
      <PeerView
        peer={{ ...PEER, metadata: { hook_session_id: "session-a" } }}
        events={[deltaOther, deltaActive]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    expect(await screen.findByText("active stream")).toBeInTheDocument();
    expect(screen.queryByText("other stream")).not.toBeInTheDocument();
  });

  it("orders streaming delta blocks by chunk_index instead of timestamp", async () => {
    const laterChunkFirstByTimestamp: Event = {
      id: "delta-1",
      type: "chat_turn_delta",
      timestamp: "2025-01-01T00:00:00Z",
      peer_id: PEER.peer_id,
      session_id: "session-a",
      turn_id: "turn-a",
      chunk_index: 1,
      kind: "text",
      text: "second block",
    };
    const earlierChunkSecondByTimestamp: Event = {
      ...laterChunkFirstByTimestamp,
      id: "delta-0",
      timestamp: "2025-01-01T00:00:01Z",
      chunk_index: 0,
      text: "first block",
    };

    render(
      <PeerView
        peer={{ ...PEER, metadata: { hook_session_id: "session-a" } }}
        events={[laterChunkFirstByTimestamp, earlierChunkSecondByTimestamp]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    const first = await screen.findByText("first block");
    const second = screen.getByText("second block");
    expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

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

  it("captures the just-committed thread when compose is dirtied right after an SSE commit", () => {
    // Documents the intended invariant from useLayoutEffect timing: after an
    // SSE-driven commit, the snapshot provider's ref already reflects the
    // new thread, so a user keystroke that immediately follows the commit
    // captures the just-arrived event in the freeze. Note: jsdom + RTL's
    // act() flushes both layout and passive effects before returning, so
    // this test cannot independently distinguish layout from passive
    // timing — its primary value is regression coverage for the captured
    // snapshot's contents and lock-in for the documented contract.
    const initial: Event[] = [chatTurn("e1", "first turn", "2025-01-01T00:00:00Z")];
    const { rerender } = render(
      <PeerView peer={PEER} events={initial} apiBase="" onClose={() => {}} onSent={() => {}} />
    );

    const incoming = [...initial, chatTurn("e2", "just arrived", "2025-01-01T00:00:01Z")];
    // Commit the new event first; layout effects run during this commit so
    // the snapshot provider ref must now reflect `incoming`. A passive
    // useEffect would leave a window here in real browsers where the user
    // could dirty compose before the ref refresh ran.
    act(() => {
      rerender(
        <PeerView peer={PEER} events={incoming} apiBase="" onClose={() => {}} onSent={() => {}} />
      );
    });
    // Now simulate the user dirtying compose immediately after the commit.
    act(() => {
      setDraftText(PEER.peer_id, "drafting");
    });

    expect(isProtected(PEER.peer_id)).toBe(true);
    const snapshot = getFrozenThread<Event>(PEER.peer_id);
    expect(snapshot).not.toBeNull();
    expect(snapshot!.map((e) => e.id)).toContain("e2");
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
