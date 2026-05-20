import { Bell, CalendarClock, GitBranch, MonitorSmartphone, Network, ShieldCheck } from "lucide-react";

const features = [
  {
    name: "Ask across repos",
    description: "Need the real API shape from another checkout? Ask the live peer that is already working there and get an explicit ack back.",
    icon: GitBranch,
  },
  {
    name: "Coordinate with an orchestrator",
    description: "Keep one session focused on dispatch, status checks, review handoffs, and follow-ups while project peers do the implementation work.",
    icon: Network,
  },
  {
    name: "Steer from browser or phone",
    description: "Use the dashboard, Telegram, or Slack as human peers when you want to monitor, nudge, or route work without returning to every terminal.",
    icon: MonitorSmartphone,
  },
  {
    name: "Schedule the next nudge",
    description: "Wake a peer later for CI checks, review reminders, morning handoffs, or any other deferred ask or notification.",
    icon: CalendarClock,
  },
  {
    name: "Local by default",
    description: "The daemon runs on your machine. The hosted relay is optional for remote dashboard access and cross-machine mesh traffic.",
    icon: ShieldCheck,
  },
  {
    name: "Transport-aware, not transport-locked",
    description: "Hooks and MCP are the stable path today; Claude Code channel/ACP delivery is experimental and opt-in as the routing boundary hardens.",
    icon: Bell,
  },
];

export default function Features() {
  return (
    <section className="border-b border-border-faint bg-surface py-14 sm:py-20 lg:py-24">
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        <div className="max-w-2xl">
          <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
            What changes day to day
          </p>
          <h2 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
            Less terminal babysitting. Better handoffs.
          </h2>
          <p className="mt-4 text-lg leading-8 text-on-surface-variant">
            Repowire is not another coding agent. It is the operating layer around the agents you already run, with practical messaging and control surfaces for multi-session work.
          </p>
        </div>

        <div className="mt-12 grid gap-px overflow-hidden rounded-lg border border-border-faint bg-border-faint md:grid-cols-2 lg:grid-cols-3">
          {features.map((feature) => (
            <article key={feature.name} className="bg-surface-container-low p-6 transition-colors hover:bg-surface-container">
              <feature.icon className="h-5 w-5 text-primary" aria-hidden="true" />
              <h3 className="mt-5 font-headline text-base font-semibold text-on-surface">{feature.name}</h3>
              <p className="mt-3 text-sm leading-6 text-on-surface-variant">{feature.description}</p>
            </article>
          ))}
        </div>
      </div>
    </section>
  );
}
