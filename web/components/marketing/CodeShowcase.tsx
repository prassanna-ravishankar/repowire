"use client";

import { motion } from "framer-motion";

const terminalLines = [
  { text: "$ claude", className: "t-c" },
  { text: "You: I just built the pricing card. Ask Codex to review the component.", className: "t-row" },
  {
    text: 'Claude: mcp__repowire.ask({ peer_name: "ui-codex", query: "Review web/components/PricingCard.tsx. Focus on accessibility, responsive layout, and state handling." })',
    className: "t-row",
  },
  { text: "Repowire: ask-c91f2b sent to @ui-codex", className: "t-g" },
  { text: "[ask #ask-c91f2b from @site-claude] Review web/components/PricingCard.tsx...", className: "t-m" },
  { text: "Codex: reading component, styles, and tests", className: "t-row" },
  {
    text: '[ack #ask-c91f2b from @ui-codex] Two fixes: button label wraps at 360px, and loading state needs aria-busy. The props contract looks clean.',
    className: "t-g",
  },
  { text: "Claude: applying the responsive label and aria-busy fixes now.", className: "t-row" },
];

export default function CodeShowcase() {
  return (
    <section className="showcase">
      <div className="section-head">
        <span className="eyebrow">The terminal</span>
        <h2>The terminal is the source of truth.</h2>
        <p className="section-sub">
          Claude can ask a Codex peer for a real review, Codex closes the thread with an ack,
          and the whole exchange stays visible to the mesh.
        </p>
      </div>
      <div className="showcase-grid">
        <div className="showcase-panel terminal-panel">
          <div className="term-head">
            <div className="term-dots">
              <span />
              <span />
              <span />
            </div>
            <div className="term-title">claude ↔ repowire ↔ codex</div>
          </div>
          <motion.div
            className="term-body"
            role="img"
            aria-label="Animated terminal showing Claude asking Codex to review a React component and receiving an ack"
            initial="hidden"
            animate="shown"
            variants={{
              hidden: {},
              shown: { transition: { staggerChildren: 0.12 } },
            }}
          >
            {terminalLines.map((line) => (
              <motion.div
                key={line.text}
                className={line.className}
                variants={{
                  hidden: { opacity: 0, y: 6 },
                  shown: { opacity: 1, y: 0 },
                }}
                transition={{ duration: 0.28, ease: "easeOut" }}
              >
                {line.text}
              </motion.div>
            ))}
          </motion.div>
        </div>
      </div>
    </section>
  );
}
