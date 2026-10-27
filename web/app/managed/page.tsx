"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Bot, Plus, Shield } from "lucide-react";
import { cn } from "../dashboard/lib/utils";
import { ThemeToggle } from "../dashboard/components/ThemeToggle";
import { AgentDetail } from "./components/AgentDetail";
import { AgentList } from "./components/AgentList";
import { ClientsView } from "./components/ClientsView";
import { CreateAgentDialog, RetireDialog } from "./components/Dialogs";
import { GateScreen } from "./components/Gate";
import { Banner, Brand, ConnectionBadge, EmptyState, InlineError, Toasts, primaryButtonClass, useToasts } from "./components/ui";
import { managedApi, type Agent, type Me, type OAuthApp } from "./lib/api";
import { deliver, draft, reconcile, retrying, type Outgoing } from "./lib/composer";
import { useHashRoute, type Route } from "./lib/hashRoute";
import { connectedDaemon, formatClock, visibleAgents } from "./lib/state";
import type { TranscriptItem } from "./lib/transcript";
import { useManagedLive } from "./lib/useManagedLive";
import { useMe } from "./lib/useMe";

export default function ManagedPage() {
  const [gate, retry] = useMe();
  if (gate.kind !== "ready") return <GateScreen gate={gate} onRetry={retry} />;
  return <ManagedApp me={gate.me} />;
}

/** Re-renders on a local clock so durations and "ago" labels stay current. Not a network poll. */
function useNow(fast: boolean) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), fast ? 1000 : 15_000);
    return () => clearInterval(timer);
  }, [fast]);
  return now;
}

