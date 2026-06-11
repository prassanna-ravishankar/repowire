type IconName = "ask" | "review" | "handoff" | "context" | "checkpoint" | "compare" | "escalate" | "share";

const secondary: { icon: IconName; title: string; body: string }[] = [
  {
    icon: "review",
    title: "Cross-agent review",
    body: "Ask the peer nearest the code to review a component, migration, prompt, or PR branch before you merge it.",
  },
  {
    icon: "handoff",
    title: "Repo-to-repo handoff",
    body: "Ask the API repo what changed, then let the frontend peer adapt against the answer it acks back.",
  },
  {
    icon: "context",
    title: "Live context lookup",
    body: "Ask another session for the exact file, endpoint, schema, or command output from its own checkout.",
  },
  {
    icon: "checkpoint",
    title: "Checkpoint with closure",
    body: "Use ask when a status update must close cleanly. Open threads keep resurfacing until the peer acks.",
  },
  {
    icon: "compare",
    title: "Divergent second opinion",
    body: "Ask a different backend to critique an approach while the first agent keeps the original thread moving.",
  },
  {
    icon: "escalate",
    title: "Human escalation",
    body: "Route the same ask shape through Telegram, Slack, or the dashboard when a person needs to decide.",
  },
];

export default function Features() {
  return (
    <section className="features" id="features">
      <div className="section-head">
        <span className="eyebrow">What Repowire does</span>
        <h2>Built around one idea: agents that can ask.</h2>
      </div>

      <div className="feature-hero">
        <div className="feature-hero-copy">
          <div className="feature-icon feature-icon-lg">
            <FeatureIcon name="ask" />
          </div>
          <h3>Ask across repos</h3>
          <p>
            Send a question to the peer that&apos;s already working in another checkout. It answers
            from its live tree and sends back an explicit ack, never a vibes-based reply or a
            copy-paste handoff.
          </p>
        </div>
        <div className="feature-hero-visual">
          <div className="mesh-log-rows">
            <AskRow t="14:02" from="@backend" verb="ask" to="@frontend" body="What's the auth response shape from /me?" />
            <AskRow t="14:02" from="@frontend" verb="ack" to="@backend" body="{ user, session } — no nested wrapper." />
            <AskRow t="14:04" from="@db-migrations" verb="notify" body="Migration 0042 applied." />
          </div>
        </div>
      </div>

      <div className="feature-grid feature-grid-six">
        {secondary.map((it) => (
          <div className="feature" key={it.title}>
            <div className="feature-icon">
              <FeatureIcon name={it.icon} />
            </div>
            <h3>{it.title}</h3>
            <p>{it.body}</p>
          </div>
        ))}
      </div>

      <div className="feature feature-wide">
        <div className="feature-wide-content">
          <div className="feature-wide-copy">
            <div className="feature-icon">
              <FeatureIcon name="share" />
            </div>
            <span className="feature-new-badge">New</span>
            <h3>Share a session</h3>
            <p>
              Generate a shareable link for any running agent peer.
              Read-only observers watch the live event stream in a browser — no login, no Repowire
              install. Flip to read-write and let a colleague inject asks directly.
            </p>
            <code className="feature-cmd">repowire share my-agent [--rw] [--ttl 3600]</code>
          </div>
          <div className="feature-wide-demo">
            <div className="share-demo-pill share-demo-ro">read-only</div>
            <div className="share-demo-url">repowire.io/s/sh_xxxxxxxx</div>
            <div className="share-demo-pill share-demo-rw">read-write</div>
          </div>
        </div>
      </div>
    </section>
  );
}

function AskRow({
  t,
  from,
  verb,
  to,
  body,
}: {
  t: string;
  from: string;
  verb: "ask" | "ack" | "notify";
  to?: string;
  body: string;
}) {
  const verbClass = { ask: "v-ask", ack: "v-ack", notify: "v-notify" }[verb];
  return (
    <div className="mesh-log-row">
      <div className="mesh-log-when">{t}</div>
      <div>
        <div className="mesh-log-head-row">
          <span className="mesh-log-peer">{from}</span>
          <span className={`verb-pill ${verbClass}`}>{verb}</span>
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

function FeatureIcon({ name }: { name: IconName }) {
  const stroke = {
    stroke: "currentColor",
    strokeWidth: 1.75,
    fill: "none",
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
  };
  const props = { width: 20, height: 20, viewBox: "0 0 24 24", ...stroke };
  switch (name) {
    case "ask":
      return (
        <svg {...props}>
          <path d="M16 18l6-6-6-6M8 6l-6 6 6 6" />
        </svg>
      );
    case "review":
      return (
        <svg {...props}>
          <path d="M4 5h16v12H7l-3 3z" />
          <path d="M8 10h8" />
          <path d="M8 13h5" />
        </svg>
      );
    case "handoff":
      return (
        <svg {...props}>
          <path d="M7 7h10" />
          <path d="M14 4l3 3-3 3" />
          <path d="M17 17H7" />
          <path d="M10 14l-3 3 3 3" />
        </svg>
      );
    case "context":
      return (
        <svg {...props}>
          <circle cx="11" cy="11" r="7" />
          <path d="M16.5 16.5 21 21" />
          <path d="M8 11h6" />
          <path d="M11 8v6" />
        </svg>
      );
    case "checkpoint":
      return (
        <svg {...props}>
          <path d="M20 6 9 17l-5-5" />
          <path d="M4 20h16" />
        </svg>
      );
    case "compare":
      return (
        <svg {...props}>
          <path d="M5 7h14" />
          <path d="M7 7l3 12" />
          <path d="M17 7l-3 12" />
          <path d="M3 19h18" />
        </svg>
      );
    case "escalate":
      return (
        <svg {...props}>
          <circle cx="12" cy="8" r="4" />
          <path d="M4 21a8 8 0 0 1 16 0" />
        </svg>
      );
    case "share":
      return (
        <svg {...props}>
          <circle cx="18" cy="5" r="3" />
          <circle cx="6" cy="12" r="3" />
          <circle cx="18" cy="19" r="3" />
          <path d="M8.59 13.51 15.42 17.49" />
          <path d="M15.41 6.51 8.59 10.49" />
        </svg>
      );
  }
}
