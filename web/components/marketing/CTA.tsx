import CopyButton from "./CopyButton";

const INSTALL_CMD = "curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh";

export default function CTA() {
  return (
    <section className="cta-band" id="install">
      <h2>Wire up your agents in one command.</h2>
      <p>Free, local, and open source. The hosted relay is opt-in — only there for browser and phone access.</p>
      <div className="install-strip large">
        <div className="install-cmd">
          <span className="install-prompt">$</span>
          <span className="install-text">{INSTALL_CMD}</span>
        </div>
        <CopyButton text={INSTALL_CMD} />
      </div>
      <div className="cta-meta">Requires macOS or Linux · tmux 3.0+ · no Python required</div>
    </section>
  );
}
