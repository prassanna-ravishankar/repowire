"use client";

import { useEffect, useState } from "react";

type Verb = "ask" | "ack" | "notify" | "broadcast";

const peers = [
  { id: "backend", agent: "claude-code", x: 50, y: 14, status: "on" },
  { id: "frontend", agent: "codex", x: 86, y: 50, status: "on" },
  { id: "db-migrations", agent: "pi", x: 50, y: 86, status: "stale" },
  { id: "qa", agent: "opencode", x: 14, y: 50, status: "on" },
] as const;

const events = [
  { from: 0, to: 1 },
  { from: 1, to: 0 },
  { from: 0, to: 2 },
  { from: 2, to: 3 },
] as const;

const logRows: { t: string; from: string; verb: Verb; to: string; body: string }[] = [
  { t: "14:02:11", from: "@backend", verb: "ask", to: "@frontend", body: "What's the auth response shape from /me right now?" },
  { t: "14:02:18", from: "@frontend", verb: "ack", to: "@backend", body: "{ user, session } — no nested wrapper." },
  { t: "14:03:42", from: "@backend", verb: "notify", to: "", body: "Migration 0042 applied — added users.last_active." },
  { t: "14:03:58", from: "@db-migrations", verb: "ack", to: "@qa", body: "Smoke checks green, safe to deploy." },
];

const verbClass: Record<Verb, string> = {
  ask: "v-ask",
  ack: "v-ack",
  notify: "v-notify",
  broadcast: "v-broadcast",
};

export default function MeshDemo({ variant = "section" }: { variant?: "section" | "hero" } = {}) {
  const [tick, setTick] = useState(0);

  useEffect(() => {
    const id = setInterval(() => setTick((t) => (t + 1) % events.length), 2200);
    return () => clearInterval(id);
  }, []);

  const active = events[tick];

  return (
    <div className={`mesh-demo${variant === "hero" ? " mesh-demo--hero" : ""}`}>
      <div className="mesh-canvas">
        <svg className="mesh-edges" viewBox="0 0 100 100" preserveAspectRatio="none">
          {peers.map((p, i) =>
            peers.slice(i + 1).map((q, j) => (
              <line
                key={`${i}-${j}`}
                x1={p.x}
                y1={p.y}
                x2={q.x}
                y2={q.y}
                stroke="currentColor"
                strokeWidth="0.2"
                opacity="0.15"
              />
            )),
          )}
          <line
            key="active"
            x1={peers[active.from].x}
            y1={peers[active.from].y}
            x2={peers[active.to].x}
            y2={peers[active.to].y}
            stroke="var(--accent)"
            strokeWidth="0.5"
            strokeDasharray="2 2"
            className="mesh-active-edge"
          />
        </svg>
        {peers.map((p, i) => (
          <div
            key={p.id}
            className={`mesh-peer ${i === active.from || i === active.to ? "mesh-peer-active" : ""}`}
            style={{ left: `${p.x}%`, top: `${p.y}%` }}
          >
            <div className={`mesh-dot ${p.status === "stale" ? "stale" : ""}`} />
            <div className="mesh-name">@{p.id}</div>
            <div className="mesh-agent">{p.agent}</div>
          </div>
        ))}
        <div className="mesh-hub" />
      </div>
      <div className="mesh-log">
        <div className="mesh-log-head">
          <div className="mesh-log-title">Live mesh activity</div>
          <span className="badge-online">
            <span className="dot" />
            Streaming
          </span>
        </div>
        <div className="mesh-log-rows">
          {logRows.map((row, i) => (
            <MeshLogRow key={row.t} {...row} highlight={tick === i} />
          ))}
        </div>
      </div>
    </div>
  );
}

function MeshLogRow({
  t,
  from,
  verb,
  to,
  body,
  highlight,
}: {
  t: string;
  from: string;
  verb: Verb;
  to: string;
  body: string;
  highlight: boolean;
}) {
  return (
    <div className={`mesh-log-row ${highlight ? "row-hl" : ""}`}>
      <div className="mesh-log-when">{t}</div>
      <div>
        <div className="mesh-log-head-row">
          <span className="mesh-log-peer">{from}</span>
          <span className={`verb-pill ${verbClass[verb]}`}>{verb}</span>
          {to && (
            <>
              <span className="mesh-log-arrow">→</span>
              <span className="mesh-log-peer">{to}</span>
            </>
          )}
        </div>
        <div className="mesh-log-body">{body}</div>
      </div>
    </div>
  );
}
