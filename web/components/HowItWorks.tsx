import Image from "next/image";

const steps = [
  {
    label: "01",
    title: "Ask enters the daemon",
    description: "The source peer sends an ask, notify, or broadcast through MCP, dashboard, Telegram, Slack, or a hook.",
  },
  {
    label: "02",
    title: "Router finds the target",
    description: "The daemon checks peer state, circle access, and transport availability, then forwards the message.",
  },
  {
    label: "03",
    title: "Transport delivers context",
    description: "Hooks inject framed text into the agent session, or the channel transport emits a structured message.",
  },
  {
    label: "04",
    title: "Response returns to mesh",
    description: "Stop hooks and ack tools capture the reply and close the ask lifecycle without a polling loop.",
  },
];

export default function HowItWorks() {
  return (
    <section className="border-b border-border-faint bg-surface-dim py-14 sm:py-20 lg:py-24">
      <div className="mx-auto grid max-w-7xl gap-12 px-4 sm:px-6 lg:grid-cols-[0.9fr_1.1fr] lg:px-8">
        <div>
          <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
            How it works
          </p>
          <h2 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
            A broker for active sessions
          </h2>
          <p className="mt-4 text-lg leading-8 text-on-surface-variant">
            Repowire does one job: route messages between running agents and the human control surfaces around them.
          </p>

          <div className="mt-10 space-y-4">
            {steps.map((step) => (
              <div key={step.label} className="border-l-2 border-primary/60 bg-surface-container-low p-5">
                <div className="mb-2 font-mono text-[10px] font-bold uppercase tracking-[0.18em] text-primary-fixed">
                  {step.label}
                </div>
                <h3 className="font-headline text-base font-semibold text-on-surface">{step.title}</h3>
                <p className="mt-2 text-sm leading-6 text-on-surface-variant">{step.description}</p>
              </div>
            ))}
          </div>
        </div>

        <div className="relative overflow-hidden border border-border-faint bg-surface-container-low p-4 sm:min-h-[360px]">
          <div className="mb-4 font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-outline">
            System architecture
          </div>
          <Image
            src="/brand/repowire-arch.webp"
            alt="Repowire architecture"
            width={1000}
            height={700}
            className="h-auto w-full object-cover opacity-80 sm:h-full sm:min-h-[300px]"
          />
          <div className="mt-4 border-l-2 border-primary bg-surface-dim/95 p-4 sm:absolute sm:inset-x-4 sm:bottom-4 sm:mt-0">
            <p className="font-mono text-xs leading-6 text-on-surface-variant">
              daemon/ws {">"} hooks/channel {">"} peer session {">"} response capture
            </p>
          </div>
        </div>
      </div>
    </section>
  );
}
