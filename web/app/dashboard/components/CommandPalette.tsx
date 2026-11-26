import { useEffect, useMemo, useRef, useState } from "react";
import { Search } from "lucide-react";
import { cn } from "../lib/utils";

// The dashboard's command palette (#215): one keyboard entry point to the
// operator actions that already exist across the top bar, the roster and a
// peer's header. It adds no action of its own: commands are data, and the
// page runs each one (onRun) with the same handler its button uses.

export interface Command {
  id: string;
  label: string;
  /** Shown dimmed on the right: a circle, a shortcut. */
  hint?: string;
}

/** matches reports whether every word of query appears in text, any order. */
export function matches(text: string, query: string): boolean {
  const hay = text.toLowerCase();
  return query
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((word) => hay.includes(word));
}

/** useCommandShortcut toggles open on Cmd-K / Ctrl-K, from anywhere. */
export function useCommandShortcut(setOpen: (update: (open: boolean) => boolean) => void) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((open) => !open);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [setOpen]);
}

export function CommandPalette({ commands, onRun, onClose }: { commands: Command[]; onRun: (id: string) => void; onClose: () => void }) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const shown = useMemo(() => commands.filter((c) => matches(`${c.label} ${c.hint ?? ""}`, query)), [commands, query]);
  const current = Math.min(active, Math.max(shown.length - 1, 0));

  useEffect(() => {
    input.current?.focus();
  }, []);

  const run = (c: Command | undefined) => {
    if (!c) return;
    onClose();
    onRun(c.id);
  };

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/40 px-4 pt-[15vh]" onMouseDown={onClose}>
      <div
        role="dialog"
        aria-label="Command palette"
        className="w-full max-w-lg overflow-hidden rounded border border-border bg-surface-container shadow-xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2 border-b border-border-faint px-3 py-2">
          <Search aria-hidden className="h-4 w-4 text-outline" />
          <input
            ref={input}
            aria-label="Search commands"
            value={query}
            placeholder="Type a command or a peer"
            onChange={(e) => {
              setQuery(e.target.value);
              setActive(0);
            }}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setActive(Math.min(current + 1, shown.length - 1));
              } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setActive(Math.max(current - 1, 0));
              } else if (e.key === "Enter") {
                e.preventDefault();
                run(shown[current]);
              } else if (e.key === "Escape") {
                e.preventDefault();
                onClose();
              }
            }}
            className="min-w-0 flex-1 bg-transparent font-mono text-base text-on-surface outline-none placeholder:text-outline md:text-sm"
          />
          <kbd className="font-mono text-[10px] text-outline">esc</kbd>
        </div>
        <ul role="listbox" aria-label="Commands" className="max-h-80 overflow-y-auto py-1">
          {shown.length === 0 && <li className="px-3 py-2 font-mono text-xs text-outline">No command matches.</li>}
          {shown.map((c, i) => (
            <li
              key={c.id}
              role="option"
              aria-selected={i === current}
              onMouseEnter={() => setActive(i)}
              onClick={() => run(c)}
              className={cn(
                "flex cursor-pointer items-center gap-2 px-3 py-1.5 font-mono text-xs",
                i === current ? "bg-surface-container-high text-on-surface" : "text-on-surface-variant",
              )}
            >
              <span className="min-w-0 flex-1 truncate">{c.label}</span>
              {c.hint && <span className="shrink-0 text-[10px] text-outline">{c.hint}</span>}
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
