"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { RotateCcw, SkipForward } from "lucide-react";

type Pane = "claude" | "codex";
type Kind = "prompt" | "reply" | "tool" | "sent" | "recv-ask" | "recv-ack";
type WireDir = "to-codex" | "to-claude";

type Step = {
  pane: Pane;
  kind: Kind;
  text: string;
  /** Fire a pulse along the wire when this step starts typing. */
  wire?: WireDir;
  /** Pause (ms) before this step starts typing. */
  pause?: number;
};

const STEPS: Step[] = [
  { pane: "claude", kind: "prompt", text: "I just built the pricing card. Ask the codex peer to review it before I ship." },
  { pane: "claude", kind: "reply", text: "I'll ask @ui-codex over the mesh." },
  { pane: "claude", kind: "tool", text: 'ask(peer: "ui-codex", query: "Review web/components/PricingCard.tsx: a11y, responsive layout, state handling")' },
  { pane: "claude", kind: "sent", text: "ask #c91f2b sent → @ui-codex", wire: "to-codex" },
  { pane: "codex", kind: "recv-ask", text: "[ask #c91f2b from @site-claude] Review web/components/PricingCard.tsx: a11y, responsive layout, state handling", pause: 750 },
  { pane: "codex", kind: "reply", text: "Reading the component, styles, and tests." },
  { pane: "codex", kind: "tool", text: "Read PricingCard.tsx · PricingCard.test.tsx · tokens.css" },
  { pane: "codex", kind: "reply", text: "Two issues: the CTA label wraps at 360px, and the loading state never sets aria-busy." },
  { pane: "codex", kind: "tool", text: 'ack(#c91f2b, "Two fixes: CTA label wraps at 360px; add aria-busy on loading. Props contract is clean.")', wire: "to-claude" },
  { pane: "claude", kind: "recv-ack", text: "[ack #c91f2b from @ui-codex] Two fixes: CTA label wraps at 360px; add aria-busy on loading. Props contract is clean.", pause: 750 },
  { pane: "claude", kind: "reply", text: "Applying both fixes now." },
  { pane: "claude", kind: "tool", text: "Edit PricingCard.tsx (+9 −3) · tests green in 1.4s" },
];

const PROMPT = STEPS[0].text;
const INPUT_TICK_MS = 26;
const LOG_TICK_MS = 24;
const LOG_CHARS_PER_TICK = 2;
const STEP_GAP_MS = 420;
const SEND_GAP_MS = 400;

/**
 * Scripted replay of a real mesh exchange: Claude asks a Codex peer for a
 * review, the ask crosses the wire, Codex acks back. Server-renders the
 * finished transcript (the demo can never be blank without JS), then resets
 * and types itself out once it scrolls into view, unless the user prefers
 * reduced motion.
 */
