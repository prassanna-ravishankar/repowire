import { useEffect, useState, type ReactNode } from "react";
import { Bot, Play, RefreshCw, X } from "lucide-react";
import { cn } from "../lib/utils";
import type { DaemonHealth, Peer } from "../types";
import { peerLabel } from "../types";
import { StatusLabel } from "./status";

interface SpawnConfig {
  enabled: boolean;
  commands?: Record<string, string>;
  profiles?: Record<string, Record<string, { args?: string[]; description?: string | null }>>;
  allowed_paths: string[];
}

function backendOptions(config: SpawnConfig | null): string[] {
  if (!config) return [];
  return Object.keys(config.commands ?? {});
}

function profileOptions(config: SpawnConfig | null, backend: string): string[] {
  if (!config || !backend) return [];
  return Object.keys(config.profiles?.[backend] ?? {});
}

const inputClass = "w-full rounded border border-border-faint bg-surface-container-lowest px-3 py-2 font-mono text-base text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary md:text-sm";

export function SpawnDialog({ apiBase, onClose, onSpawned }: { apiBase: string; onClose: () => void; onSpawned: () => void }) {
  const [config, setConfig] = useState<SpawnConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [path, setPath] = useState("");
  const [backend, setBackend] = useState("");
  const [profile, setProfile] = useState("");
  const [circle, setCircle] = useState("default");
  const [error, setError] = useState<string | null>(null);
  const [spawning, setSpawning] = useState(false);

  useEffect(() => {
    fetch(`${apiBase}/spawn/config`)
      .then((res) => {
        if (!res.ok) throw new Error(`${res.status}`);
        return res.json();
      })
      .then((data: SpawnConfig) => {
        setConfig(data);
        const options = backendOptions(data);
        if (options.length > 0) setBackend(options[0]);
        setLoading(false);
      })
      .catch(() => {
        setConfig({ enabled: false, commands: {}, allowed_paths: [] });
        setLoading(false);
      });
  }, [apiBase]);

  const handleSpawn = async () => {
    if (!path.trim() || !backend || spawning) return;
    setError(null);
    setSpawning(true);
    try {
      const res = await fetch(`${apiBase}/spawn`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: path.trim(), backend, profile: profile || undefined, circle }),
      });
      const data = await res.json();
      if (!res.ok) setError(data.detail || `Error ${res.status}`);
      else {
        onSpawned();
        onClose();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Spawn failed");
    } finally {
      setSpawning(false);
    }
  };

  return (
    <Modal title="Spawn new peer" onClose={onClose}>
      {loading ? (
        <div className="flex items-center justify-center py-8 font-mono text-sm text-outline">
          <RefreshCw className="mr-2 h-4 w-4 animate-spin" /> Loading config...
        </div>
      ) : config && !config.enabled ? (
        <div className="space-y-2 py-4 text-sm text-outline">
          <p className="text-on-surface-variant">Spawn is disabled.</p>
          <p className="font-mono text-xs">Set daemon.spawn.commands and daemon.spawn.allowed_paths in ~/.repowire/config.yaml</p>
        </div>
      ) : (
        <div className="space-y-4">
          <Field label="Project path">
            <input value={path} onChange={(event) => setPath(event.target.value)} placeholder="~/git/my-project" className={inputClass} />
          </Field>
          <Field label="Backend">
            <select value={backend} onChange={(event) => {
              setBackend(event.target.value);
              setProfile("");
            }} className={inputClass}>
              {backendOptions(config).map((name) => <option key={name} value={name}>{name}</option>)}
            </select>
          </Field>
          {profileOptions(config, backend).length > 0 && (
            <Field label="Profile">
              <select value={profile} onChange={(event) => setProfile(event.target.value)} className={inputClass}>
                <option value="">default</option>
                {profileOptions(config, backend).map((name) => (
                  <option key={name} value={name}>{name}</option>
                ))}
              </select>
            </Field>
          )}
          <Field label="Circle">
            <input value={circle} onChange={(event) => setCircle(event.target.value)} placeholder="default" className={inputClass} />
          </Field>
          {config && config.allowed_paths.length > 0 && (
            <p className="font-mono text-[10px] text-outline">Allowed: {config.allowed_paths.join(", ")}</p>
          )}
        </div>
      )}
      {error && <p className="mt-3 font-mono text-xs text-error">{error}</p>}
      {config?.enabled && (
        <div className="mt-5 flex justify-end">
          <button
            onClick={handleSpawn}
            disabled={!path.trim() || !backend || spawning}
            className="inline-flex items-center gap-2 rounded bg-primary px-4 py-2 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98] disabled:opacity-40"
          >
            {spawning ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
            Spawn
          </button>
        </div>
      )}
      <OrphanPanesList apiBase={apiBase} />
    </Modal>
  );
}

