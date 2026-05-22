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

  it("searches the normalized timeline and jumps to a result", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/timeline/search")) {
        return new Response(
          JSON.stringify({
            degraded: false,
            degradation_message: "",
            results: [
              {
                cursor: "session-b:turn-b",
                target_id: "turn-session-b-turn-b",
                item: {
                  id: "history:session-b:turn-b",
                  kind: "turn",
                  source: "history",
                  timestamp: "2025-01-01T00:00:02Z",
                  session_id: "session-b",
                  turn_id: "turn-b",
                  role: "assistant",
                  text: "search target answer",
                  tool_calls: [],
                },
                match: {
                  start: 0,
                  end: 6,
                  snippet: "search target answer",
                },
              },
            ],
          }),
          { status: 200 },
        );
      }
      if (url.includes("session_id=session-b")) {
        return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
      }
      if (url.includes("/spawn/config")) return new Promise<Response>(() => {});
      return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<PeerView peer={PEER} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />);

    fireEvent.change(screen.getByLabelText("Search conversation"), {
      target: { value: "search" },
    });
    fireEvent.click(screen.getByRole("button", { name: "search" }));

    const result = await screen.findByRole("button", { name: /search target answer/i });
    fireEvent.click(result);

    expect(await screen.findByTestId("timeline-session-b:turn-b")).toBeInTheDocument();
    await waitFor(() => {
      expect(fetchMock.mock.calls.some((call) => String(call[0]).includes("session_id=session-b"))).toBe(true);
    });
    expect(scrollSpy).toHaveBeenCalledWith({ block: "center" });
  });

  it("shows search degradation when only event-ring data is searchable", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/timeline/search")) {
        return new Response(
          JSON.stringify({
            degraded: true,
            degradation_message: "Search is limited to realtime timeline item(s) still present in the dashboard event ring.",
            results: [],
          }),
          { status: 200 },
        );
      }
      if (url.includes("/spawn/config")) return new Promise<Response>(() => {});
      return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
    });
    vi.stubGlobal("fetch", fetchMock);

    render(<PeerView peer={PEER} events={[]} apiBase="" onClose={() => {}} onSent={() => {}} />);

    fireEvent.change(screen.getByLabelText("Search conversation"), {
      target: { value: "legacy" },
    });
    fireEvent.click(screen.getByRole("button", { name: "search" }));

    expect(await screen.findByText(/event ring/i)).toBeInTheDocument();
  });

  it("uses apiBase for thread attachment download links", () => {
    const event: Event = {
      id: "att-event",
      type: "notification",
      timestamp: "2025-01-01T00:00:00Z",
      from: "alice",
      to: "dashboard",
      from_peer_id: PEER.peer_id,
      text: "attached",
      attachments: [{
        id: "att-123",
        filename: "diagram.png",
      }],
    };

    render(
      <PeerView
        peer={PEER}
        events={[event]}
        apiBase="http://daemon.test"
        onClose={() => {}}
        onSent={() => {}}
      />
    );

    expect(screen.getByRole("link", { name: /diagram\.png/i })).toHaveAttribute(
      "href",
      "http://daemon.test/attachments/att-123",
    );
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

describe("PeerView MCP config scope", () => {
  it("labels Codex MCP edits as global backend config before adding", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/peers/alice/mcp")) {
        return new Response(
          JSON.stringify({
            servers: [],
            config_scope: {
              backend: "codex",
              owner: "backend",
              effective_scope: "backend_global",
              label: "Codex global backend config",
              description: "Codex MCP edits target the user-level Codex config shared by Codex sessions on this host.",
              supported_scopes: ["user"],
              default_scope: "user",
              is_global: true,
              peer_id: PEER.peer_id,
              peer_name: "alice",
              project_path: "/tmp/alice",
              peer_machine: "host",
              self_machine: "host",
              same_host: true,
            },
          }),
          { status: 200 },
        );
      }
      return new Promise<Response>(() => {});
    });
    vi.stubGlobal("fetch", fetchMock);

    render(
      <PeerView
        peer={{ ...PEER, backend: "codex" }}
        events={[]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    fireEvent.click(screen.getByRole("tab", { name: "mcp" }));

    expect(await screen.findByText("Codex global backend config")).toBeInTheDocument();
    fireEvent.click(await screen.findByText("+ add server"));

    expect(screen.getByText("editing Codex global backend config")).toBeInTheDocument();
    expect(screen.getByText("scope: user")).toBeInTheDocument();
    expect(screen.queryByText("project scope")).not.toBeInTheDocument();
  });

  it("shows cross-host scope metadata without crashing", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/peers/alice/mcp")) {
        return new Response(
          JSON.stringify({
            detail: {
              error: "cross_host",
              peer_machine: "remote-host",
              self_machine: "local-host",
              config_scope: {
                backend: "gemini",
                owner: "backend",
                effective_scope: "backend_global",
                label: "Gemini global backend config",
                description: "Gemini MCP edits target the user-level Gemini settings shared by Gemini sessions on this host.",
                supported_scopes: ["user"],
                default_scope: "user",
                is_global: true,
                peer_id: PEER.peer_id,
                peer_name: "alice",
                project_path: "/tmp/alice",
                peer_machine: "remote-host",
                self_machine: "local-host",
                same_host: false,
              },
            },
          }),
          { status: 409 },
        );
      }
      return new Promise<Response>(() => {});
    });
    vi.stubGlobal("fetch", fetchMock);

    render(
      <PeerView
        peer={{ ...PEER, backend: "gemini", machine: "remote-host" }}
        events={[]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    fireEvent.click(screen.getByRole("tab", { name: "mcp" }));

    expect(await screen.findByText("Gemini global backend config")).toBeInTheDocument();
    expect(screen.getByText("remote host")).toBeInTheDocument();
  });
});

