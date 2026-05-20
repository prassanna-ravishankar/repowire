"use client";

import { Check, Copy } from "lucide-react";
import { useState } from "react";

const installCommand = "curl -sSf https://raw.githubusercontent.com/prassanna-ravishankar/repowire/main/install.sh | sh";

export default function Installation() {
  const [copied, setCopied] = useState(false);

  const copyToClipboard = async () => {
    await navigator.clipboard.writeText(installCommand);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <section id="installation" className="border-b border-border-faint bg-surface py-14 sm:py-20 lg:py-24">
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        <div className="grid gap-10 lg:grid-cols-[0.85fr_1.15fr] lg:items-center">
          <div>
            <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
              Install
            </p>
            <h2 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
              Start with two local sessions
            </h2>
            <p className="mt-4 text-lg leading-8 text-on-surface-variant">
              Repowire runs on macOS or Linux with Python 3.10+ and tmux. The one-liner installs the CLI and runs setup; manual uv, pipx, and pip paths are documented too.
            </p>
          </div>

          <div className="rounded-lg border border-border-faint bg-surface-container-low p-4 text-left shadow-[var(--shadow-2)]">
            <div className="mb-3 flex items-center justify-between border-b border-border-faint pb-3 font-mono text-[10px] uppercase tracking-[0.16em] text-outline">
              <span>quickstart</span>
              <span>local-first</span>
            </div>
            <div className="flex items-start justify-between gap-4 font-mono text-sm text-on-surface">
              <div className="min-w-0 leading-7">
                <span className="mr-2 text-primary">$</span>
                <span className="break-all">{installCommand}</span>
              </div>
              <button
                onClick={copyToClipboard}
                className="rounded p-2 text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface focus:outline-none focus:ring-2 focus:ring-primary-fixed"
                aria-label="Copy install command"
              >
                {copied ? <Check className="h-5 w-5 text-secondary" /> : <Copy className="h-5 w-5" />}
              </button>
            </div>
            <div className="mt-5 grid gap-3 border-t border-border-faint pt-4 text-sm leading-6 text-on-surface-variant sm:grid-cols-3">
              <Step label="1" text="Run setup" />
              <Step label="2" text="Open two agent sessions" />
              <Step label="3" text="Ask one peer from another" />
            </div>
            <p className="mt-4 font-mono text-xs leading-6 text-outline">
              Prefer manual install? <code className="text-primary-fixed">uv tool install repowire && repowire setup</code>
            </p>
          </div>
        </div>
      </div>
    </section>
  );
}

function Step({ label, text }: { label: string; text: string }) {
  return (
    <div className="rounded border border-border-faint bg-surface/60 p-3">
      <div className="mb-2 font-mono text-[10px] font-bold uppercase tracking-[0.18em] text-primary-fixed">{label}</div>
      <div>{text}</div>
    </div>
  );
}
