import { useMemo, useState } from "react";
import { Megaphone, RefreshCw } from "lucide-react";
import { Button, Modal } from "../../ui";
import type { Peer } from "../types";
import { peerAddressable, peerLabel } from "../types";

// A broadcast from the dashboard (#453). It is mesh-wide, so it lives on its
// own surface, never in a peer's composer: the dialog shows who it will
// reach and asks for that count before sending. It plugs into the daemon's
// POST /broadcast; the daemon decides the final audience and the result
// shows who it actually reached.

/** The peers a dashboard broadcast reaches: every online agent or
 *  orchestrator that takes input, in every circle (the human's broadcasts
 *  are not circle-bound). The human and its surfaces never take one. */
export function broadcastAudience(peers: Peer[]): Peer[] {
  return peers
    .filter((p) => (p.status === "online" || p.status === "busy") && (p.role === "agent" || p.role === "orchestrator" || p.role === undefined) && peerAddressable(p))
    .sort((a, b) => (a.circle || "").localeCompare(b.circle || "") || peerLabel(a).localeCompare(peerLabel(b)));
}

interface BroadcastResult {
  sent_to: string[];
  failed: { peer_id?: string; peer?: string; error?: string }[];
}

export function BroadcastDialog({ apiBase, peers, onClose }: { apiBase: string; peers: Peer[]; onClose: () => void }) {
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<BroadcastResult | null>(null);
  const audience = useMemo(() => broadcastAudience(peers), [peers]);
  const circles = useMemo(() => new Set(audience.map((p) => p.circle || "default")).size, [audience]);

  const send = async () => {
    if (!text.trim() || sending || audience.length === 0) return;
    setSending(true);
    setError(null);
    try {
      const res = await fetch(`${apiBase}/broadcast`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ from_peer: "dashboard", text: text.trim() }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || data.error) {
        setError(data.error || data.detail || `Error ${res.status}`);
        return;
      }
      setResult({ sent_to: data.sent_to ?? [], failed: data.failed ?? [] });
    } catch (e) {
      setError(e instanceof Error ? e.message : "Request failed");
    } finally {
      setSending(false);
    }
  };

  if (result) {
    return (
      <Modal title="Broadcast sent" onClose={onClose} footer={<Button variant="primary" onClick={onClose}>Done</Button>}>
        <p className="font-mono text-xs text-on-surface-variant">
          Reached {result.sent_to.length} peer{result.sent_to.length === 1 ? "" : "s"}
          {result.failed.length > 0 ? `; ${result.failed.length} failed` : ""}.
        </p>
        {result.sent_to.length > 0 && (
          <p className="mt-2 font-mono text-[11px] text-outline" data-testid="broadcast-sent-to">
            {result.sent_to.map((n) => "@" + n).join(", ")}
          </p>
        )}
        {result.failed.length > 0 && (
          <ul className="mt-2 font-mono text-[11px] text-error">
            {result.failed.map((f, i) => (
              <li key={i}>
                {f.peer || f.peer_id}: {f.error}
              </li>
            ))}
          </ul>
        )}
      </Modal>
    );
  }

  return (
    <Modal
      title="Broadcast to the mesh"
      onClose={onClose}
      footer={
        <Button variant="primary" onClick={() => void send()} disabled={!text.trim() || sending || audience.length === 0}>
          {sending ? <RefreshCw aria-hidden className="h-3.5 w-3.5 animate-spin" /> : <Megaphone aria-hidden className="h-3.5 w-3.5" />}
          {audience.length === 0 ? "No one to reach" : `Broadcast to ${audience.length} peer${audience.length === 1 ? "" : "s"}`}
        </Button>
      }
    >
      <p className="font-mono text-[11px] text-outline">
        Every online agent in every circle gets this as a broadcast from you, with no reply thread.
        {audience.length > 0 ? ` That is ${audience.length} peer${audience.length === 1 ? "" : "s"} in ${circles} circle${circles === 1 ? "" : "s"} now:` : " No agent is online now."}
      </p>
      {audience.length > 0 && (
        <ul className="mt-2 max-h-32 overflow-y-auto font-mono text-[11px] text-on-surface-variant" aria-label="broadcast audience">
          {audience.map((p) => (
            <li key={p.peer_id}>
              <span className="text-outline">{p.circle || "default"} /</span> {peerLabel(p)}
            </li>
          ))}
        </ul>
      )}
      <textarea
        aria-label="broadcast message"
        value={text}
        onChange={(e) => setText(e.target.value)}
        rows={4}
        placeholder="What should every agent hear?"
        className="mt-3 w-full rounded border border-border-faint bg-surface-container-lowest px-3 py-2 font-mono text-base text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary md:text-sm"
      />
      {error && <p className="mt-2 font-mono text-xs text-error">{error}</p>}
    </Modal>
  );
}
