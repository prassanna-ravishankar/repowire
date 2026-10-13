import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { BriefcaseBusiness, Play, RefreshCw, RotateCcw, XCircle } from "lucide-react";
import { cn, shortPath, timeAgo } from "../lib/utils";
import type { JobStatus, JobsResponse, RecurringJobStatus } from "../types";
import { EmptyState } from "./EmptyState";

type JobItem =
  | { type: "work"; id: string; status: JobStatus }
  | { type: "recurring"; id: string; status: RecurringJobStatus };
type JobFilter = "all" | "attention" | "active" | "recurring" | "closed";

const terminalStates = new Set(["completed", "failed", "cancelled", "expired", "unavailable"]);
const retryableTerminalStates = new Set(["failed", "unavailable"]);
const jobFilters: { id: JobFilter; label: string }[] = [
  { id: "all", label: "All" },
  { id: "attention", label: "Attention" },
  { id: "active", label: "Active" },
  { id: "recurring", label: "Recurring" },
  { id: "closed", label: "Closed" },
];

function itemTitle(item: JobItem): string {
  return item.status.title || item.id;
}

function itemState(item: JobItem): string {
  return item.status.state || "unknown";
}

function itemUpdatedAt(item: JobItem): string | undefined {
  return item.status.updated_at || item.status.created_at;
}

function stateTone(state: string): string {
  if (state === "completed") return "border-secondary/25 bg-secondary/10 text-secondary";
  if (state === "running" || state === "delivered") return "border-tertiary-fixed-dim/30 bg-tertiary-fixed-dim/10 text-tertiary-fixed-dim";
  if (state === "failed" || state === "cancelled" || state === "expired" || state === "unavailable") return "border-error/25 bg-error/10 text-error";
  if (state === "active") return "border-primary/30 bg-primary/10 text-primary-fixed";
  return "border-border-faint bg-surface-container-low text-outline";
}

