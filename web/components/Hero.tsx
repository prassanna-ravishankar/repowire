"use client";

import { ArrowRight, Terminal } from "lucide-react";
import Image from "next/image";
import Link from "next/link";
import { motion } from "framer-motion";

export default function Hero() {
  return (
    <section className="relative overflow-hidden border-b border-border-faint pt-24 sm:min-h-[92svh] sm:pt-28">
      <Image
        src="/brand/repowire-arch.webp"
        alt=""
        width={1280}
        height={720}
        priority
        className="pointer-events-none absolute bottom-0 right-0 w-[920px] max-w-none opacity-35 mix-blend-screen"
      />
      <div className="absolute inset-0 bg-surface/85" />

      <div className="relative z-10 mx-auto flex max-w-7xl flex-col justify-center px-4 pb-14 sm:min-h-[calc(92svh-7rem)] sm:px-6 sm:pb-20 lg:px-8">
        <motion.div
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.4 }}
          className="max-w-3xl"
        >
          <div className="mb-6 inline-flex items-center border border-primary/30 bg-primary/10 px-3 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-primary-fixed">
            Public beta available
          </div>
          <h1 className="font-headline text-4xl font-bold leading-tight tracking-normal text-on-surface sm:text-5xl lg:text-6xl">
            Mesh network for AI coding agents
          </h1>
          <p className="mt-5 max-w-2xl text-lg leading-8 text-on-surface-variant">
            Stop the copy-paste dance. Repowire connects active coding agents into a local mesh so they can ask, notify, and broadcast across repositories.
          </p>
          <div className="mt-8 flex flex-col gap-3 sm:flex-row">
            <Link
              href="#installation"
              className="inline-flex items-center justify-center gap-2 rounded bg-primary px-5 py-3 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98]"
            >
              Get started
              <ArrowRight className="h-4 w-4" />
            </Link>
            <Link
              href="https://github.com/prassanna-ravishankar/repowire"
              target="_blank"
              className="inline-flex items-center justify-center gap-2 rounded border border-border bg-surface-container-low px-5 py-3 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-surface transition-colors hover:bg-surface-container-high"
            >
              <Terminal className="h-4 w-4" />
              View on GitHub
            </Link>
          </div>
        </motion.div>

        <div className="mt-14 grid max-w-4xl gap-2 font-mono text-xs text-outline sm:grid-cols-3">
          <Metric label="transports" value="hooks / channel / relay" />
          <Metric label="runtime" value="Claude Code / Codex / Gemini" />
          <Metric label="default" value="local-first daemon" />
        </div>
      </div>
    </section>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="border-l-2 border-primary/50 bg-surface-container-low/80 p-3">
      <div className="mb-1 font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-outline">{label}</div>
      <div className="truncate text-on-surface-variant">{value}</div>
    </div>
  );
}
