import { useEffect, useState, type ReactNode } from "react";
import { Bot, Play, RefreshCw, X } from "lucide-react";
import { cn } from "../lib/utils";
import type { Peer } from "../types";
import { peerLabel } from "../types";
import { StatusLabel } from "./status";

interface SpawnConfig {
  enabled: boolean;
  allowed_commands: string[];
  allowed_paths: string[];
}

const inputClass = "w-full rounded border border-border-faint bg-surface-container-lowest px-3 py-2 font-mono text-sm text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary";

export function SpawnDialog({ apiBase, onClose, onSpawned }: { apiBase: string; onClose: () => void; onSpawned: () => void }) {
  const [config, setConfig] = useState<SpawnConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [path, setPath] = useState("");
  const [command, setCommand] = useState("");
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
        if (data.allowed_commands.length > 0) setCommand(data.allowed_commands[0]);
        setLoading(false);
      })
      .catch(() => {
        setConfig({ enabled: false, allowed_commands: [], allowed_paths: [] });
        setLoading(false);
      });
  }, [apiBase]);

  const handleSpawn = async () => {
    if (!path.trim() || !command || spawning) return;
    setError(null);
    setSpawning(true);
    try {
      const res = await fetch(`${apiBase}/spawn`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: path.trim(), command, circle }),
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
          <p className="font-mono text-xs">Set daemon.spawn.allowed_commands and daemon.spawn.allowed_paths in ~/.repowire/config.yaml</p>
        </div>
      ) : (
        <div className="space-y-4">
          <Field label="Project path">
            <input value={path} onChange={(event) => setPath(event.target.value)} placeholder="~/git/my-project" className={inputClass} />
          </Field>
          <Field label="Command">
            <select value={command} onChange={(event) => setCommand(event.target.value)} className={inputClass}>
              {config?.allowed_commands.map((cmd) => <option key={cmd} value={cmd}>{cmd}</option>)}
            </select>
          </Field>
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
            disabled={!path.trim() || !command || spawning}
            className="inline-flex items-center gap-2 rounded bg-primary px-4 py-2 font-mono text-xs font-bold uppercase tracking-[0.12em] text-on-primary transition-[filter,transform] hover:brightness-110 active:scale-[0.98] disabled:opacity-40"
          >
            {spawning ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
            Spawn
          </button>
        </div>
      )}
    </Modal>
  );
}

export function SettingsDialog({ apiBase, isConnected, peers, onClose }: { apiBase: string; isConnected: boolean; peers: Peer[]; onClose: () => void }) {
  const [relayEnabled, setRelayEnabled] = useState(false);
  const host = apiBase.replace(/^https?:\/\//, "");
  const servicePeers = peers.filter((peer) => peer.role === "service");

  return (
    <Modal title="Configuration" onClose={onClose} wide>
      <div className="space-y-5">
        <section className="border border-border-faint bg-surface-container-low p-4">
          <div className="mb-4 flex items-start justify-between gap-4">
            <div>
              <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-outline">Service identity</p>
              <h3 className="font-headline text-lg font-bold text-on-surface">Daemon status</h3>
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
          <div className="flex items-center justify-between gap-4">
            <div>
              <h3 className="font-headline text-sm font-semibold text-on-surface">Relay enabled</h3>
              <p className="text-xs text-outline">Tunnel local nodes to the hosted relay.</p>
            </div>
            <button
              role="switch"
              aria-checked={relayEnabled}
              onClick={() => setRelayEnabled((value) => !value)}
              className={cn("relative h-6 w-11 rounded-full transition-colors", relayEnabled ? "bg-primary" : "bg-surface-container-highest")}
            >
              <span className={cn("absolute top-1 h-4 w-4 rounded-full bg-on-surface transition-transform", relayEnabled ? "translate-x-6" : "translate-x-1")} />
            </button>
          </div>
          <Field label="API key">
            <input className={inputClass} type="password" placeholder="rw_..." readOnly />
          </Field>
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
                <p className="font-headline text-sm font-bold text-on-surface">{peerLabel(peer)}</p>
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

function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4" onClick={onClose}>
      <div
        className={cn("max-h-[90vh] w-full overflow-y-auto border border-border bg-surface-container-low shadow-[var(--shadow-3)]", wide ? "max-w-2xl" : "max-w-md")}
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
