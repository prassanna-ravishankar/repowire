import { ArrowUpRight } from "lucide-react";
import CopyButton from "./CopyButton";
import MeshFigure from "./MeshFigure";
import { BREW_INSTALL, DOCS_URL, RELAY_DASHBOARD_URL } from "./links";

const SETUP = `${BREW_INSTALL}\nrepowire setup`;

export default function Hero() {
  return (
    <section className="hero">
      <div className="hero-copy">
        <p className="kicker">
          <span className="kicker-dot" aria-hidden />
          Local-first · open source · MIT
        </p>
        <h1>
          Your coding agents, <em>on speaking terms.</em>
        </h1>
        <p className="lead">
          Repowire puts every Claude Code, Codex, OpenCode and Pi session on one local mesh. Each
          gets an address. They ask each other questions, answer with an ack, and stay steerable
          from your browser or your phone.
        </p>
        <div className="codeblock" aria-label="Install">
          <pre>
            <code>
              <span className="cb-line"><span className="cb-prompt">$</span>{BREW_INSTALL}</span>
              <span className="cb-line"><span className="cb-prompt">$</span>repowire setup</span>
            </code>
          </pre>
          <CopyButton text={SETUP} label="Copy install commands" />
        </div>
        <div className="hero-links">
          <a className="btn primary" href="#install">Get started</a>
          <a className="btn ghost" href={DOCS_URL}>
            Read the docs <ArrowUpRight width={15} height={15} strokeWidth={1.5} />
          </a>
          <a className="btn ghost" href={RELAY_DASHBOARD_URL}>
            Open the relay <ArrowUpRight width={15} height={15} strokeWidth={1.5} />
          </a>
        </div>
      </div>
      <div className="hero-figure">
        <MeshFigure fig="1" caption="Every agent session gets an address on one local mesh, and a message finds its peer hop by hop through the daemon." />
      </div>
    </section>
  );
}
