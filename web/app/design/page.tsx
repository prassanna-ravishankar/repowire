"use client";

import { useEffect, useState, type ReactNode } from "react";
import { Inbox, Plus, RefreshCw, Trash2 } from "lucide-react";
import {
  Banner,
  Brand,
  Button,
  CopyId,
  Dot,
  EmptyState,
  IconButton,
  InlineError,
  Modal,
  SkeletonRows,
  StatusBadge,
  StatusText,
  ThemeToggle,
  Toasts,
  inputClass,
  labelClass,
  useToasts,
  type Tone,
} from "../ui";

// A living reference for the product design system: every token and component
// as the browser renders it, in the current theme. Contrast figures are
// measured here from the computed colours, so a screenshot of this page is a
// review of the tokens. Not linked from the product; not indexed.

const SURFACES = [
  ["page", "--page"],
  ["sunken", "--surface-sunken"],
  ["surface", "--surface"],
  ["raised", "--surface-raised"],
  ["overlay", "--surface-overlay"],
] as const;
const TEXT = ["fg", "fg-muted", "fg-subtle", "accent", "success", "warning", "danger", "info"] as const;
const TONES: Tone[] = ["success", "warning", "danger", "accent", "neutral"];

export default function DesignPage() {
  const { toasts, push } = useToasts();
  const [modal, setModal] = useState(false);
  return (
    <div className="min-h-dvh bg-page text-fg">
      <header className="sticky top-0 z-10 flex items-center justify-between border-b border-border-faint bg-page/90 px-4 py-2 backdrop-blur md:px-8">
        <Brand suffix="DESIGN" />
        <ThemeToggle />
      </header>
      <main className="mx-auto max-w-5xl space-y-12 px-4 py-8 md:px-8 md:py-12">
        <div className="max-w-2xl">
          <h1 className="text-2xl font-semibold tracking-tight">Design system</h1>
          <p className="mt-2 text-sm leading-6 text-fg-muted">
            The tokens and components every product surface shares: the dashboard, managed agents, admin and the account portal. Switch the theme
            to check both. Contrast is measured live; every text role must read at 4.5:1 or better on every surface.
          </p>
        </div>

        <Section title="Text on surfaces">
          <ContrastTable />
        </Section>

        <Section title="Status">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
            {TONES.map((tone) => (
              <div key={tone} className="flex flex-col gap-2 rounded border border-border-faint bg-surface p-3">
                <span className="flex items-center gap-2">
                  <Dot tone={tone} live={tone === "success"} />
                  <StatusText tone={tone}>{tone}</StatusText>
                </span>
                <StatusBadge tone={tone}>{tone === "success" ? "live" : tone}</StatusBadge>
              </div>
            ))}
          </div>
        </Section>

        <Section title="Controls">
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="primary" onClick={() => push("Saved")}>
              <Plus aria-hidden className="h-3.5 w-3.5" /> New agent
            </Button>
            <Button onClick={() => push("Something failed", "error")}>Show an error toast</Button>
            <Button variant="ghost">Ghost</Button>
            <Button variant="danger" onClick={() => setModal(true)}>
              <Trash2 aria-hidden className="h-3.5 w-3.5" /> Retire
            </Button>
            <Button disabled>Disabled</Button>
            <IconButton label="Refresh">
              <RefreshCw aria-hidden className="h-3.5 w-3.5" />
            </IconButton>
          </div>
          <div className="mt-6 grid max-w-xl gap-4">
            <label className="grid gap-1.5">
              <span className={labelClass}>Agent name</span>
              <input className={inputClass} placeholder="reviewer" />
            </label>
            <label className="grid gap-1.5">
              <span className={labelClass}>Message</span>
              <textarea className={inputClass} rows={3} placeholder="Ask the agent something" />
            </label>
            <p className="text-xs text-fg-subtle">
              Peer ids copy on click: <CopyId value="repow-default-7f3a" onCopied={() => push("Copied")} className="text-fg-muted" />
            </p>
          </div>
        </Section>

        <Section title="Feedback">
          <div className="grid gap-4 lg:grid-cols-2">
            <div className="space-y-3">
              <InlineError error="relay returned 502: the machine did not respond in time" onRetry={() => push("Retrying")} />
              <div className="overflow-hidden rounded border border-border-faint">
                <Banner tone="warning">The relay lost its link to the agent runtime. Showing the last known state.</Banner>
                <Banner tone="danger">Agent retired. It no longer takes input.</Banner>
              </div>
              <div className="overflow-hidden rounded border border-border-faint bg-surface">
                <SkeletonRows rows={2} />
              </div>
            </div>
            <div className="rounded border border-border-faint bg-surface">
              <EmptyState icon={<Inbox className="h-5 w-5" />} title="Inbox empty">
                Messages and asks agents send you land here.
              </EmptyState>
            </div>
          </div>
        </Section>
      </main>
      {modal && (
        <Modal
          title="Retire agent"
          onClose={() => setModal(false)}
          footer={
            <>
              <Button onClick={() => setModal(false)}>Cancel</Button>
              <Button variant="danger" onClick={() => setModal(false)}>
                Retire
              </Button>
            </>
          }
        >
          <p className="text-sm leading-6 text-fg-muted">The agent stops taking input. Its transcript stays readable.</p>
        </Modal>
      )}
      <Toasts toasts={toasts} />
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section>
      <h2 className="mb-4 text-sm font-semibold">{title}</h2>
      {children}
    </section>
  );
}

