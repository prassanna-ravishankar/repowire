"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { DocsNavSection } from "./_nav";

export default function DocsSidebar({ sections }: { sections: DocsNavSection[] }) {
  const pathname = usePathname();

  return (
    <aside className="hidden w-56 shrink-0 lg:block">
      <nav className="sticky top-24 space-y-7">
        {sections.map((section) => (
          <div key={section.label}>
            <div className="mb-2 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
              {section.label}
            </div>
            <ul className="space-y-1">
              {section.items.map((item) => {
                const active = pathname === item.href || pathname.startsWith(item.href + "/");
                return (
                  <li key={item.href}>
                    <Link
                      href={item.href}
                      className={
                        "block border-l-2 px-3 py-1.5 font-mono text-xs transition-colors " +
                        (active
                          ? "border-primary bg-surface-container-low text-primary-fixed"
                          : "border-transparent text-on-surface-variant hover:border-border hover:text-on-surface")
                      }
                    >
                      {item.label}
                    </Link>
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </nav>
    </aside>
  );
}
