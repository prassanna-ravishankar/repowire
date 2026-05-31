import { useCallback, useEffect, useState } from "react";
import { ListChecks, RefreshCw } from "lucide-react";
import { cn } from "../lib/utils";

interface BeadsRow {
  id: string;
  title: string;
  status: string;
  priority: number | null;
  issue_type: string | null;
  assignee: string | null;
}

interface BeadsGroup {
  items: BeadsRow[];
  total: number;
  truncated: boolean;
}

interface BeadsBoardData {
  available: boolean;
  ready: BeadsGroup;
  in_progress: BeadsGroup;
  blocked: BeadsGroup;
  recently_closed: BeadsGroup;
}

const COLUMNS: { key: keyof Omit<BeadsBoardData, "available">; label: string; tone: string }[] = [
  { key: "ready", label: "Ready", tone: "text-primary-fixed" },
  { key: "in_progress", label: "In progress", tone: "text-tertiary-fixed-dim" },
  { key: "blocked", label: "Blocked", tone: "text-error" },
  { key: "recently_closed", label: "Recently closed", tone: "text-secondary" },
];

/** Compact assignee label: localpart for emails, else the raw value. */
function assigneeLabel(assignee: string | null): string {
  if (!assignee) return "Unassigned";
  return assignee.includes("@") ? assignee.split("@")[0] : assignee;
}

export function BeadsBoard({ apiBase }: { apiBase: string }) {
  const [data, setData] = useState<BeadsBoardData | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(() => {
    setLoading(true);
    fetch(`${apiBase}/beads/board`)
      .then((res) => (res.ok ? res.json() : null))
      .then((d: BeadsBoardData | null) => setData(d))
      .catch(() => setData(null))
      .finally(() => setLoading(false));
  }, [apiBase]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  if (loading && data === null) {
    return (
      <div className="flex items-center justify-center py-12 font-mono text-sm text-outline">
        <RefreshCw className="mr-2 h-4 w-4 animate-spin" /> Loading Beads board...
      </div>
    );
  }

  if (data === null || !data.available) {
    return (
      <div className="flex flex-col items-center justify-center gap-2 py-12 font-mono text-sm text-outline">
        <ListChecks className="h-5 w-5" />
        <p>Beads board unavailable.</p>
        <p className="text-[11px]">No Beads workspace for this repo, or the `bd` CLI isn&apos;t installed.</p>
      </div>
    );
  }

  return (
    <div className="p-3 md:p-4" data-testid="beads-board">
      <div className="mb-3 flex items-center justify-between">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-outline">
          Beads board · read-only
        </span>
        <button
          onClick={refresh}
          aria-label="Refresh Beads board"
          className="text-outline transition-colors hover:text-on-surface"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", loading && "animate-spin")} />
        </button>
      </div>
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
        {COLUMNS.map((col) => {
          const group = data[col.key];
          return (
            <div key={col.key} className="rounded border border-border-faint bg-surface-dim">
              <div className="flex items-center justify-between border-b border-border-faint px-2.5 py-1.5">
                <span className={cn("font-mono text-[11px] font-bold uppercase tracking-[0.1em]", col.tone)}>
                  {col.label}
                </span>
                <span className="font-mono text-[10px] text-outline">
                  {group.total}
                  {group.truncated ? "+" : ""}
                </span>
              </div>
              <div className="divide-y divide-border-faint">
                {group.items.length === 0 ? (
                  <p className="px-2.5 py-2 font-mono text-[11px] text-outline">none</p>
                ) : (
                  group.items.map((row) => (
                    <div key={row.id} className="px-2.5 py-1.5">
                      <div className="flex items-baseline gap-1.5 font-mono text-[11px]">
                        <span className="shrink-0 font-bold text-on-surface">{row.id}</span>
                        <span className="min-w-0 flex-1 truncate text-on-surface-variant">{row.title}</span>
                      </div>
                      <div className="mt-0.5 flex items-center gap-2 font-mono text-[10px] text-outline">
                        {row.issue_type && <span>{row.issue_type}</span>}
                        {row.priority != null && <span>P{row.priority}</span>}
                        <span className="truncate">{assigneeLabel(row.assignee)}</span>
                      </div>
                    </div>
                  ))
                )}
                {group.truncated && (
                  <p className="px-2.5 py-1 font-mono text-[10px] text-outline">
                    +{group.total - group.items.length} more
                  </p>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
