export function WireTrace({ active }: { active: boolean }) {
  return (
    <div className="pointer-events-none absolute -left-[1px] top-0 hidden h-full w-[1px] bg-primary/45 md:block">
      {active && <div className="absolute left-[-2px] top-0 h-1.5 w-1.5 animate-pulse rounded-full bg-primary shadow-[var(--glow-copper)]" />}
    </div>
  );
}