// Measured from the computed colours, so it reflects what is on screen.
function ContrastTable() {
  const [ratios, setRatios] = useState<Record<string, number> | null>(null);
  useEffect(() => {
    const measure = () => {
      const probe = document.createElement("div");
      document.body.appendChild(probe);
      const rgb = (cssVar: string) => {
        probe.style.color = `var(${cssVar})`;
        return getComputedStyle(probe).color;
      };
      const out: Record<string, number> = {};
      for (const [, surface] of SURFACES) {
        const ground = parse(rgb(surface), parse(rgb("--page")));
        for (const text of TEXT) out[`${text}${surface}`] = contrast(parse(rgb(`--${text}`), ground), ground);
      }
      probe.remove();
      setRatios(out);
    };
    const frame = requestAnimationFrame(measure);
    const observer = new MutationObserver(measure);
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
    };
  }, []);
  return (
    <div className="overflow-x-auto rounded border border-border-faint">
      <table className="w-full min-w-[640px] border-collapse font-mono text-[11px]">
        <thead>
          <tr>
            <th className="bg-page p-2 text-left font-semibold text-fg-subtle">text</th>
            {SURFACES.map(([name, cssVar]) => (
              <th key={name} className="p-2 text-left font-semibold text-fg-muted" style={{ background: `var(${cssVar})` }}>
                {name}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {TEXT.map((text) => (
            <tr key={text} className="border-t border-border-faint">
              <td className="bg-page p-2 text-fg-subtle">{text}</td>
              {SURFACES.map(([name, cssVar]) => {
                const ratio = ratios?.[`${text}${cssVar}`];
                return (
                  <td key={name} className="p-2" style={{ background: `var(${cssVar})`, color: `var(--${text})` }}>
                    Agent online {ratio ? <span className={ratio >= 4.5 ? "" : "font-bold underline"}>{ratio.toFixed(1)}</span> : "…"}
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

type RGB = [number, number, number];
function parse(css: string, base: RGB = [255, 255, 255]): RGB {
  const parts = css.match(/[\d.]+/g)?.map(Number) ?? [0, 0, 0];
  const [r, g, b, a = 1] = parts;
  return [r * a + base[0] * (1 - a), g * a + base[1] * (1 - a), b * a + base[2] * (1 - a)];
}
function contrast(a: RGB, b: RGB) {
  const lum = ([r, g, bl]: RGB) => {
    const f = (c: number) => (c / 255 <= 0.03928 ? c / 255 / 12.92 : ((c / 255 + 0.055) / 1.055) ** 2.4);
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(bl);
  };
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}
