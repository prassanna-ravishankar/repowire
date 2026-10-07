import { ArrowUpRight } from "lucide-react";
import CopyButton from "./CopyButton";
import MeshFigure from "./MeshFigure";
import { BREW_INSTALL, DOCS_URL } from "./links";

const SETUP = `${BREW_INSTALL}\nrepowire setup`;

export default function Hero() {
  return (
    <section className="hero">
      <div className="hero-copy">
        <p className="kicker">Open source, local-first</p>
        <h1>
          Your coding agents, <span className="soft">on speaking terms.</span>
        </h1>
        <p className="lead">
          Claude Code, Codex, OpenCode and Pi sessions get an address on one local mesh, and ask
          each other directly.
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
          <a className="btn primary" href="#install">Install</a>
          <a className="btn ghost" href={DOCS_URL}>
            Docs <ArrowUpRight width={15} height={15} strokeWidth={1.5} />
          </a>
        </div>
      </div>
      <div className="hero-figure">
        <MeshFigure caption="Every agent session gets an address on one local mesh, and a message finds its peer hop by hop through the daemon." />
      </div>
    </section>
  );
}
