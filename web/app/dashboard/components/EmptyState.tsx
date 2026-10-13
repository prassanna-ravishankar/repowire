import type { ReactNode } from "react";

export function EmptyState({ icon, title, detail }: { icon: ReactNode; title: string; detail: string }) {
  return (
    <div className="flex min-h-64 flex-col items-center justify-center px-6 py-12 text-center font-mono">
      <div className="mb-3 text-outline">{icon}</div>
      <div className="text-xs font-semibold uppercase tracking-[0.18em] text-on-surface-variant">{title}</div>
      <div className="mt-2 max-w-sm text-xs leading-5 text-outline">{detail}</div>
    </div>
  );
}
