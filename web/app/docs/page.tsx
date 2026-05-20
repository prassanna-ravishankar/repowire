import Link from "next/link";
import { ArrowRight, BookOpen, Compass, Terminal, Wrench } from "lucide-react";
import { docsNav } from "./_nav";

const startHere = [
  {
    title: "Install and send your first ask",
    description: "Set up the daemon, open two agent sessions, and route a real question between repos.",
    href: "/docs/quickstart",
    icon: Terminal,
  },
  {
    title: "Understand the mesh",
    description: "Learn peers, circles, asks, notifications, schedules, and how human surfaces fit in.",
    href: "/docs/concepts",
    icon: Compass,
  },
  {
    title: "Use the MCP tools",
    description: "List peers, ask and ack, send notifications, spawn sessions, and schedule follow-ups from supported agents.",
    href: "/docs/reference/tools",
    icon: BookOpen,
  },
  {
    title: "Look up CLI commands",
    description: "Reference setup, serve, peer, schedule, build-ui, Telegram, Slack, and related operational commands.",
    href: "/docs/reference/cli",
    icon: Wrench,
  },
];

export default function DocsIndex() {
  return (
    <article className="max-w-5xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Docs
      </p>
      <h1 className="mt-3 max-w-3xl font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        Build a useful local agent mesh, then add surfaces as you need them.
      </h1>
      <p className="mt-4 max-w-3xl text-base leading-7 text-on-surface-variant">
        Repowire routes messages between live AI coding agents on your machine. Start with one ask across two sessions, then add orchestration, schedules, dashboard, Telegram, Slack, or relay when the workflow calls for it.
      </p>

      <section className="mt-10">
        <h2 className="sr-only">Start here</h2>
        <div className="grid gap-px overflow-hidden rounded-lg border border-border-faint bg-border-faint sm:grid-cols-2">
          {startHere.map((item) => (
            <Link
              key={item.href}
              href={item.href}
              className="group block bg-surface-container-low p-5 transition-colors hover:bg-surface-container focus:outline-none focus:ring-2 focus:ring-primary-fixed focus:ring-inset"
            >
              <item.icon className="h-5 w-5 text-primary" aria-hidden="true" />
              <div className="mt-4 flex items-center justify-between gap-3">
                <h3 className="font-headline text-base font-semibold text-on-surface">{item.title}</h3>
                <ArrowRight className="h-4 w-4 shrink-0 text-outline transition-colors group-hover:text-primary" />
              </div>
              <p className="mt-2 text-sm leading-6 text-on-surface-variant">{item.description}</p>
            </Link>
          ))}
        </div>
      </section>

      <div className="mt-12 space-y-8">
        {docsNav.map((section) => (
          <section key={section.label}>
            <div className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
              {section.label}
            </div>
            <ul className="grid gap-px overflow-hidden rounded-lg border border-border-faint bg-border-faint sm:grid-cols-2">
              {section.items.map((item) => (
                <li key={item.href} className="bg-surface-container-low">
                  <Link
                    href={item.href}
                    className="group block h-full p-5 transition-colors hover:bg-surface-container focus:outline-none focus:ring-2 focus:ring-primary-fixed focus:ring-inset"
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
