import type { ReactNode } from "react";
import PackageFigure, { type PackageFigureName } from "./PackageFigure";

type Chapter = {
  id: string;
  num: string;
  title: string;
  body: ReactNode;
  points: { k: string; v: string }[];
  code?: string;
  figure: { name: PackageFigureName; caption: string; label: string };
};

const CHAPTERS: Chapter[] = [
  {
    id: "runtimes",
    num: "§ 3",
    title: "Every runtime plugs into one daemon.",
    body: (
      <>
        Each runtime connects the way it can: Claude Code through hooks and its inbox, Codex through
        its App Server, OpenCode through a plugin, Pi through an extension. The daemon on <code>:8377</code> is the only
        hub. Identity, routing and lifecycle live there, not in the transports.
      </>
    ),
    points: [
      { k: "Agents", v: "Claude Code · Codex · OpenCode · Pi" },
      { k: "Humans", v: "Dashboard · Telegram · Slack" },
      { k: "State", v: "SQLite, local, schema-versioned" },
    ],
    figure: {
      name: "patch",
      caption: "A patch panel: one port per peer. Point at a cable to lift it.",
      label: "A patch panel of twenty-four ports with cables, standing for peers plugged into one daemon",
    },
  },
  {
    id: "anywhere",
    num: "§ 4",
    title: "Steer it from your phone.",
    body: (
      <>
        The dashboard, Telegram and Slack join the mesh as peers, so an agent can ask you a question
        the same way it asks another agent. The hosted relay is opt-in and dials out: no inbound
        ports, nothing exposed.
      </>
    ),
    points: [
      { k: "Answer", v: "Inline buttons for open asks" },
      { k: "Share", v: "Read-only or read-write session links" },
      { k: "Relay", v: "Outbound only, off by default" },
    ],
    code: "repowire share my-agent --rw --ttl 3600",
    figure: {
      name: "phone",
      caption: "A phone in layers. Move across to open it; move down to pick a layer.",
      label: "A phone taken apart in layers, standing for steering agents from a phone",
    },
  },
  {
    id: "jobs",
    num: "§ 5",
    title: "Work that outlives the session.",
    body: (
      <>
        Durable jobs carry lifecycle and results, one-off or on a cron. Schedules wake a peer later
        with a reminder or an ask. An orchestrator session can dispatch, collect status and keep the
        reviews moving while you are away.
      </>
    ),
    points: [
      { k: "Jobs", v: "Run, retry, cancel, from CLI or MCP" },
      { k: "Schedules", v: "One-shot or cron wake-ups" },
      { k: "Orchestrator", v: "One peer that runs the queue" },
    ],
    code: 'repowire jobs create "Daily brief" --path .repowire/agents/daily-brief --backend codex --cron "@daily" --prompt "Prepare the brief."',
    figure: {
      name: "slow",
      caption: "Jobs ride the belt through the gate. Hover to slow the clock.",
      label: "Crates riding a conveyor belt through a gate, standing for durable jobs",
    },
  },
  {
    id: "why",
    num: "§ 6",
    title: "Every commit remembers why.",
    body: (
      <>
        Agent commits carry <code>Repowire-Thread</code> and <code>Repowire-Session</code> trailers.
        Months later, <code>repowire why</code> replays the conversations that produced a commit: who
        asked, who answered, and what they agreed.
      </>
    ),
    points: [
      { k: "Trailers", v: "Added by the commit hook" },
      { k: "Replay", v: "Ask threads behind any commit" },
      { k: "Output", v: "Readable, or --json" },
    ],
    code: "repowire why HEAD~3",
    figure: {
      name: "branches",
      caption: "A branch forks off main and merges back. Point at a commit to raise its history.",
      label: "A commit graph with a branch forking off main and merging back",
    },
  },
];

export default function Chapters() {
  return (
    <>
      {CHAPTERS.map((c, i) => (
        <section className={`section chapter ${i % 2 ? "flip" : ""}`} id={c.id} key={c.id}>
          <div className="chapter-copy">
            <p className="section-num">{c.num}</p>
            <h2>{c.title}</h2>
            <p className="section-sub">{c.body}</p>
            <dl className="spec">
              {c.points.map((p) => (
                <div key={p.k}>
                  <dt>{p.k}</dt>
                  <dd>{p.v}</dd>
                </div>
              ))}
            </dl>
            {c.code && (
              <pre className="inline-code">
                <code>
                  <span className="cb-prompt">$</span>
                  {c.code}
                </code>
              </pre>
            )}
          </div>
          <div className="chapter-figure">
            <PackageFigure fig={String(i + 2)} {...c.figure} />
          </div>
        </section>
      ))}
    </>
  );
}
