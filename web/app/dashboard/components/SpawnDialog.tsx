"use client";

import { useState, useEffect } from "react";
import { X, Play, RefreshCw } from "lucide-react";
import { cn } from "../lib/utils";

interface SpawnConfig {
  enabled: boolean;
  allowed_commands: string[];
  allowed_paths: string[];
}

interface SpawnDialogProps {
  apiBase: string;
  onClose: () => void;
  onSpawned: () => void;
}

export function SpawnDialog({ apiBase, onClose, onSpawned }: SpawnDialogProps) {
  const [config, setConfig] = useState<SpawnConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [path, setPath] = useState("");
  const [command, setCommand] = useState("");
  const [circle, setCircle] = useState("default");
  const [error, setError] = useState<string | null>(null);
  const [spawning, setSpawning] = useState(false);

  useEffect(() => {
    fetch(`${apiBase}/spawn/config`)
      .then((r) => {
        if (!r.ok) throw new Error(`${r.status}`);
        return r.json();
      })
      .then((data: SpawnConfig) => {
        setConfig(data);
        if (data.allowed_commands.length > 0) setCommand(data.allowed_commands[0]);
        setLoading(false);
      })
      .catch(() => {
        // Endpoint not available — spawn not supported by this daemon version
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
      if (!res.ok) {
        setError(data.detail || `Error ${res.status}`);
      } else {
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
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50" onClick={onClose}>
      <div
        className="bg-zinc-900 border border-zinc-700 rounded-xl w-full max-w-md mx-4 shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-zinc-800">
          <h2 className="text-sm font-semibold text-zinc-200">New Session</h2>
          <button onClick={onClose} className="p-1 hover:bg-zinc-800 rounded-md transition-colors">
            <X className="w-4 h-4 text-zinc-500" />
          </button>
        </div>

        {/* Body */}
        <div className="px-5 py-4 space-y-4">
          {loading ? (
            <div className="flex items-center justify-center py-8 text-zinc-500 text-sm">
              <RefreshCw className="w-4 h-4 animate-spin mr-2" /> Loading config...
            </div>
          ) : config && !config.enabled ? (
            <div className="text-sm text-zinc-500 py-4">
              <p className="text-zinc-400 mb-2">Spawn is disabled.</p>
              <p className="text-xs font-mono">
                Set <code className="text-zinc-300">daemon.spawn.allowed_commands</code> and{" "}
                <code className="text-zinc-300">daemon.spawn.allowed_paths</code> in{" "}
                <code className="text-zinc-300">~/.repowire/config.yaml</code>
              </p>
            </div>
          ) : (
            <>
              {/* Path */}
              <div>
                <label className="text-[10px] text-zinc-500 uppercase tracking-wider font-mono block mb-1.5">
                  Project Path
                </label>
                <input
                  type="text"
                  value={path}
                  onChange={(e) => setPath(e.target.value)}
                  placeholder="~/git/my-project"
                  className="w-full bg-zinc-950 border border-zinc-700 rounded-lg px-3 py-2 text-sm text-zinc-300 placeholder-zinc-600 focus:outline-none focus:ring-1 focus:ring-zinc-500"
                />
                {config && config.allowed_paths.length > 0 && (
                  <p className="text-[10px] text-zinc-600 mt-1 font-mono">
                    Allowed: {config.allowed_paths.join(", ")}
                  </p>
                )}
              </div>

              {/* Command */}
              <div>
                <label className="text-[10px] text-zinc-500 uppercase tracking-wider font-mono block mb-1.5">
                  Command
                </label>
                <select
                  value={command}
                  onChange={(e) => setCommand(e.target.value)}
                  className="w-full bg-zinc-950 border border-zinc-700 rounded-lg px-3 py-2 text-sm text-zinc-300 focus:outline-none focus:ring-1 focus:ring-zinc-500"
                >
                  {config?.allowed_commands.map((cmd) => (
                    <option key={cmd} value={cmd}>{cmd}</option>
                  ))}
                </select>
              </div>

              {/* Circle */}
              <div>
                <label className="text-[10px] text-zinc-500 uppercase tracking-wider font-mono block mb-1.5">
                  Circle
                </label>
                <input
                  type="text"
                  value={circle}
                  onChange={(e) => setCircle(e.target.value)}
                  placeholder="default"
                  className="w-full bg-zinc-950 border border-zinc-700 rounded-lg px-3 py-2 text-sm text-zinc-300 placeholder-zinc-600 focus:outline-none focus:ring-1 focus:ring-zinc-500"
                />
              </div>
            </>
          )}

          {error && <p className="text-xs text-red-400 font-mono">{error}</p>}
        </div>

        {/* Footer */}
        {config?.enabled && (
          <div className="px-5 py-3 border-t border-zinc-800 flex justify-end">
            <button
              onClick={handleSpawn}
              disabled={!path.trim() || !command || spawning}
              className={cn(
                "flex items-center gap-2 px-4 py-2 rounded-lg text-sm font-medium transition-colors",
                "bg-emerald-600 hover:bg-emerald-500 text-white",
                "disabled:opacity-40 disabled:cursor-not-allowed"
              )}
            >
              {spawning ? (
                <RefreshCw className="w-4 h-4 animate-spin" />
              ) : (
                <Play className="w-4 h-4" />
              )}
              Spawn
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
