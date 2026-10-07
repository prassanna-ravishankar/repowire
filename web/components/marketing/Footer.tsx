import { DOCS_URL, GITHUB_URL, RELAY_DASHBOARD_URL, RELEASES_URL } from "./links";

const COLS = [
  {
    title: "Product",
    links: [
      { label: "How it works", href: "#wire" },
      { label: "Install", href: "#install" },
      { label: "Changelog", href: RELEASES_URL },
    ],
  },
  {
    title: "Developers",
    links: [
      { label: "Docs", href: DOCS_URL },
      { label: "MCP tools", href: `${DOCS_URL}/reference/mcp-tools/` },
      { label: "Relay dashboard", href: RELAY_DASHBOARD_URL },
      { label: "GitHub", href: GITHUB_URL },
    ],
  },
];

export default function Footer() {
  return (
    <footer className="footer">
      <div className="footer-inner">
        <div className="footer-brand">
          <div className="brand">
            <span className="brand-mark" aria-hidden />
            <span>Repowire</span>
          </div>
          <p>A mesh for the coding agents you already run.</p>
        </div>
        <div className="footer-cols">
          {COLS.map((col) => (
            <div key={col.title}>
              <p className="footer-col-title">{col.title}</p>
              {col.links.map((l) => (
                <a key={l.label} href={l.href}>{l.label}</a>
              ))}
            </div>
          ))}
        </div>
      </div>
      <div className="footer-bottom">
        <span>© 2026 Repowire · MIT</span>
        <span>
          Figures drawn with <a href="https://hairline.lucasmarkes.com">Hairline</a>.
        </span>
      </div>
    </footer>
  );
}