interface OrphanPane {
  pane_id: string;
  command: string;
  cwd: string;
  detected_backend: string;
  confidence: string;
}

/** Read-only list of unregistered tmux panes + the `repowire link` command to
 *  adopt each. Adoption itself is the explicit CLI step (no button in v1). */
export function OrphanPanesList({ apiBase }: { apiBase: string }) {
  const [panes, setPanes] = useState<OrphanPane[] | null>(null);

  useEffect(() => {
    let cancelled = false;
    fetch(`${apiBase}/panes/orphans`)
      .then((res) => (res.ok ? res.json() : { panes: [] }))
      .then((data: { panes: OrphanPane[] }) => {
        if (!cancelled) setPanes(data.panes ?? []);
      })
      .catch(() => {
        if (!cancelled) setPanes([]);
      });
    return () => {
      cancelled = true;
    };
  }, [apiBase]);

  if (panes === null || panes.length === 0) return null;

  return (
    <div className="mt-6 border-t border-border-faint pt-4" data-testid="orphan-panes">
      <p className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-outline">
        Unlinked panes — adopt with the CLI
      </p>
      <div className="space-y-2">
        {panes.map((p) => {
          const backend = p.confidence === "hint" ? p.detected_backend : "<backend>";
          return (
            <div key={p.pane_id} className="font-mono text-[11px]">
              <div className="flex items-center gap-2 text-on-surface-variant">
                <span className="font-bold text-on-surface">{p.pane_id}</span>
                <span className={p.confidence === "hint" ? "text-primary" : "text-outline"}>
                  {p.detected_backend}
                </span>
                <span className="min-w-0 flex-1 truncate text-outline">{p.command} · {p.cwd}</span>
              </div>
              <code className="mt-0.5 block select-all rounded bg-surface-container-lowest px-2 py-1 text-[10px] text-outline">
                repowire link --pane {p.pane_id} --backend {backend}
              </code>
            </div>
          );
        })}
      </div>
    </div>
  );
}

