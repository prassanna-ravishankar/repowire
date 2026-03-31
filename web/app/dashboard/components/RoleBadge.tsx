import { cn, roleBadgeClass } from "../lib/utils";

export function RoleBadge({ role }: { role?: string }) {
  if (!role || role === "agent") return null;
  const badge = roleBadgeClass(role);
  if (!badge) return null;
  return (
    <span className={cn("font-mono text-[10px] px-2 py-1 uppercase", badge)}>
      {role}
    </span>
  );
}
