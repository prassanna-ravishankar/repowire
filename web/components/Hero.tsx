"use client";

import { ArrowRight, Github, Terminal } from "lucide-react";
import Image from "next/image";
import Link from "next/link";
import { motion } from "framer-motion";

export default function Hero() {
  return (
    <section className="relative overflow-hidden border-b border-border-faint pt-20 sm:pt-22 lg:pt-24">
      <Image
        src="/brand/repowire-arch.webp"
        alt=""
        width={1280}
        height={720}
        priority
        className="pointer-events-none absolute -right-40 bottom-0 hidden w-[820px] max-w-none opacity-25 mix-blend-screen lg:block"
      />
      <div className="absolute inset-0 bg-surface/88" />

      <div className="relative z-10 mx-auto grid max-w-7xl items-center gap-8 px-4 pb-10 sm:px-6 sm:pb-12 lg:grid-cols-[0.95fr_1.05fr] lg:px-8 lg:pb-14">
        <motion.div
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.4 }}
          className="max-w-3xl"
        >
          <div className="mb-6 inline-flex items-center gap-2 border border-primary/30 bg-primary/10 px-3 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-primary-fixed">
            <span className="h-1.5 w-1.5 rounded-full bg-secondary" aria-hidden="true" />
            Local-first agent team harness
          </div>
          <h1 className="font-headline text-4xl font-bold leading-tight tracking-normal text-on-surface sm:text-5xl">
            Coordinate agent teams locally.
          </h1>
          <p className="mt-5 max-w-2xl text-lg leading-8 text-on-surface-variant">
            Repowire gives Claude Code, Codex, Gemini CLI, OpenCode, and Pi sessions an address in one mesh so they can ask each other questions, send updates, schedule follow-ups, and stay steerable from your browser or phone.
          </p>
          <div className="mt-8 flex flex-col gap-3 sm:flex-row">
            <Link
              href="https://docs.repowire.io/quickstart/"
              className="inline-flex items-center justify-center gap-2 rounded bg-primary px-5 py-3 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 focus:outline-none focus:ring-2 focus:ring-primary-fixed focus:ring-offset-2 focus:ring-offset-surface active:scale-[0.98]"
            >
              Start in 5 minutes
              <ArrowRight className="h-4 w-4" />
            </Link>
            <Link
              href="https://github.com/prassanna-ravishankar/repowire"
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center justify-center gap-2 rounded border border-border bg-surface-container-low px-5 py-3 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-surface transition-colors hover:bg-surface-container-high focus:outline-none focus:ring-2 focus:ring-primary-fixed focus:ring-offset-2 focus:ring-offset-surface"
            >
              <Github className="h-4 w-4" />
              View source
            </Link>
          </div>

          <dl className="mt-10 grid max-w-3xl gap-3 font-mono text-xs text-outline sm:grid-cols-3">
            <Metric label="Default" value="local daemon" />
            <Metric label="Messages" value="ask / notify / schedule" />
            <Metric label="Surfaces" value="dashboard / Telegram / Slack" />
          </dl>
        </motion.div>

        <motion.div
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.45, delay: 0.08 }}
        >
          <div className="overflow-hidden rounded-lg border border-border-strong bg-surface-container-low shadow-[var(--shadow-3)]">
            <div className="flex items-center justify-between border-b border-border-faint bg-surface-container px-4 py-3">
              <div className="flex items-center gap-2" aria-hidden="true">
                <span className="h-2.5 w-2.5 rounded-full bg-error" />
                <span className="h-2.5 w-2.5 rounded-full bg-tertiary" />
                <span className="h-2.5 w-2.5 rounded-full bg-secondary" />
              </div>
              <div className="font-mono text-[10px] uppercase tracking-[0.16em] text-outline">dashboard</div>
            </div>
            <Image
              src="/screenshots/dashboard-peer-overview.png"
              alt="Repowire dashboard showing active peers and their recent work"
              width={1440}
              height={986}
              className="h-auto max-h-[360px] w-full object-cover object-top lg:max-h-[390px]"
              priority
            />
            <div className="grid border-t border-border-faint bg-surface/95 p-4 text-sm leading-6 text-on-surface-variant sm:grid-cols-[1fr_auto] sm:items-center sm:gap-4">
              <p>
                See peers, status, chat turns, tool calls, and routed messages from the same browser surface that can reach your local daemon or optional relay.
              </p>
              <div className="mt-3 inline-flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.16em] text-primary-fixed sm:mt-0">
                <Terminal className="h-3.5 w-3.5" /> real product surface
              </div>
            </div>
          </div>
        </motion.div>
      </div>
    </section>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="border-l-2 border-primary/50 bg-surface-container-low/80 p-3">
      <dt className="mb-1 font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-outline">{label}</dt>
      <dd className="text-on-surface-variant">{value}</dd>
    </div>
  );
}