function formatDate(value?: string | null): string {
  if (!value) return "-";
  return new Date(value).toLocaleString([], {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function canRun(item: JobItem): boolean {
  return item.type === "work" && ["queued", "failed", "unavailable"].includes(itemState(item));
}

function canRetry(item: JobItem): boolean {
  return item.type === "work" && ["failed", "unavailable", "delivered"].includes(itemState(item));
}

function canCancel(item: JobItem): boolean {
  if (item.type === "recurring") return itemState(item) !== "cancelled";
  return !terminalStates.has(itemState(item));
}

function isActiveItem(item: JobItem): boolean {
  const state = itemState(item);
  if (item.type === "recurring") return state !== "cancelled";
  return !terminalStates.has(state);
}

function isAttentionItem(item: JobItem): boolean {
  return item.type === "work" && retryableTerminalStates.has(itemState(item));
}

function matchesFilter(item: JobItem, filter: JobFilter): boolean {
  if (filter === "all") return true;
  if (filter === "attention") return isAttentionItem(item);
  if (filter === "active") return isActiveItem(item);
  if (filter === "recurring") return item.type === "recurring";
  return !isActiveItem(item) && !isAttentionItem(item);
}

export function JobsView({
  apiBase,
  refreshSignal,
}: {
  apiBase: string;
  refreshSignal: number;
}) {
  const [jobs, setJobs] = useState<JobsResponse>({ work: [], recurring: [] });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [filter, setFilter] = useState<JobFilter>("all");
  const [detailById, setDetailById] = useState<Record<string, JobStatus | RecurringJobStatus>>({});
  const [detailLoadingId, setDetailLoadingId] = useState<string | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [busyAction, setBusyAction] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const fetchJobs = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`${apiBase}/jobs?view=summary`);
      if (!res.ok) {
        setError(`Error ${res.status}`);
        return;
      }
      const data = (await res.json()) as JobsResponse;
      setJobs({ work: data.work || [], recurring: data.recurring || [] });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load jobs");
    } finally {
      setLoading(false);
    }
  }, [apiBase]);

  useEffect(() => {
    void fetchJobs();
  }, [fetchJobs, refreshSignal]);

  const items = useMemo<JobItem[]>(() => {
    const workItems = jobs.work.map((status): JobItem => ({
      type: "work",
      id: status.job_id || status.work_id || "",
      status,
    })).filter((item) => item.id);
    const recurringItems = jobs.recurring.map((status): JobItem => ({
      type: "recurring",
      id: status.calendar_id,
      status,
    })).filter((item) => item.id);
    return [...workItems, ...recurringItems].sort((a, b) => {
      const aTime = itemUpdatedAt(a) || "";
      const bTime = itemUpdatedAt(b) || "";
      return bTime.localeCompare(aTime);
    });
  }, [jobs]);

  const activeCount = items.filter(isActiveItem).length;
  const attentionCount = items.filter(isAttentionItem).length;
  const closedCount = items.length - activeCount - attentionCount;
  const filteredItems = useMemo(
    () => items.filter((item) => matchesFilter(item, filter)),
    [filter, items],
  );
  const activeItems = filteredItems.filter(isActiveItem);
  const attentionItems = filteredItems.filter(isAttentionItem);
  const closedItems = filteredItems.filter((item) => !isActiveItem(item) && !isAttentionItem(item));
  const selected = filteredItems.find((item) => item.id === selectedId) ?? activeItems[0] ?? attentionItems[0] ?? closedItems[0] ?? null;
  const selectedDetailId = selected?.id ?? null;
  const selectedDetail = selected ? detailById[selected.id] : null;
  const displaySelected: JobItem | null = selected && selectedDetail
    ? selected.type === "work"
      ? { ...selected, status: selectedDetail as JobStatus }
      : { ...selected, status: selectedDetail as RecurringJobStatus }
    : selected;
  const selectedHasDetail = selected ? Boolean(selectedDetail) : false;

  useEffect(() => {
    const nextId = filteredItems[0]?.id ?? null;
    if (selectedId && filteredItems.some((item) => item.id === selectedId)) {
      return;
    }
    if (selectedId !== nextId) {
      setSelectedId(nextId);
      setActionError(null);
      setDetailError(null);
    }
  }, [filteredItems, selectedId]);

  const selectJob = useCallback((id: string) => {
    setSelectedId(id);
    setActionError(null);
    setDetailError(null);
  }, []);

  useEffect(() => {
    if (!selectedDetailId) return;
    const id = selectedDetailId;
    const controller = new AbortController();
    setDetailLoadingId(id);
    setDetailError(null);
    fetch(`${apiBase}/jobs/${encodeURIComponent(id)}/status`, { signal: controller.signal })
      .then((res) => {
        if (!res.ok) throw new Error(`Error ${res.status}`);
        return res.json();
      })
      .then((data: { status?: JobStatus | RecurringJobStatus }) => {
        if (!data.status) throw new Error("Missing job status");
        setDetailById((current) => ({ ...current, [id]: data.status! }));
      })
      .catch((err) => {
        if (err instanceof Error && err.name === "AbortError") return;
        setDetailError(err instanceof Error ? err.message : "Failed to load job detail");
      })
      .finally(() => {
        if (!controller.signal.aborted) {
          setDetailLoadingId((current) => current === id ? null : current);
        }
      });
    return () => controller.abort();
  }, [apiBase, selectedDetailId, refreshSignal]);

  const runAction = async (item: JobItem, action: "run" | "retry" | "cancel") => {
    setActionError(null);
    setBusyAction(`${item.id}:${action}`);
    try {
      const res = await fetch(`${apiBase}/jobs/${encodeURIComponent(item.id)}/${action}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: action === "cancel" ? JSON.stringify({ reason: "dashboard" }) : JSON.stringify({}),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        setActionError(typeof data?.detail === "string" ? data.detail : `Error ${res.status}`);
        return;
      }
      const data = await res.json().catch(() => null);
      if (data?.status) {
        setDetailById((current) => ({ ...current, [item.id]: data.status }));
      }
      await fetchJobs();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Action failed");
    } finally {
      setBusyAction(null);
    }
  };

  return (
    <>
      <div className="sticky top-[var(--topbar-offset)] z-10 flex flex-wrap items-center justify-between gap-3 border-b border-border-faint bg-surface-dim px-4 py-3 md:static md:px-6">
        <div>
          <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.22em] text-primary">JOBS / durable work</div>
          <h1 className="mt-1 font-headline text-2xl font-semibold text-on-surface">tracked work</h1>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <JobMetric label="active" value={activeCount} />
          <JobMetric label="attention" value={attentionCount} />
          <JobMetric label="closed" value={closedCount} />
          <button
            onClick={() => void fetchJobs()}
            aria-label="Refresh jobs"
            className="inline-flex h-8 w-8 items-center justify-center rounded border border-border text-outline transition-colors hover:bg-surface-container-high hover:text-on-surface"
          >
            <RefreshCw className={cn("h-3.5 w-3.5", loading && "animate-spin")} />
          </button>
        </div>
      </div>

      <div className="grid min-h-0 flex-1 bg-surface-dim md:grid-cols-[minmax(300px,420px)_1fr] md:overflow-hidden">
        <div className="min-h-0 border-r border-border-faint md:overflow-y-auto">
          {loading && items.length === 0 ? (
            <EmptyState icon={<RefreshCw className="h-5 w-5 animate-spin" />} title="loading jobs" detail="reading durable work from daemon" />
          ) : error ? (
            <EmptyState icon={<XCircle className="h-5 w-5" />} title="jobs unavailable" detail={error} />
          ) : items.length === 0 ? (
            <EmptyState icon={<BriefcaseBusiness className="h-5 w-5" />} title="no jobs yet" detail="create one with repowire jobs create or an MCP job_create tool call" />
          ) : (
            <>
              <JobFilterBar filter={filter} onChange={setFilter} />
              {filteredItems.length === 0 && (
                <EmptyState icon={<BriefcaseBusiness className="h-5 w-5" />} title="no matching jobs" detail="change the jobs filter to see more tracked work" />
              )}
              <JobGroup title="active" items={activeItems} selectedId={selected?.id ?? null} onSelect={selectJob} />
              <JobGroup title="needs attention" items={attentionItems} selectedId={selected?.id ?? null} onSelect={selectJob} />
              <JobGroup title="closed" items={closedItems} selectedId={selected?.id ?? null} onSelect={selectJob} />
            </>
          )}
        </div>

        <div className="min-h-0 md:overflow-y-auto">
          {displaySelected ? (
            <JobDetail
              item={displaySelected}
              busyAction={busyAction}
              actionError={actionError}
              detailLoading={detailLoadingId === displaySelected.id}
              detailError={detailError}
              hasDetail={selectedHasDetail}
              onRun={() => void runAction(displaySelected, "run")}
              onRetry={() => void runAction(displaySelected, "retry")}
              onCancel={() => void runAction(displaySelected, "cancel")}
            />
          ) : (
            <EmptyState icon={<BriefcaseBusiness className="h-5 w-5" />} title="select a job" detail="job detail and actions appear here" />
          )}
        </div>
      </div>
    </>
  );
}

function JobFilterBar({ filter, onChange }: { filter: JobFilter; onChange: (filter: JobFilter) => void }) {
  return (
    <div className="flex flex-wrap gap-1 border-b border-border-faint px-3 py-2">
      {jobFilters.map((option) => (
        <button
          key={option.id}
          type="button"
          onClick={() => onChange(option.id)}
          aria-pressed={filter === option.id}
          className={cn(
            "h-7 rounded border px-2.5 font-mono text-[10px] font-semibold uppercase tracking-[0.12em] transition-colors",
            filter === option.id
              ? "border-primary bg-primary/10 text-primary-fixed"
              : "border-border-faint text-outline hover:bg-surface-container-high hover:text-on-surface",
          )}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

function JobMetric({ label, value }: { label: string; value: number }) {
  return (
    <div className="border border-border-faint bg-surface-container-low px-2.5 py-1 text-right font-mono">
      <div className="text-sm font-bold tabular-nums text-primary-fixed">{value}</div>
      <div className="text-[9px] uppercase tracking-[0.16em] text-outline">{label}</div>
    </div>
  );
}

function JobGroup({
  title,
  items,
  selectedId,
  onSelect,
}: {
  title: string;
  items: JobItem[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  if (items.length === 0) return null;
  return (
    <section>
      <div className="flex items-baseline justify-between px-3.5 pb-1.5 pt-3 font-mono text-[9px] font-semibold uppercase tracking-[0.2em] text-outline">
        <span>{title}</span>
        <span>{items.length}</span>
      </div>
      {items.map((item) => (
        <JobRow
          key={item.id}
          item={item}
          active={item.id === selectedId}
          onClick={() => onSelect(item.id)}
        />
      ))}
    </section>
  );
}

function JobRow({ item, active, onClick }: { item: JobItem; active: boolean; onClick: () => void }) {
  const state = itemState(item);
  const targetPath = item.status.execution?.target?.path;
  const path = targetPath ? shortPath(targetPath) : null;
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "block w-full border-b border-border-faint border-l-2 px-3 py-2.5 text-left transition-colors",
        active ? "border-l-primary bg-primary/10" : "border-l-transparent hover:bg-surface-container",
      )}
    >
      <div className="mb-1 flex min-w-0 items-center gap-2">
        <span className={cn("shrink-0 border px-1.5 py-0.5 font-mono text-[9px] uppercase tracking-[0.12em]", stateTone(state))}>
          {state}
        </span>
        <span className="min-w-0 flex-1 truncate font-mono text-[13px] font-semibold text-on-surface">{itemTitle(item)}</span>
      </div>
      <div className="truncate font-mono text-[11px] leading-5 text-outline">
        {item.type === "recurring" ? `cron ${item.status.cron || "-"}` : item.status.kind || "general"} · {item.status.circle || "default"}
      </div>
      {path && (
        <div className="truncate font-mono text-[11px] leading-5 text-outline">
          {path.parent}<span className="text-on-surface-variant">{path.folder}</span>
        </div>
      )}
      <div className="font-mono text-[11px] leading-5 text-outline">
        updated {timeAgo(itemUpdatedAt(item)) || "-"}
      </div>
    </button>
  );
}

function JobDetail({
  item,
  busyAction,
  actionError,
  detailLoading,
  detailError,
  hasDetail,
  onRun,
  onRetry,
  onCancel,
}: {
  item: JobItem;
  busyAction: string | null;
  actionError: string | null;
  detailLoading: boolean;
  detailError: string | null;
  hasDetail: boolean;
  onRun: () => void;
  onRetry: () => void;
  onCancel: () => void;
}) {
  const state = itemState(item);
  const execution = item.status.execution || {};
  const target = execution.target || {};
  const prompt = execution.prompt || {};
  const progressEvents = item.type === "work" ? item.status.progress_events || [] : [];
  const stateReason = item.type === "work" ? item.status.state_reason : null;
  const cancellationReason = item.type === "work" ? item.status.cancellation_reason : null;
  return (
    <div className="space-y-4 px-4 py-4 md:px-6">
      <section className="border border-border-faint bg-surface-container-low p-4">
        <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-outline">
              {item.type === "recurring" ? "recurring" : "job"} / {item.id}
            </p>
            <h2 className="break-words font-headline text-xl font-semibold text-on-surface">{itemTitle(item)}</h2>
          </div>
          <span className={cn("shrink-0 border px-2 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.14em]", stateTone(state))}>
            {state}
          </span>
        </div>

        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <DetailMetric label="kind" value={item.status.kind || "general"} />
          <DetailMetric label="circle" value={item.status.circle || "default"} />
          <DetailMetric label={item.type === "recurring" ? "next due" : "due"} value={formatDate(item.type === "recurring" ? item.status.next_due_at : item.status.due_at)} />
          <DetailMetric label="updated" value={formatDate(itemUpdatedAt(item))} />
        </div>

        {item.type === "work" && item.status.result_summary && (
          <div className="mt-4 border-l-2 border-secondary bg-secondary/10 p-3 text-sm text-on-surface">
            {item.status.result_summary}
          </div>
        )}
        {actionError && <p className="mt-3 font-mono text-xs text-error">{actionError}</p>}
        {detailLoading && <p className="mt-3 font-mono text-xs text-outline">loading detail</p>}
        {detailError && !hasDetail && (
          <div className="mt-4 border border-error/25 bg-error/10 p-3">
            <p className="font-mono text-xs font-semibold uppercase tracking-[0.14em] text-error">detail unavailable</p>
            <p className="mt-1 font-mono text-xs leading-5 text-on-surface-variant">
              {detailError}. The list is still loaded from the jobs summary view; refresh after checking the daemon or relay tunnel.
            </p>
          </div>
        )}
        {detailError && hasDetail && <p className="mt-3 font-mono text-xs text-error">{detailError}</p>}

        <div className="mt-4 flex flex-wrap gap-2">
          {canRun(item) && (
            <ActionButton icon={<Play className="h-3.5 w-3.5" />} label="Run now" busy={busyAction === `${item.id}:run`} onClick={onRun} />
          )}
          {canRetry(item) && (
            <ActionButton icon={<RotateCcw className="h-3.5 w-3.5" />} label="Retry" busy={busyAction === `${item.id}:retry`} onClick={onRetry} />
          )}
          {canCancel(item) && (
            <ActionButton danger icon={<XCircle className="h-3.5 w-3.5" />} label={item.type === "recurring" ? "Cancel schedule" : "Cancel"} busy={busyAction === `${item.id}:cancel`} onClick={onCancel} />
          )}
        </div>
      </section>

      <section className="grid gap-4 lg:grid-cols-3">
        <InfoPanel title="target">
          <InfoLine label="path" value={target.path || "-"} />
          <InfoLine label="backend" value={target.backend || "-"} />
          <InfoLine label="profile" value={target.profile || "default"} />
          <InfoLine label="assigned" value={target.assigned_peer_id || item.status.assigned_peer_id || "-"} />
        </InfoPanel>
        <InfoPanel title="schedule">
          <InfoLine label="cron" value={item.type === "recurring" ? item.status.cron || "-" : "-"} />
          <InfoLine label="next" value={item.type === "recurring" ? formatDate(item.status.next_due_at) : "-"} />
          <InfoLine label="last job" value={item.type === "recurring" ? item.status.last_occurrence_work_id || "-" : "-"} />
          <InfoLine label="delivery" value={execution.delivery?.kind || "-"} />
        </InfoPanel>
        <InfoPanel title="state">
          <InfoLine label="reason" value={stateReason || "-"} />
          <InfoLine label="cancel" value={cancellationReason || "-"} />
          <InfoLine label="owner" value={item.status.owner_peer_id || "-"} />
          <InfoLine label="creator" value={item.status.created_by_peer_id || "-"} />
        </InfoPanel>
      </section>

      <InfoPanel title="prompt">
        <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words font-mono text-xs leading-5 text-on-surface-variant">
          {prompt.body || itemTitle(item)}
        </pre>
      </InfoPanel>

      {progressEvents.length > 0 && (
        <InfoPanel title="progress">
          <div className="space-y-2">
            {progressEvents.slice(-6).map((event, index) => (
              <div key={index} className="border-l border-border-strong pl-3 font-mono text-xs leading-5">
                <div className="text-outline">{typeof (event.at ?? event.timestamp) === "string" ? formatDate(event.at ?? event.timestamp) : "event"}</div>
                <div className="text-on-surface-variant">{event.note || JSON.stringify(event)}</div>
              </div>
            ))}
          </div>
        </InfoPanel>
      )}
    </div>
  );
}

function DetailMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="border-l-2 border-primary/60 bg-surface-container-lowest p-3">
      <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.14em] text-outline">{label}</p>
      <p className="break-words font-mono text-sm text-primary-fixed">{value}</p>
    </div>
  );
}

function InfoPanel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="border border-border-faint bg-surface-container-low p-4">
      <h3 className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">{title}</h3>
      {children}
    </section>
  );
}

function InfoLine({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid grid-cols-[90px_1fr] gap-3 border-b border-border-faint py-1.5 font-mono text-xs last:border-b-0">
      <span className="text-outline">{label}</span>
      <span className="min-w-0 break-words text-on-surface-variant">{value}</span>
    </div>
  );
}

function ActionButton({
  label,
  icon,
  busy,
  danger,
  onClick,
}: {
  label: string;
  icon: ReactNode;
  busy: boolean;
  danger?: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      disabled={busy}
      className={cn(
        "inline-flex h-8 items-center gap-2 rounded border px-3 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors disabled:opacity-50",
        danger
          ? "border-error/35 bg-error/10 text-error hover:bg-error/20"
          : "border-border text-outline hover:bg-surface-container-high hover:text-on-surface",
      )}
    >
      {busy ? <RefreshCw className="h-3.5 w-3.5 animate-spin" /> : icon}
      {label}
    </button>
  );
}
