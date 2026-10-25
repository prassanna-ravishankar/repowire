import Link from "next/link";
import { CircleAlert, KeyRound, Loader, PlugZap } from "lucide-react";
import type { Gate } from "../lib/useMe";
import { PageMessage, buttonClass, primaryButtonClass } from "./ui";

/** Renders every gate state except `ready`. */
export function GateScreen({ gate, onRetry }: { gate: Exclude<Gate, { kind: "ready" }>; onRetry: () => void }) {
  switch (gate.kind) {
    case "loading":
      return (
        <div className="flex min-h-dvh items-center justify-center bg-background text-outline" aria-busy="true">
          <Loader className="h-5 w-5 animate-spin" />
        </div>
      );
    case "signed-out":
      return (
        <PageMessage
          icon={<KeyRound className="h-6 w-6" />}
          title="Sign in to continue"
          action={<Link href="/" className={primaryButtonClass}>Sign in</Link>}
        >
          Your relay session has ended or was never started.
        </PageMessage>
      );
    case "unconfigured":
      return (
        <PageMessage icon={<PlugZap className="h-6 w-6" />} title="Managed agents unavailable">
          <p className="font-mono text-[12px]">{gate.message}</p>
          <p className="mt-2 text-outline">
            The relay needs <code>REPOWIRE_MANAGED_URL</code> and <code>REPOWIRE_MANAGED_CONTROL_TOKEN</code>.
          </p>
        </PageMessage>
      );
    case "error":
      return (
        <PageMessage icon={<CircleAlert className="h-6 w-6" />} title="Could not load" action={<button onClick={onRetry} className={buttonClass}>Retry</button>}>
          <p className="font-mono text-[12px] text-error">{gate.message}</p>
        </PageMessage>
      );
  }
}
