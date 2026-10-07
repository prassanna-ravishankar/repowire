import PackageFigure from "./PackageFigure";
import PhoneChat from "./PhoneChat";

const RUNTIMES = [
  { k: "Claude Code", v: "hooks and its inbox" },
  { k: "Codex", v: "its App Server" },
  { k: "OpenCode", v: "a plugin" },
  { k: "Pi", v: "an extension" },
  { k: "Humans", v: "dashboard, Telegram, Slack" },
];

const PHONE_FACTS = [
  { k: "Answer", v: "Inline buttons for open asks" },
  { k: "Share", v: "Read-only or read-write session links" },
  { k: "Relay", v: "Outbound only, off by default" },
];

function Runtimes() {
  return (
    <section className="section split" id="runtimes">
      <div className="split-copy">
        <h2>Every runtime plugs into one daemon.</h2>
        <p className="section-sub">
          Each runtime connects the way it can. The daemon on <code>:8377</code> is the only hub:
          identity, routing and lifecycle live there, not in the transports.
        </p>
        <dl className="spec">
          {RUNTIMES.map((r) => (
            <div key={r.k}>
              <dt>{r.k}</dt>
              <dd>{r.v}</dd>
            </div>
          ))}
        </dl>
      </div>
      <div className="split-figure">
        <PackageFigure
          name="patch"
          caption="Claude Code, Codex, OpenCode and Pi each take a port on the same daemon: different transports, one protocol behind them."
          label="A patch panel of twenty-four ports with cables, standing for peers plugged into one daemon"
        />
      </div>
    </section>
  );
}

function Phone() {
  return (
    <section className="section moment" id="anywhere">
      <header className="moment-head">
        <h2>Steer it from your phone.</h2>
        <p className="section-sub">
          The dashboard, Telegram and Slack join the mesh as peers, so an agent asks you a question
          the same way it asks another agent.
        </p>
      </header>
      <div className="moment-figure">
        <PhoneChat caption="An agent's ask, as it lands in Telegram. Tap an answer and it goes straight back to the agent that asked." />
      </div>
      <dl className="moment-facts">
        {PHONE_FACTS.map((f) => (
          <div key={f.k}>
            <dt>{f.k}</dt>
            <dd>{f.v}</dd>
          </div>
        ))}
      </dl>
      <pre className="inline-code moment-code">
        <code>
          <span className="cb-prompt">$</span>
          repowire share my-agent --rw --ttl 3600
        </code>
      </pre>
    </section>
  );
}

function Pair() {
  return (
    <section className="section pair" id="jobs">
      <article className="pair-cell pair-wide" id="why">
        <PackageFigure
          name="branches"
          caption="A commit carries the ask threads that produced it, and repowire why walks back through them."
          label="A commit graph with a branch forking off main and merging back"
        />
        <h2>Commits that remember why.</h2>
        <p className="section-sub">
          Turn on the commit hook in a repository and every commit made from a registered agent pane
          carries <code>Repowire-Thread</code> trailers for the asks it closed since the last commit.{" "}
          <code>repowire why</code> replays those threads: who asked, who answered, and how it closed.
        </p>
        <pre className="inline-code">
          <code>
            <span className="cb-prompt">$</span>
            repowire setup --git-hooks
          </code>
        </pre>
      </article>
      <article className="pair-cell" id="schedules">
        <PackageFigure
          name="slow"
          caption="Jobs keep moving through the queue with their state and results, whether or not anyone is watching."
          label="Crates riding a conveyor belt through a gate, standing for durable jobs"
        />
        <h2>Work that outlives the session.</h2>
        <p className="section-sub">
          Durable jobs carry lifecycle and results, one-off or on a cron. Schedules wake a peer later,
          and an orchestrator session can keep the queue moving while you are away.
        </p>
      </article>
    </section>
  );
}

export default function Chapters() {
  return (
    <>
      <Runtimes />
      <Phone />
      <Pair />
    </>
  );
}
