"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { ChevronDown } from "lucide-react";
import type { DocsNavSection } from "./_nav";

export default function DocsSidebar({ sections }: { sections: DocsNavSection[] }) {
  const pathname = usePathname();

  return (
    <>
      <div className="lg:hidden">
        <details className="border-b border-border-faint pb-4">
          <summary className="flex cursor-pointer list-none items-center justify-between py-3 font-mono text-[10px] font-semibold uppercase tracking-[0.16em] text-primary-fixed [&::-webkit-details-marker]:hidden">
            Docs navigation
            <ChevronDown className="h-4 w-4 text-outline" />
          </summary>
          <nav className="grid gap-5 pb-3 sm:grid-cols-2">
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
                            "block border-l-2 px-3 py-2 font-mono text-xs transition-colors " +
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
        </details>
      </div>

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
    </>
  );
}
