import { useState } from "react";
import { cn } from "../../dashboard/lib/utils";
import type { Client, Clients, OAuthApp } from "../lib/api";
import { formatAgo, formatDate, formatWhen } from "../lib/state";
import { InlineError, SectionLabel, SkeletonRows, buttonClass } from "./ui";

export function ClientsView({
  clients,
  gateway,
  outbox,
  error,
  now,
  onRetry,
  onRevoke,
}: {
  clients: Clients | null;
  gateway: Client[];
  outbox: number;
  error: string | null;
  now: number;
  onRetry: () => void;
  onRevoke: (app: OAuthApp) => Promise<void>;
}) {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto pb-6">
      <div className="mx-auto max-w-3xl">
        {error && <InlineError className="m-4" error={error} onRetry={onRetry} />}
        <SectionLabel right={<Muted>One per account. A new connection replaces it.</Muted>}>Gateway daemon</SectionLabel>
        <Card>
          {gateway.length === 0 ? (
            <Row dot="bg-outline" title="No daemon has connected">
              Run <code>repowire setup</code> with the hosted backend enabled on a machine. Agents still answer you here; mesh tools fail until one connects.
            </Row>
          ) : (
            gateway.map((client) => (
              <Row
                key={client.daemon_id ?? client.connected_at}
                dot={client.connected ? "bg-secondary pulse-online" : "bg-outline"}
                title={client.machine ?? client.daemon_id ?? "unknown machine"}
                right={
                  <span className={cn(outbox > 0 && !client.connected ? "text-tertiary-fixed-dim" : "text-outline")}>outbox {outbox}</span>
                }
              >
                {[client.version ? `repowire ${client.version}` : "version unknown", client.connected ? `connected since ${formatWhen(client.connected_at, now)}` : `last seen ${formatWhen(client.disconnected_at ?? client.connected_at, now)}`].join(" · ")}
              </Row>
            ))
          )}
        </Card>

        <SectionLabel right={<Muted>Daemons linked to the relay</Muted>}>Relay daemons</SectionLabel>
        <Card>
          {!clients ? (
            <SkeletonRows rows={1} />
          ) : clients.daemons.length === 0 ? (
            <Row dot="bg-outline" title="None connected">Daemons appear here while they hold a relay connection.</Row>
          ) : (
            clients.daemons.map((d) => (
              <Row key={d.daemon_id} dot="bg-secondary" title={d.daemon_id}>
                connected {formatAgo(d.connected_at, now)}
              </Row>
            ))
          )}
        </Card>

        <SectionLabel right={<Muted>OAuth grants</Muted>}>Apps</SectionLabel>
        <Card>
          {!clients ? (
            <SkeletonRows rows={1} />
          ) : clients.apps.length === 0 ? (
            <Row dot="bg-outline" title="No apps">Apps you authorise to reach your mesh appear here.</Row>
          ) : (
            clients.apps.map((app) => <AppRow key={app.grant_id} app={app} onRevoke={onRevoke} />)
          )}
        </Card>
      </div>
    </div>
  );
}

function AppRow({ app, onRevoke }: { app: OAuthApp; onRevoke: (app: OAuthApp) => Promise<void> }) {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const revoke = async () => {
    setBusy(true);
    setError(null);
    try {
      await onRevoke(app);
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
      setConfirming(false);
    }
  };
  return (
    <Row
      dot={app.active ? "bg-secondary" : "bg-outline"}
      title={app.client_name ?? app.client_id}
      right={
        confirming ? (
          <span className="flex gap-2">
            <button onClick={() => setConfirming(false)} className={buttonClass}>Keep</button>
            <button onClick={revoke} disabled={busy} className={cn(buttonClass, "border-error/40 text-error hover:text-error")}>
              {busy ? "Revoking" : "Revoke"}
            </button>
          </span>
        ) : (
          <button onClick={() => setConfirming(true)} className={buttonClass}>Revoke</button>
        )
      }
    >
      {[app.scope || "no scope", app.active ? `expires ${formatDate(app.expires)}` : "inactive"].join(" · ")}
      {confirming && !busy && <span className="block text-on-surface-variant">Revoking signs {app.client_name ?? app.client_id} out. It must be authorised again to reconnect.</span>}
      {error && <InlineError className="mt-2" error={error} />}
    </Row>
  );
}

function Muted({ children }: { children: React.ReactNode }) {
  return <span className="hidden font-mono text-[10px] text-outline sm:inline">{children}</span>;
}

function Card({ children }: { children: React.ReactNode }) {
  return <div className="mx-4 overflow-hidden rounded-[8px] border border-border bg-surface-container-low">{children}</div>;
}

function Row({ dot, title, right, children }: { dot: string; title: string; right?: React.ReactNode; children?: React.ReactNode }) {
  return (
    <div className="flex items-start gap-3 border-b border-border-faint px-4 py-3 last:border-b-0">
      <span className={cn("mt-1.5 h-2 w-2 shrink-0 rounded-full", dot)} />
      <div className="min-w-0 flex-1">
        <div className="truncate font-mono text-[13px] font-semibold text-on-surface">{title}</div>
        {children && <div className="mt-0.5 font-mono text-[11px] leading-5 text-outline [&_code]:text-on-surface-variant">{children}</div>}
      </div>
      {right && <div className="shrink-0 font-mono text-[11px]">{right}</div>}
    </div>
  );
}