function ManagedApp({ me }: { me: Me }) {
  const { live, dispatch, since, applyAgent, loaded, listError, clients, clientsError, loadClients, resync, connection, upstream } = useManagedLive();
  const [outgoing, setOutgoing] = useState<Outgoing[]>([]);
  const [showRetired, setShowRetired] = useState(false);
  const [dialog, setDialog] = useState<null | "create" | "retire">(null);
  const [route, navigate] = useHashRoute();
  const { toasts, push } = useToasts();

  const anyWorking = live.agents.some((a) => a.state === "working");
  const now = useNow(anyWorking);

  const selectedId = route.view === "agents" ? route.agent : null;
  const selected = live.agents.find((a) => a.id === selectedId) ?? null;
  const shown = useMemo(() => visibleAgents(live.agents, showRetired || selected?.state === "retired"), [live.agents, showRetired, selected?.state]);
  const retiredCount = live.agents.filter((a) => a.state === "retired").length;
  const daemon = connectedDaemon(live.clients);
  // Agents on managed run their mesh tools in the cloud; only a daemon's own agents need it connected.
  const needsDaemon = live.agents.some((a) => a.created_by === "daemon" && a.state !== "retired");
  const degraded = connection.status === "live" && !upstream.connected;
  const stale = connection.status === "offline" || degraded;

  const refreshAgent = useCallback(
    async (id: string) => {
      const token = since();
      try {
        applyAgent(await managedApi.agent(id), token);
      } catch {
        // The live stream will carry the change; a failed refresh is not an error to show.
      }
    },
    [since, applyAgent],
  );

  const send = (text: string) => {
    if (!selected) return;
    const message = draft(selected.id, text);
    setOutgoing((current) => [...current, message]);
    void deliver(message, managedApi.send).then((done) => setOutgoing((current) => current.map((m) => (m.operationId === done.operationId ? done : m))));
  };

  const retrySend = (message: Outgoing) => {
    const again = retrying(message);
    setOutgoing((current) => current.map((m) => (m.operationId === again.operationId ? again : m)));
    void deliver(again, managedApi.send).then((done) => setOutgoing((current) => current.map((m) => (m.operationId === done.operationId ? done : m))));
  };

  const onTranscript = useCallback((agentId: string, items: TranscriptItem[]) => {
    setOutgoing((current) => {
      const next = current.filter((m) => m.agent !== agentId || reconcile([m], items).length > 0);
      return next.length === current.length ? current : next;
    });
  }, []);

  const stop = async (agent: Agent) => {
    try {
      const { aborted } = await managedApi.abort(agent.id);
      push(aborted ? "Run stopped" : "Nothing was running");
      await refreshAgent(agent.id);
    } catch (err) {
      push((err as Error).message, "error");
    }
  };

  const revoke = async (app: OAuthApp) => {
    await managedApi.revokeApp(app.grant_id);
    push(`Revoked ${app.client_name ?? app.client_id}`);
    loadClients();
  };

  const go = (next: Route) => navigate(next);

  return (
    <div className="flex h-dvh flex-col bg-background text-on-surface">
      <header className="flex min-h-[calc(48px+env(safe-area-inset-top))] shrink-0 items-center gap-3 border-b border-border-faint bg-surface-dim px-3 pt-[env(safe-area-inset-top)] md:h-[52px] md:min-h-0 md:px-5 md:pt-0">
        <Brand suffix="MANAGED" />
        <nav className="ml-4 hidden overflow-hidden rounded border border-border md:flex">
          {(["agents", "clients"] as const).map((view, i) => (
            <button
              key={view}
              onClick={() => go(view === "agents" ? { view, agent: null } : { view })}
              aria-pressed={route.view === view}
              className={cn(
                "h-8 px-3 font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-colors",
                i > 0 && "border-l border-border",
                route.view === view ? "bg-surface-container-high text-on-surface" : "text-outline hover:bg-surface-container-high hover:text-on-surface",
              )}
            >
              {view}
            </button>
          ))}
        </nav>
        <div className="ml-auto flex items-center gap-2">
          {stale && live.at && <span className="hidden font-mono text-[10px] text-outline sm:inline">as of {formatClock(live.at)}</span>}
          <ConnectionBadge connection={connection} upstream={upstream} />
          {me.admin && (
            <a href="/admin" title="Admin" className="hidden h-8 w-8 items-center justify-center rounded border border-border text-outline hover:bg-surface-container-high hover:text-on-surface sm:inline-flex">
              <Shield className="h-3.5 w-3.5" />
            </a>
          )}
          <ThemeToggle />
          <button onClick={() => setDialog("create")} className={primaryButtonClass} aria-label="New agent">
            <Plus className="h-3.5 w-3.5" />
            <span className="hidden md:inline">New agent</span>
          </button>
        </div>
      </header>

      {loaded && !daemon && (needsDaemon || live.outbox > 0) && (
        <Banner tone="warning">
          Your Repowire daemon is not connected. Agents it created still answer you here, but their mesh tools fail
          {live.outbox > 0 ? ` and ${live.outbox} result${live.outbox === 1 ? "" : "s"} wait in the outbox` : ""}.{" "}
          <a href="#/clients" className="font-semibold underline underline-offset-2">Clients</a>
        </Banner>
      )}
      {listError && <InlineError className="mx-4 mt-3" error={listError} onRetry={() => void resync()} />}

      <div className={cn("flex min-h-0 flex-1 transition-opacity", stale && "opacity-60")}>
        {route.view === "clients" ? (
          <ClientsView clients={clients} gateway={live.clients} outbox={live.outbox} error={clientsError} now={now} onRetry={loadClients} onRevoke={revoke} />
        ) : (
          <>
            <div className={cn("min-h-0 w-full md:block md:w-[360px] md:shrink-0 md:border-r md:border-border-faint", selected ? "hidden" : "block")}>
              <AgentList
                agents={shown}
                total={live.agents.length}
                retiredCount={retiredCount}
                loading={!loaded && !listError}
                selected={selectedId}
                showRetired={showRetired}
                now={now}
                onShowRetired={setShowRetired}
                onSelect={(id) => go({ view: "agents", agent: id, tab: "chat" })}
                onCreate={() => setDialog("create")}
              />
            </div>
            <main className={cn("min-h-0 min-w-0 flex-1 flex-col bg-background", selected ? "flex" : "hidden md:flex")}>
              {selected && route.view === "agents" && route.agent ? (
                <AgentDetail
                  key={selected.id}
                  agent={selected}
                  tab={route.tab}
                  workspace={live.workspaces.find((w) => w.id === selected.workspace) ?? null}
                  runs={live.runs[selected.id]}
                  outgoing={outgoing.filter((m) => m.agent === selected.id)}
                  now={now}
                  onTab={(tab) => go({ view: "agents", agent: selected.id, tab })}
                  onBack={() => go({ view: "agents", agent: null })}
                  onSend={send}
                  onRetry={retrySend}
                  onTranscript={(items) => onTranscript(selected.id, items)}
                  onLoadRuns={() => managedApi.runs(selected.id).then((runs) => dispatch({ type: "runs", agent: selected.id, runs }))}
                  onStop={() => stop(selected)}
                  onRetire={() => setDialog("retire")}
                  onCopied={() => push("Copied")}
                  since={since}
                  onAgent={applyAgent}
                />
              ) : (
                <EmptyState icon={<Bot className="h-6 w-6" />} title={selectedId && loaded ? `No agent ${selectedId}` : "Select an agent"}>
                  {selectedId && loaded ? "It may belong to another account or was never created." : "Its transcript, runs and workspace open here."}
                </EmptyState>
              )}
            </main>
          </>
        )}
      </div>

      {!selected && (
        <nav className="flex shrink-0 border-t border-border-faint bg-surface-dim pb-[max(env(safe-area-inset-bottom),0.5rem)] md:hidden">
          <MobileTab active={route.view === "agents"} label="AGENTS" sub={`${live.agents.length - retiredCount} agents`} onClick={() => go({ view: "agents", agent: null })} />
          <MobileTab active={route.view === "clients"} label="CLIENTS" sub={daemon ? "daemon online" : "no daemon"} onClick={() => go({ view: "clients" })} />
        </nav>
      )}

      {dialog === "create" && (
        <CreateAgentDialog
          workspaces={live.workspaces}
          since={since}
          onClose={() => setDialog(null)}
          onCreated={(agent, existed, token) => {
            setDialog(null);
            applyAgent(agent, token);
            push(existed ? `Agent ${agent.id} already exists` : `Created ${agent.id}`);
            go({ view: "agents", agent: agent.id, tab: "chat" });
            void managedApi.workspaces().then((items) => items.forEach((workspace) => dispatch({ type: "workspace", workspace })), () => undefined);
          }}
        />
      )}
      {dialog === "retire" && selected && (
        <RetireDialog
          agent={selected}
          workspace={live.workspaces.find((w) => w.id === selected.workspace) ?? null}
          since={since}
          onClose={() => setDialog(null)}
          onRetired={(agent, token) => {
            setDialog(null);
            applyAgent(agent, token);
            push(`Retired ${agent.id}`);
          }}
        />
      )}
      <Toasts toasts={toasts} />
    </div>
  );
}

// Same pattern as the dashboard's MobileTabs: label plus a one-line count.
function MobileTab({ active, label, sub, onClick }: { active: boolean; label: string; sub: string; onClick: () => void }) {
  return (
    <button onClick={onClick} className={cn("flex flex-1 flex-col items-center gap-1 border-t-2 px-3 py-2", active ? "border-primary text-primary-fixed" : "border-transparent text-outline")}>
      <span className="font-mono text-[11px] font-semibold tracking-[0.18em]">{label}</span>
      <span className="font-mono text-[10px]">{sub}</span>
    </button>
  );
}