describe("PeerView session controls", () => {
  it("notifies the active session through the session control endpoint", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/spawn/config")) return new Promise<Response>(() => {});
      if (url.includes("/transcript")) {
        return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
      }
      if (url.includes("/controls/resume")) {
        return new Response(
          JSON.stringify({
            ok: true,
            repowire_session_id: "session-a",
            session_status: "active",
            status: "active_executor",
            capability: "active_executor",
            message: "Session already has an active executor; send controls to that peer.",
            backend: "codex",
            executor_peer_id: PEER.peer_id,
            executor_peer_name: "alice",
            resume_capability: {},
          }),
          { status: 200 },
        );
      }
      if (url.includes("/controls/notify")) {
        return new Response(
          JSON.stringify({ ok: true, delivery_state: "delivered", reason: "sent" }),
          { status: 200 },
        );
      }
      return new Promise<Response>(() => {});
    });
    vi.stubGlobal("fetch", fetchMock);

    render(
      <PeerView
        peer={{ ...PEER, backend: "codex", metadata: { hook_session_id: "session-a" } }}
        events={[]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    expect(await screen.findByText("active executor")).toBeInTheDocument();
    const input = screen.getByPlaceholderText("notify active alice session...");
    fireEvent.change(input, { target: { value: "status please" } });
    fireEvent.click(screen.getByLabelText("Notify active session"));

    await waitFor(() => {
      expect(fetchMock.mock.calls.some((call) => String(call[0]).includes("/sessions/session-a/controls/notify"))).toBe(true);
    });
    const notifyCall = fetchMock.mock.calls.find((call) => String(call[0]).includes("/controls/notify"));
    expect(notifyCall?.[1]).toMatchObject({ method: "POST" });
    expect(JSON.parse(String((notifyCall?.[1] as RequestInit).body))).toMatchObject({
      from_peer: "dashboard",
      text: "status please",
      bypass_circle: true,
    });
    expect(await screen.findByText("delivered to active session")).toBeInTheDocument();
  });

  it("keeps the plain ask composer on /ask alongside session controls", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/spawn/config")) return new Promise<Response>(() => {});
      if (url.includes("/transcript")) {
        return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
      }
      if (url.includes("/controls/resume")) {
        return new Response(
          JSON.stringify({
            ok: true,
            repowire_session_id: "session-a",
            session_status: "active",
            status: "active_executor",
            capability: "active_executor",
            message: "Session already has an active executor.",
            backend: "codex",
            resume_capability: {},
          }),
          { status: 200 },
        );
      }
      if (url === "/ask") {
        return new Response(JSON.stringify({ correlation_id: "ask-12345678" }), { status: 200 });
      }
      return new Promise<Response>(() => {});
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

    const askComposer = screen.getByTestId("compose-textarea");
    fireEvent.change(askComposer, { target: { value: "normal ask" } });
    fireEvent.click(screen.getByLabelText("Ask peer"));

    await waitFor(() => {
      expect(fetchMock.mock.calls.some((call) => String(call[0]) === "/ask")).toBe(true);
    });
    const askCall = fetchMock.mock.calls.find((call) => String(call[0]) === "/ask");
    expect(JSON.parse(String((askCall?.[1] as RequestInit).body))).toMatchObject({
      from_peer: "dashboard",
      to_peer: "alice",
      bypass_circle: true,
    });
  });

  it("shows unsupported and no-session controls as disabled states", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/spawn/config")) return new Promise<Response>(() => {});
      if (url.includes("/transcript")) {
        return new Response(JSON.stringify({ turns: [], next_before: null }), { status: 200 });
      }
      if (url.includes("/controls/resume")) {
        return new Response(
          JSON.stringify({
            ok: true,
            repowire_session_id: "session-a",
            session_status: "detached",
            status: "unsupported",
            capability: "unsupported",
            message: "No compatible backend resume capability is recorded for this session.",
            backend: "opencode",
            resume_capability: {},
          }),
          { status: 200 },
        );
      }
      return new Promise<Response>(() => {});
    });
    vi.stubGlobal("fetch", fetchMock);

    const { rerender } = render(
      <PeerView
        peer={{ ...PEER, backend: "opencode", metadata: { hook_session_id: "session-a" } }}
        events={[]}
        apiBase=""
        onClose={() => {}}
        onSent={() => {}}
      />,
    );

    expect(await screen.findByText("resume unsupported")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("session notify unavailable")).toBeDisabled();
    expect(screen.getByLabelText("Notify active session")).toBeDisabled();
    expect(await screen.findByText(/no messages with alice/i)).toBeInTheDocument();

    await act(async () => {
      rerender(
        <PeerView
          peer={{ ...PEER, peer_id: "peer-no-session", metadata: {} }}
          events={[]}
          apiBase=""
          onClose={() => {}}
          onSent={() => {}}
        />,
      );
      await Promise.resolve();
    });

    expect(screen.getByText("no session selected")).toBeInTheDocument();
    expect(screen.getByText("Controls appear after a session is selected from live or persisted history.")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("session notify unavailable")).toBeDisabled();
  });
});