export default function SessionReplay() {
  // "done" is both the SSR state and the end state; "input" types the prompt
  // into the composer; "log" replays the transcript step by step.
  const [phase, setPhase] = useState<"done" | "input" | "log">("done");
  const [stepIdx, setStepIdx] = useState(STEPS.length - 1);
  const [chars, setChars] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const startedRef = useRef(false);

  const begin = useCallback(() => {
    setStepIdx(0);
    setChars(0);
    setPhase("input");
  }, []);

  const skip = useCallback(() => {
    setStepIdx(STEPS.length - 1);
    setChars(STEPS[STEPS.length - 1].text.length);
    setPhase("done");
  }, []);

  // Auto-start once on scroll-into-view.
  useEffect(() => {
    if (startedRef.current) return;
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) return;
    const el = rootRef.current;
    if (!el) return;
    if (typeof IntersectionObserver === "undefined") {
      // Set the ref inside the callback: StrictMode's replayed effect clears
      // the first timer, and a pre-set ref would make the rerun bail forever.
      const t = setTimeout(() => {
        startedRef.current = true;
        begin();
      }, 0);
      return () => clearTimeout(t);
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting) && !startedRef.current) {
          startedRef.current = true;
          io.disconnect();
          begin();
        }
      },
      { rootMargin: "-120px" },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [begin]);

  // Phase "input": type the prompt into the composer, then submit it.
  useEffect(() => {
    if (phase !== "input") return;
    if (chars >= PROMPT.length) {
      const t = setTimeout(() => {
        setStepIdx(1);
        setChars(0);
        setPhase("log");
      }, SEND_GAP_MS);
      return () => clearTimeout(t);
    }
    const t = setTimeout(() => setChars((c) => c + 1), INPUT_TICK_MS);
    return () => clearTimeout(t);
  }, [phase, chars]);

  // Phase "log": type the current step, pause, advance to the next.
  useEffect(() => {
    if (phase !== "log") return;
    const step = STEPS[stepIdx];
    if (chars >= step.text.length) {
      const last = stepIdx === STEPS.length - 1;
      const t = setTimeout(() => {
        if (last) {
          setPhase("done");
        } else {
          setStepIdx(stepIdx + 1);
          setChars(0);
        }
      }, last ? 600 : STEPS[stepIdx + 1].pause ?? STEP_GAP_MS);
      return () => clearTimeout(t);
    }
    const t = setTimeout(
      () => setChars((c) => Math.min(c + LOG_CHARS_PER_TICK, step.text.length)),
      LOG_TICK_MS,
    );
    return () => clearTimeout(t);
  }, [phase, stepIdx, chars]);

  // The wire pulse is derived, not stored: it renders for as long as the
  // wire-carrying step is current (typing + the pause before the next step),
  // which outlasts its 0.9s one-shot animation.
  const activeWire = phase === "log" ? STEPS[stepIdx].wire : undefined;
  const pulse: { dir: WireDir; key: number } | null = activeWire
    ? { dir: activeWire, key: stepIdx }
    : null;

  const visible =
    phase === "done" ? STEPS : phase === "log" ? STEPS.slice(0, stepIdx + 1) : [];
  const lineFor = (step: Step, i: number) => ({
    step,
    text: phase === "log" && i === stepIdx ? step.text.slice(0, chars) : step.text,
    cursor: phase === "log" && i === stepIdx && chars < step.text.length,
  });
  const claudeLines = visible
    .map((s, i) => ({ s, i }))
    .filter(({ s }) => s.pane === "claude")
    .map(({ s, i }) => lineFor(s, i));
  const codexLines = visible
    .map((s, i) => ({ s, i }))
    .filter(({ s }) => s.pane === "codex")
    .map(({ s, i }) => lineFor(s, i));

  return (
    <section className="section showcase" id="wire" ref={rootRef}>
      <header className="section-head">
        <h2>Ask, then ack.</h2>
        <p className="section-sub">
          Claude asks a Codex peer for a review. The question crosses the mesh, Codex reads the code
          in its own checkout, and the answer comes back as an ack that closes the thread. Nothing
          is copy-pasted between terminals.
        </p>
      </header>
      <div
        className="replay-grid"
        role="group"
        aria-label="Animated replay of two terminals: Claude asks a Codex peer to review a React component over the Repowire mesh, and Codex replies with an ack listing two fixes"
      >
        <Terminal
          title="claude: @site-claude"
          lines={claudeLines}
          controls={
            <div className="replay-controls">
              <button type="button" aria-label="Skip to end" onClick={skip}>
                <SkipForward width={13} height={13} strokeWidth={1.75} />
              </button>
              <button type="button" aria-label="Replay demo" onClick={begin}>
                <RotateCcw width={13} height={13} strokeWidth={1.75} />
              </button>
            </div>
          }
          footer={
            <div className="replay-composer">
              <span className="rp-glyph rp-glyph-prompt">❯</span>
              <span className="replay-composer-text">
                {phase === "input" ? (
                  <>
                    {PROMPT.slice(0, chars)}
                    <span className="rp-cursor" />
                  </>
                ) : (
                  <span className="replay-placeholder">Ask another agent for a review…</span>
                )}
              </span>
            </div>
          }
        />
        <div className="replay-wire" aria-hidden>
          {pulse && (
            <span
              key={pulse.key}
              className={`replay-wire-dot ${pulse.dir === "to-codex" ? "go-codex" : "go-claude"}`}
            />
          )}
          <span className="replay-wire-label">repowire</span>
        </div>
        <Terminal
          title="codex: @ui-codex"
          lines={codexLines}
          idle={codexLines.length === 0}
          footer={<div className="replay-status">connected to the repowire daemon on :8377</div>}
        />
      </div>
    </section>
  );
}

const GLYPH: Record<Kind, { glyph: string; cls: string }> = {
  prompt: { glyph: "❯", cls: "rp-prompt" },
  reply: { glyph: "⏺", cls: "rp-reply" },
  tool: { glyph: "⎿", cls: "rp-tool" },
  sent: { glyph: "⎿", cls: "rp-sent" },
  "recv-ask": { glyph: "", cls: "rp-recv rp-recv-ask" },
  "recv-ack": { glyph: "", cls: "rp-recv rp-recv-ack" },
};

function Terminal({
  title,
  lines,
  controls,
  footer,
  idle = false,
}: {
  title: string;
  lines: { step: Step; text: string; cursor: boolean }[];
  controls?: React.ReactNode;
  footer?: React.ReactNode;
  idle?: boolean;
}) {
  return (
    <div className="showcase-panel terminal-panel replay-term">
      <div className="term-head">
        <div className="term-dots">
          <span />
          <span />
          <span />
        </div>
        <div className="term-title">{title}</div>
        {controls ?? <div className="replay-controls-spacer" />}
      </div>
      <div className="replay-body">
        <div className="replay-lines">
          {idle && <div className="replay-idle">online, waiting on the wire</div>}
          {lines.map(({ step, text, cursor }, i) => {
            const { glyph, cls } = GLYPH[step.kind];
            return (
              <div key={i} className={`replay-line ${cls}`}>
                {glyph && (
                  <span className="rp-glyph" aria-hidden>
                    {glyph}
                  </span>
                )}
                <span className="rp-text">
                  {text}
                  {cursor && <span className="rp-cursor" />}
                </span>
              </div>
            );
          })}
        </div>
        {footer}
      </div>
    </div>
  );
}
