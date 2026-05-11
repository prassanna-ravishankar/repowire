import { Network, RefreshCw, Server, Share2, Shield, Zap } from "lucide-react";

const features = [
  {
    name: "Sync communication",
    description: "Message active coding sessions without pasting context between terminals.",
    icon: Zap,
  },
  {
    name: "Multi-repo context",
    description: "Ask questions about code in another repository without leaving the current session.",
    icon: Share2,
  },
  {
    name: "Tmux integration",
    description: "Discover sessions, inject asks, and capture responses through the default hooks transport.",
    icon: Server,
  },
  {
    name: "Daemon routing",
    description: "A local FastAPI daemon keeps peer state and routes asks, notifications, and broadcasts.",
    icon: Network,
  },
  {
    name: "Local by default",
    description: "Communication stays on your machine unless you explicitly enable relay mode.",
    icon: Shield,
  },
  {
    name: "Lazy repair",
    description: "No polling loops. Liveness and persistence work happens when the mesh is already active.",
    icon: RefreshCw,
  },
];

export default function Features() {
  return (
    <section className="border-b border-border-faint bg-surface py-14 sm:py-20 lg:py-24">
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        <div className="max-w-2xl">
          <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
            Features
          </p>
          <h2 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
            Built for live agent work
          </h2>
          <p className="mt-4 text-lg leading-8 text-on-surface-variant">
            Repowire sits between isolated agent sessions and gives them a shared routing layer.
          </p>
        </div>

        <div className="mt-12 grid gap-px overflow-hidden border border-border-faint bg-border-faint md:grid-cols-2 lg:grid-cols-3">
          {features.map((feature) => (
            <div key={feature.name} className="bg-surface-container-low p-6 transition-colors hover:bg-surface-container">
              <feature.icon className="h-5 w-5 text-primary" aria-hidden="true" />
              <h3 className="mt-5 font-headline text-base font-semibold text-on-surface">{feature.name}</h3>
              <p className="mt-3 text-sm leading-6 text-on-surface-variant">{feature.description}</p>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