export function SettingsDialog({ apiBase, isConnected, peers, onClose }: { apiBase: string; isConnected: boolean; peers: Peer[]; onClose: () => void }) {
  const [health, setHealth] = useState<DaemonHealth | null>(null);
  const [healthError, setHealthError] = useState<string | null>(null);
  const [healthLoading, setHealthLoading] = useState(true);
  const host = apiBase.replace(/^https?:\/\//, "");
  const servicePeers = peers.filter((peer) => peer.role === "service");

  useEffect(() => {
    let cancelled = false;
    fetch(`${apiBase}/health`)
      .then((res) => {
        if (!res.ok) throw new Error(`Error ${res.status}`);
        return res.json();
      })
      .then((data: DaemonHealth) => {
        if (!cancelled) {
          setHealth(data);
          setHealthError(null);
        }
      })
      .catch((err) => {
        if (!cancelled) setHealthError(err instanceof Error ? err.message : "Failed to load health");
      })
      .finally(() => {
        if (!cancelled) setHealthLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [apiBase]);

  return (
    <Modal title="Configuration" onClose={onClose} wide>
      <div className="space-y-5">
        <section className="border border-border-faint bg-surface-container-low p-4">
          <div className="mb-4 flex items-start justify-between gap-4">
            <div>
              <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-outline">Service identity</p>
              <h3 className="font-headline text-lg font-semibold text-on-surface">Daemon status</h3>
            </div>
            <div className={cn("flex items-center gap-2 border px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.12em]", isConnected ? "border-secondary/25 bg-secondary/10 text-secondary" : "border-error/25 bg-error/10 text-error")}>
              <span className={cn("h-2 w-2 rounded-full", isConnected ? "bg-secondary pulse-online" : "bg-error")} />
              {isConnected ? "Running" : "Disconnected"}
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Metric label="Host address" value={host} />
            <Metric label="Status" value={isConnected ? "Active" : "Unreachable"} />
          </div>
        </section>

        <section className="border border-border-faint bg-surface-container-low p-4">
          <div className="mb-4 flex items-start justify-between gap-4">
            <div>
              <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-outline">Runtime health</p>
              <h3 className="font-headline text-sm font-semibold text-on-surface">Daemon report</h3>
            </div>
            <HealthBadge status={health?.status ?? (healthLoading ? "loading" : "error")} />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Metric label="Version" value={health?.version || "-"} />
            <Metric label="Relay mode" value={health ? (health.relay_mode ? "Enabled" : "Disabled") : "-"} />
            <Metric label="Channel" value={health?.channel?.status || "-"} />
            <Metric label="ACP broker" value={health?.acp_broker?.status || "-"} />
            <Metric label="ACP clients" value={String(health?.acp_broker?.active_clients ?? "-")} />
            <Metric label="Approvals pending" value={String(health?.acp_broker?.permissions?.pending ?? "-")} />
          </div>
          {health?.acp_broker?.last_error && (
            <p className="mt-3 break-words font-mono text-xs text-error">{health.acp_broker.last_error}</p>
          )}
          {health?.channel?.last_error && (
            <p className="mt-3 break-words font-mono text-xs text-error">{health.channel.last_error}</p>
          )}
          {healthError && <p className="mt-3 font-mono text-xs text-error">{healthError}</p>}
        </section>

        <section>
          <h3 className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">External integrations</h3>
          <div className="grid gap-3 sm:grid-cols-2">
            {servicePeers.length > 0 ? servicePeers.map((peer) => (
              <div key={peer.peer_id} className="border border-border-faint border-t-2 border-t-primary bg-surface-container-low p-4">
                <div className="mb-3 flex items-center justify-between">
                  <Bot className="h-5 w-5 text-on-surface-variant" />
                  <StatusLabel status={peer.status} />
                </div>
                <p className="font-headline text-sm font-semibold text-on-surface">{peerLabel(peer)}</p>
                <p className="font-mono text-[10px] text-outline">{peer.backend || "service"} · {peer.circle}</p>
              </div>
            )) : (
              <div className="border border-border-faint bg-surface-container-low p-4 text-sm text-outline">No service-role peers connected</div>
            )}
          </div>
        </section>
      </div>
    </Modal>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="mt-3 block space-y-1.5">
      <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-outline">{label}</span>
      {children}
    </label>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="border-l-2 border-primary/60 bg-surface-container-lowest p-3">
      <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.14em] text-outline">{label}</p>
      <p className="font-mono text-sm text-primary-fixed">{value}</p>
    </div>
  );
}

function HealthBadge({ status }: { status: string }) {
  const ok = status === "ok";
  const loading = status === "loading";
  return (
    <div className={cn(
      "flex items-center gap-2 border px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.12em]",
      ok
        ? "border-secondary/25 bg-secondary/10 text-secondary"
        : loading
          ? "border-border-faint bg-surface-container-high text-outline"
          : "border-error/25 bg-error/10 text-error",
    )}>
      <span className={cn("h-2 w-2 rounded-full", ok ? "bg-secondary pulse-online" : loading ? "bg-outline" : "bg-error")} />
      {loading ? "Loading" : ok ? "Healthy" : "Unknown"}
    </div>
  );
}

function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      <div
        className={cn("max-h-[90vh] w-full overflow-y-auto border border-border bg-surface-container-low shadow-[var(--shadow-xl)]", wide ? "max-w-2xl" : "max-w-md")}
        onClick={(event) => event.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-border-faint px-5 py-4">
          <h2 className="font-mono text-xs font-bold uppercase tracking-[0.2em] text-primary">{title}</h2>
          <button onClick={onClose} className="text-outline transition-colors hover:text-on-surface" aria-label="Close">
            <X className="h-4 w-4" />
          </button>
        </div>
        <div className="p-5">{children}</div>
      </div>
    </div>
  );
}
