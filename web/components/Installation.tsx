"use client";

import { Check, Copy } from "lucide-react";
import { useState } from "react";

export default function Installation() {
  const [copied, setCopied] = useState(false);

  const copyToClipboard = () => {
    navigator.clipboard.writeText("uv tool install repowire");
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <section id="installation" className="border-b border-border-faint bg-surface py-14 sm:py-20 lg:py-24">
      <div className="mx-auto max-w-7xl px-4 text-center sm:px-6 lg:px-8">
        <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
          Install
        </p>
        <h2 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
          Start the mesh locally
        </h2>
        <p className="mx-auto mt-4 max-w-2xl text-lg leading-8 text-on-surface-variant">
          Install with uv. Repowire supports macOS/Linux and Python 3.10+.
        </p>

        <div className="mx-auto mt-8 max-w-2xl border border-border-faint bg-surface-container-low p-4 text-left shadow-[var(--shadow-2)]">
          <div className="flex items-center justify-between gap-4 font-mono text-sm text-on-surface">
            <div className="flex min-w-0 items-center">
              <span className="mr-2 text-primary">$</span>
              <span className="truncate">uv tool install repowire</span>
            </div>
            <button
              onClick={copyToClipboard}
              className="rounded p-2 text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface focus:outline-none focus:ring-1 focus:ring-primary"
              aria-label="Copy to clipboard"
            >
              {copied ? <Check className="h-5 w-5 text-secondary" /> : <Copy className="h-5 w-5" />}
            </button>
          </div>
        </div>

        <div className="mt-8 font-mono text-xs leading-6 text-outline">
          <p>Alternative: <code className="text-primary-fixed">pip install repowire</code></p>
          <p className="mt-2">
            Full setup lives in the{" "}
            <a href="https://github.com/prassanna-ravishankar/repowire" className="text-primary-fixed hover:underline">
              repository documentation
            </a>.
          </p>
        </div>
      </div>
    </section>
  );
}
