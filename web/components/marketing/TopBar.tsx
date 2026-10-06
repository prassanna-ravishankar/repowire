"use client";

import { useState } from "react";
import { Menu, X } from "lucide-react";
import ThemeToggle from "./ThemeToggle";
import GitHubMark from "./GitHubMark";
import { DOCS_URL, GITHUB_URL, RELAY_DASHBOARD_URL, RELEASES_URL } from "./links";

const NAV_LINKS = [
  { label: "How it works", href: "#wire" },
  { label: "Docs", href: DOCS_URL },
  { label: "Relay", href: RELAY_DASHBOARD_URL },
  { label: "Changelog", href: RELEASES_URL },
];

export default function TopBar() {
  const [mobileOpen, setMobileOpen] = useState(false);

  return (
    <header className="topbar">
      <div className="topbar-inner">
        <a className="brand" href="#">
          <span className="brand-mark" aria-hidden />
          <span>Repowire</span>
        </a>
        <nav className="topnav">
          {NAV_LINKS.map((link) => (
            <a key={link.href} href={link.href}>{link.label}</a>
          ))}
        </nav>
        <div className="top-actions">
          <ThemeToggle />
          <a className="icon-btn" href={GITHUB_URL} target="_blank" rel="noreferrer" aria-label="GitHub">
            <GitHubMark size={16} />
          </a>
          <a className="cta" href="#install">Install</a>
          <button
            className="icon-btn mobile-toggle"
            aria-label={mobileOpen ? "Close menu" : "Open menu"}
            aria-expanded={mobileOpen}
            onClick={() => setMobileOpen((open) => !open)}
          >
            {mobileOpen ? <X width={18} height={18} strokeWidth={1.75} /> : <Menu width={18} height={18} strokeWidth={1.75} />}
          </button>
        </div>
      </div>

      {mobileOpen && (
        <div className="mobile-menu">
          {NAV_LINKS.map((link) => (
            <a key={link.href} href={link.href} onClick={() => setMobileOpen(false)}>
              {link.label}
            </a>
          ))}
          <a href={GITHUB_URL} target="_blank" rel="noreferrer" onClick={() => setMobileOpen(false)}>
            GitHub
          </a>
          <a className="mobile-menu-cta" href="#install" onClick={() => setMobileOpen(false)}>
            Install
          </a>
        </div>
      )}
    </header>
  );
}
