import Link from "next/link";
import { ArrowRight } from "lucide-react";

export const metadata = {
  title: "Quickstart · Repowire Docs",
};

export default function Quickstart() {
  return (
    <article className="max-w-3xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Quickstart
      </p>
      <h1 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        First cross-repo ask in three steps
      </h1>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        Install Repowire, run setup, open two agents in tmux. They auto-register and discover each other. Tested on macOS and Linux with Python 3.10+.
      </p>

      <Step n="01" title="Install">
        <CodeBlock>
{`# recommended: install with uv (fast, isolated)
uv tool install repowire

# or use the interactive installer (detects uv / pipx / pip)
curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh`}
        </CodeBlock>
        <p className="mt-3 text-sm leading-6 text-on-surface-variant">
          Requires tmux. The installer detects your agents (Claude Code, Codex, Gemini, OpenCode) and wires hooks and MCP for each one it finds.
        </p>
      </Step>

      <Step n="02" title="Set up">
        <CodeBlock>{`repowire setup`}</CodeBlock>
        <p className="mt-3 text-sm leading-6 text-on-surface-variant">
          One-time. Installs lifecycle hooks for every detected agent runtime and starts the local daemon on <code className="font-mono text-primary-fixed">127.0.0.1:8377</code>.
        </p>
      </Step>

      <Step n="03" title="Open two agents and ask">
        <CodeBlock>
{`# window 1
cd ~/projects/project-a && claude

# window 2
cd ~/projects/project-b && codex`}
        </CodeBlock>
        <p className="mt-3 text-sm leading-6 text-on-surface-variant">
          Both sessions auto-register as peers. In <code className="font-mono text-primary-fixed">project-a</code>, ask:
        </p>
        <div className="mt-3 border-l-2 border-primary/60 bg-surface-container-low p-4 font-mono text-sm leading-6 text-on-surface">
          &ldquo;Ask project-b what API endpoints they expose.&rdquo;
        </div>
        <p className="mt-3 text-sm leading-6 text-on-surface-variant">
          The agent invokes the <code className="font-mono text-primary-fixed">ask</code> MCP tool with <code className="font-mono text-primary-fixed">peer_name=&quot;project-b&quot;</code>. <code className="font-mono text-primary-fixed">project-b</code> receives the question and acks back with <code className="font-mono text-primary-fixed">ack(corr_id, &quot;...&quot;)</code>. The reply lands in <code className="font-mono text-primary-fixed">project-a</code> as a notification framed <code className="font-mono text-primary-fixed">[ack #cid from @project-b]</code>.
        </p>
      </Step>

      <div className="mt-12 border-t border-border-faint pt-8">
        <div className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
          Next
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <NextCard href="/docs/concepts" title="Concepts" desc="Peers, circles, ask vs notify vs broadcast." />
          <NextCard href="/docs/reference/tools" title="MCP tools" desc="ask, ack, notify_peer, broadcast, and friends." />
        </div>
      </div>
    </article>
  );
}

function Step({ n, title, children }: { n: string; title: string; children: React.ReactNode }) {
  return (
    <section className="mt-10 border-l-2 border-primary/40 pl-6">
      <div className="mb-2 font-mono text-[10px] font-bold uppercase tracking-[0.18em] text-primary-fixed">
        {n}
      </div>
      <h2 className="font-headline text-xl font-semibold text-on-surface">{title}</h2>
      <div className="mt-4">{children}</div>
    </section>
  );
}

function CodeBlock({ children }: { children: React.ReactNode }) {
  return (
    <pre className="overflow-x-auto border border-border-faint bg-surface-container-low p-4 font-mono text-xs leading-6 text-on-surface">
      <code>{children}</code>
    </pre>
  );
}

function NextCard({ href, title, desc }: { href: string; title: string; desc: string }) {
  return (
    <Link
      href={href}
      className="group block border border-border-faint bg-surface-container-low p-4 transition-colors hover:bg-surface-container"
    >
      <div className="flex items-center justify-between gap-3">
        <h3 className="font-headline text-sm font-semibold text-on-surface">{title}</h3>
        <ArrowRight className="h-4 w-4 text-outline transition-colors group-hover:text-primary" />
      </div>
      <p className="mt-2 text-xs leading-5 text-on-surface-variant">{desc}</p>
    </Link>
  );
}
