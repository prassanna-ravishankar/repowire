"use client";

import { useState } from "react";
import Image from "next/image";
import { Menu, X } from "lucide-react";
import ThemeToggle from "./ThemeToggle";
import GitHubMark from "./GitHubMark";
import { RELAY_DASHBOARD_URL } from "./links";

const GITHUB_URL = "https://github.com/prassanna-ravishankar/repowire";

const NAV_LINKS = [
  { label: "Product", href: "#features" },
  { label: "Docs", href: "https://docs.repowire.io" },
  { label: "Relay", href: RELAY_DASHBOARD_URL },
  { label: "Changelog", href: "https://github.com/prassanna-ravishankar/repowire/releases" },
];

export default function TopBar() {
  const [mobileOpen, setMobileOpen] = useState(false);

  return (
    <header className="topbar">
      <div className="topbar-inner">
        <a className="brand" href="#">
          <Image src="/brand/logo-mark.svg" width={20} height={22} alt="" priority style={{ height: 22, width: "auto" }} />
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
          <a className="cta" href="#install">Get started</a>
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
            Get started
          </a>
        </div>
      )}
    </header>
  );
}
