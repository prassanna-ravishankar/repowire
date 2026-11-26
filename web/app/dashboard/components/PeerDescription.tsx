import { useEffect, useRef, useState } from "react";
import { Pencil } from "lucide-react";
import type { Peer } from "../types";
import { setPeerDescription } from "../lib/peerActions";

// A peer's task description in its header, editable by the operator (#215):
// the same field an agent sets with set_description. editSignal opens the
// editor from elsewhere (the command palette); onSaved refreshes the peer.
export function PeerDescription({
  peer,
  apiBase,
  onSaved,
  editSignal = 0,
}: {
  peer: Peer;
  apiBase: string;
  onSaved: () => void;
  editSignal?: number;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const input = useRef<HTMLInputElement>(null);

  const open = () => {
    setDraft(peer.description ?? "");
    setError(null);
    setEditing(true);
  };
  // A new signal (the palette's "Set description") opens the editor.
  const seen = useRef(editSignal);
  useEffect(() => {
    if (editSignal !== seen.current) {
      seen.current = editSignal;
      open();
    }
    // open reads the current peer; the signal alone decides when.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editSignal]);
  useEffect(() => {
    if (editing) input.current?.focus();
  }, [editing]);

  const save = async () => {
    setSaving(true);
    const err = await setPeerDescription(apiBase, peer.peer_id, draft);
    setSaving(false);
    if (err) {
      setError(err);
      return;
    }
    setEditing(false);
    onSaved();
  };

  if (editing) {
    return (
      <div className="border-b border-border-faint px-4 py-2 font-mono text-xs md:px-6">
        <div className="flex items-center gap-2">
          <span className="text-primary/70">&gt;</span>
          <input
            ref={input}
            aria-label="Peer description"
            value={draft}
            maxLength={200}
            disabled={saving}
            placeholder="What is this peer working on? (empty clears it)"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                void save();
              } else if (e.key === "Escape") {
                e.preventDefault();
                setEditing(false);
              }
            }}
            className="min-w-0 flex-1 rounded border border-border-faint bg-surface-container-lowest px-2 py-1 text-base text-on-surface outline-none focus:border-primary md:text-xs"
          />
          <button onClick={() => void save()} disabled={saving} className="text-outline hover:text-on-surface disabled:opacity-50">
            save
          </button>
          <button onClick={() => setEditing(false)} disabled={saving} className="text-outline hover:text-on-surface">
            cancel
          </button>
        </div>
        {error && <p className="mt-1 text-error">{error}</p>}
      </div>
    );
  }
  return (
    <div className="group flex items-center gap-2 border-b border-border-faint px-4 py-2 font-mono text-xs text-outline md:px-6">
      <span className="text-primary/70">&gt;</span>
      <span className="min-w-0 flex-1">{peer.description || <span className="italic">no description</span>}</span>
      <button onClick={open} aria-label="Edit peer description" className="text-outline opacity-60 hover:text-on-surface group-hover:opacity-100">
        <Pencil className="h-3.5 w-3.5" />
      </button>
    </div>
  );
}
