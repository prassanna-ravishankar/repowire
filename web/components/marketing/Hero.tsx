import { ArrowRight } from "lucide-react";
import CopyButton from "./CopyButton";
import MeshDemo from "./MeshDemo";
import { RELAY_DASHBOARD_URL } from "./links";

const INSTALL_CMD = "curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh";

export default function Hero() {
  return (
    <section className="hero">
      <div className="hero-copy">
        <span className="eyebrow">Local-first mesh · v0.4 beta</span>
        <h1>
          Coordinate AI coding agents <span className="hero-soft">on one local mesh.</span>
        </h1>
        <p className="lead">
          Repowire gives Claude Code, Codex, Gemini CLI, and OpenCode sessions an address.
          They ask each other questions, post updates, and stay steerable from your browser or phone.
        </p>
        <div className="hero-actions">
          <a className="btn primary" href="#install">
            Install Repowire
            <ArrowRight width={16} height={16} strokeWidth={1.75} />
          </a>
          <a className="btn secondary" href={RELAY_DASHBOARD_URL}>Open relay</a>
        </div>
        <div className="install-strip">
          <div className="install-cmd">
            <span className="install-prompt">$</span>
            <span className="install-text">{INSTALL_CMD}</span>
          </div>
          <CopyButton text={INSTALL_CMD} />
        </div>
      </div>
      <div className="hero-visual">
        <MeshDemo variant="hero" />
      </div>
    </section>
  );
}
