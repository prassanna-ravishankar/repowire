import CopyButton from "./CopyButton";
import { BREW_INSTALL, CURL_INSTALL, DOCS_URL } from "./links";

const STEPS = [
  {
    title: "Install and wire your agents",
    lines: [BREW_INSTALL, "repowire setup"],
    note: <>Or the checksum-verified native installer: <code>{CURL_INSTALL}</code></>,
  },
  {
    title: "Open your agents as usual",
    lines: ["cd ~/projects/api && claude", "cd ~/projects/web && codex"],
    note: <>One per tmux window. Setup already hooked them in; there is nothing new to launch.</>,
  },
  {
    title: "Check they found each other",
    lines: ["repowire peer list"],
    note: <>Claude Code registers on session start, Codex when its thread opens.</>,
  },
  {
    title: "Ask across",
    lines: ["Ask web what the auth response shape is."],
    prompt: "❯",
    note: <>Say it to your agent in plain words. It calls <code>ask</code>, and the answer comes back as an ack.</>,
  },
];

export default function Install() {
  return (
    <section className="section install" id="install">
      <header className="section-head">
        <p className="section-num">§ 9</p>
        <h2>Four steps to a mesh.</h2>
        <p className="section-sub">
          macOS or Linux. Tmux for the default Claude Code workflow. No Python, no account, no
          inbound ports.
        </p>
      </header>
      <ol className="steps">
        {STEPS.map((s, i) => (
          <li key={s.title}>
            <span className="step-index">{i + 1}</span>
            <div className="step-body">
              <h3>{s.title}</h3>
              <div className="codeblock">
                <pre>
                  <code>
                    {s.lines.map((l) => (
                      <span className="cb-line" key={l}>
                        <span className="cb-prompt">{s.prompt ?? "$"}</span>
                        {l}
                      </span>
                    ))}
                  </code>
                </pre>
                {!s.prompt && <CopyButton text={s.lines.join("\n")} label={`Copy: ${s.title}`} />}
              </div>
              <p className="step-note">{s.note}</p>
            </div>
          </li>
        ))}
      </ol>
      <p className="install-more">
        Spawning peers, profiles, circles and the relay are in the <a href={DOCS_URL}>docs</a>.
      </p>
    </section>
  );
}
