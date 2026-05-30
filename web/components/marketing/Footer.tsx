import Image from "next/image";
import { RELAY_DASHBOARD_URL } from "./links";

export default function Footer() {
  return (
    <footer className="footer">
      <div className="footer-inner">
        <div className="footer-brand">
          <div className="brand">
            <Image src="/brand/logo-mark.svg" width={20} height={22} alt="" style={{ height: 22, width: "auto" }} />
            <span>Repowire</span>
          </div>
          <p>A peer-to-peer mesh for AI coding agents.</p>
        </div>
        <div className="footer-cols">
          <div>
            <div className="footer-col-title">Product</div>
            <a href="#features">Features</a>
            <a href="#how">How it works</a>
            <a href="https://github.com/prassanna-ravishankar/repowire/releases">Changelog</a>
          </div>
          <div>
            <div className="footer-col-title">Developers</div>
            <a href="https://docs.repowire.io">Docs</a>
            <a href="https://docs.repowire.io/start/install/">Install</a>
            <a href={RELAY_DASHBOARD_URL}>Relay dashboard</a>
            <a href="https://github.com/prassanna-ravishankar/repowire">GitHub</a>
            <a href="https://docs.repowire.io/reference/mcp-tools/">API reference</a>
          </div>
        </div>
      </div>
      <div className="footer-bottom">
        <span>© 2026 Repowire</span>
        <span>Built with care in the open.</span>
      </div>
    </footer>
  );
}
