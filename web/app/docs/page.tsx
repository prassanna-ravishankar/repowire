import Link from "next/link";
import { ArrowRight } from "lucide-react";
import { docsNav } from "./_nav";

export default function DocsIndex() {
  return (
    <article className="max-w-3xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Docs
      </p>
      <h1 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        Repowire documentation
      </h1>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        Repowire routes messages between live AI coding agents on your machine. Start with the quickstart, then read concepts when you want to understand what the daemon is doing.
      </p>

      <div className="mt-10 space-y-8">
        {docsNav.map((section) => (
          <section key={section.label}>
            <div className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
              {section.label}
            </div>
            <ul className="grid gap-px overflow-hidden border border-border-faint bg-border-faint sm:grid-cols-2">
              {section.items.map((item) => (
                <li key={item.href} className="bg-surface-container-low">
                  <Link
                    href={item.href}
                    className="group block h-full p-5 transition-colors hover:bg-surface-container"
                  >
                    <div className="flex items-center justify-between gap-3">
                      <h2 className="font-headline text-base font-semibold text-on-surface">
                        {item.label}
                      </h2>
                      <ArrowRight className="h-4 w-4 text-outline transition-colors group-hover:text-primary" />
                    </div>
                    <p className="mt-2 text-sm leading-6 text-on-surface-variant">{item.summary}</p>
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    </article>
  );
}
