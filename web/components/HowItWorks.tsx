import Image from "next/image";

const steps = [
  {
    label: "01",
    title: "Sessions register as peers",
    description: "Setup wires supported agents into the mesh. Each live Claude Code, Codex, Gemini, OpenCode, or Pi session gets a routable identity.",
  },
  {
    label: "02",
    title: "A message enters the daemon",
    description: "Agents and human surfaces send asks, notifications, broadcasts, or schedules through MCP, hooks, the dashboard, Telegram, Slack, or the CLI.",
  },
  {
    label: "03",
    title: "The router chooses delivery",
    description: "The local daemon checks peer state, circle access, and available transports, then forwards the message without needing every agent to know every integration.",
  },
  {
    label: "04",
    title: "The thread stays visible",
    description: "Open asks keep resurfacing until acked, and the dashboard timeline shows recent peer activity, tool calls, attachments, and routed messages.",
  },
];

export default function HowItWorks() {
  return (
    <section className="border-b border-border-faint bg-surface-dim py-14 sm:py-20 lg:py-24">
      <div className="mx-auto grid max-w-7xl gap-12 px-4 sm:px-6 lg:grid-cols-[0.95fr_1.05fr] lg:px-8">
        <div>
          <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
            How it works
          </p>
          <h2 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
            One routing layer for agents and humans
          </h2>
          <p className="mt-4 text-lg leading-8 text-on-surface-variant">
            The daemon is the hub. Agents remain the workers, transports handle delivery, and human surfaces become first-class peers you can use when you need to steer.
          </p>

          <div className="mt-10 space-y-4">
            {steps.map((step) => (
              <article key={step.label} className="rounded border border-border-faint bg-surface-container-low p-5">
                <div className="mb-2 font-mono text-[10px] font-bold uppercase tracking-[0.18em] text-primary-fixed">
                  {step.label}
                </div>
                <h3 className="font-headline text-base font-semibold text-on-surface">{step.title}</h3>
                <p className="mt-2 text-sm leading-6 text-on-surface-variant">{step.description}</p>
              </article>
            ))}
          </div>
        </div>

        <div className="space-y-5">
          <div className="relative overflow-hidden rounded-lg border border-border-faint bg-surface-container-low p-4 shadow-[var(--shadow-2)]">
            <div className="mb-4 font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-outline">
              Architecture, simplified
            </div>
            <Image
              src="/brand/repowire-arch.webp"
              alt="Repowire architecture: a local daemon routes messages between hooks, channel transport, relay, and peers"
              width={1000}
              height={700}
              className="h-auto w-full rounded border border-border-faint bg-surface opacity-90"
            />
            <div className="mt-4 rounded border-l-2 border-primary bg-surface-dim/95 p-4">
              <p className="font-mono text-xs leading-6 text-on-surface-variant">
                local daemon → transport router → agent session → ack or timeline event
              </p>
            </div>
          </div>

          <div className="rounded-lg border border-border-faint bg-surface-container-low p-5">
            <h3 className="font-headline text-base font-semibold text-on-surface">Roadmap, without overclaiming</h3>
            <p className="mt-3 text-sm leading-6 text-on-surface-variant">
              Repowire is moving toward a session-native model where sessions are the durable unit of work and commands such as resume, scheduling, approvals, and future backend/model controls share one command surface. Those controls are being shipped in compatible slices, not all at once.
            </p>
          </div>
        </div>
      </div>
    </section>
  );
}
