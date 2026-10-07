import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import SessionReplay from "./SessionReplay";

// jsdom has neither IntersectionObserver nor a matching matchMedia, so the
// component starts its replay immediately on mount (the IO fallback path).
//
// The replay chains setTimeouts: each timer schedules the next from an effect
// after re-render, so a single advanceTimersByTime only moves one step. These
// helpers step timer-by-timer inside act().

const PROMPT_START = "I just built the pricing card.";

function step(times: number) {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersToNextTimer();
    });
  }
}

/** Run the whole replay to completion (~420 chained timers, capped well above). */
function playAll() {
  step(800);
}

describe("SessionReplay", () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("skip button jumps straight to the full transcript", () => {
    vi.useFakeTimers();
    const { container } = render(<SessionReplay />);
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Skip to end" }));
    });
    expect(container.textContent).toContain("ask #c91f2b sent → @ui-codex");
    expect(container.textContent).toContain("[ack #c91f2b from @ui-codex]");
    expect(container.textContent).toContain("tests green in 1.4s");
  });

  it("types the prompt into the composer, then replays the transcript", () => {
    vi.useFakeTimers();
    const { container } = render(<SessionReplay />);

    // Mid-typing: composer holds a strict, growing prefix of the prompt.
    step(10);
    const midway = container.querySelector(".replay-composer-text")?.textContent ?? "";
    expect(midway.length).toBeGreaterThanOrEqual(5);
    expect(PROMPT_START.startsWith(midway.slice(0, PROMPT_START.length))).toBe(true);

    playAll();
    expect(container.textContent).toContain("Applying both fixes now.");
    expect(container.textContent).toContain("aria-busy");
    // Composer returns to placeholder once done.
    expect(container.textContent).toContain("Ask another agent for a review…");
  });

  it("routes steps to the right terminal panes", () => {
    vi.useFakeTimers();
    const { container } = render(<SessionReplay />);
    playAll();
    const panes = container.querySelectorAll(".replay-term");
    expect(panes).toHaveLength(2);
    const [claude, codex] = Array.from(panes);
    expect(claude.textContent).toContain("[ack #c91f2b from @ui-codex]");
    expect(claude.textContent).not.toContain("[ask #c91f2b from @site-claude]");
    expect(codex.textContent).toContain("[ask #c91f2b from @site-claude]");
    expect(codex.textContent).not.toContain("Applying both fixes now.");
  });

  it("replay button restarts the animation from the empty state", () => {
    vi.useFakeTimers();
    const { container } = render(<SessionReplay />);
    playAll();
    expect(container.textContent).toContain("tests green in 1.4s");

    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "Replay demo" }));
    });
    // Transcript cleared, codex pane back to idle.
    expect(container.textContent).not.toContain("tests green in 1.4s");
    expect(container.textContent).toContain("online, waiting on the wire");

    playAll();
    expect(container.textContent).toContain("tests green in 1.4s");
  });

  it("still auto-starts under StrictMode's replayed effects", () => {
    vi.useFakeTimers();
    const { container } = render(
      <StrictMode>
        <SessionReplay />
      </StrictMode>,
    );
    // The no-IO fallback start must survive the mount/cleanup/remount cycle.
    step(15);
    const composer = container.querySelector(".replay-composer-text")?.textContent ?? "";
    expect(composer).not.toContain("Ask another agent");
    expect(composer.length).toBeGreaterThan(0);
  });

  it("stays on the finished transcript when reduced motion is preferred", () => {
    vi.useFakeTimers();
    vi.stubGlobal(
      "matchMedia",
      vi.fn().mockReturnValue({ matches: true }),
    );
    try {
      const { container } = render(<SessionReplay />);
      // No replay ever starts: full transcript visible, composer idle.
      expect(container.textContent).toContain("tests green in 1.4s");
      step(20);
      expect(container.textContent).toContain("tests green in 1.4s");
      expect(container.textContent).toContain("Ask another agent for a review…");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("starts the replay when the IntersectionObserver reports visibility", () => {
    vi.useFakeTimers();
    let intersect: ((entries: { isIntersecting: boolean }[]) => void) | undefined;
    class FakeIO {
      constructor(cb: (entries: { isIntersecting: boolean }[]) => void) {
        intersect = cb;
      }
      observe() {}
      disconnect() {}
    }
    vi.stubGlobal("IntersectionObserver", FakeIO);
    try {
      const { container } = render(<SessionReplay />);
      // Not yet visible: SSR done state stays put.
      step(5);
      expect(container.textContent).toContain("tests green in 1.4s");

      act(() => {
        intersect?.([{ isIntersecting: true }]);
      });
      // Replay reset: transcript cleared, codex pane idle, prompt typing.
      expect(container.textContent).not.toContain("tests green in 1.4s");
      expect(container.textContent).toContain("online, waiting on the wire");
      playAll();
      expect(container.textContent).toContain("tests green in 1.4s");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("fires the wire pulse toward codex when the ask is sent", () => {
    vi.useFakeTimers();
    const { container } = render(<SessionReplay />);
    let dot: Element | null = null;
    for (let i = 0; i < 400 && !dot; i++) {
      step(1);
      dot = container.querySelector(".replay-wire-dot.go-codex");
    }
    expect(dot).not.toBeNull();
  });
});
