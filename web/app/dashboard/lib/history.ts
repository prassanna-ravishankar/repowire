// Per-peer command history for the composer. A bounded ring of sent ask texts,
// keyed by peer_id (the stable identity — same keying as the draft store, so
// history never leaks across peers that share a display name).
//
// Persisted to localStorage so history survives a reload (dashboard-local
// state, same trust level as drafts/theme). The in-memory Map stays
// authoritative for the session; localStorage is a best-effort mirror —
// any storage failure (quota, disabled, SSR) degrades to in-memory only.

// Max remembered sends per peer. Oldest entries fall off the front.
export const HISTORY_CAP = 50;
// Max peers retained in storage; the least-recently-written peer is evicted so
// a long-lived dashboard can't grow localStorage unbounded.
export const MAX_PEERS = 50;

const STORAGE_KEY = "repowire:composer-history";
const SCHEMA_VERSION = 1;

interface PeerHistory {
  entries: string[];
  updatedAt: number;
}

const history = new Map<string, PeerHistory>();
let loaded = false;

function hasStorage(): boolean {
  try {
    return typeof window !== "undefined" && !!window.localStorage;
  } catch {
    // Accessing window.localStorage can itself throw (sandboxed iframes).
    return false;
  }
}

function ensureLoaded(): void {
  if (loaded) return;
  loaded = true;
  if (!hasStorage()) return;
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return;
    const parsed = JSON.parse(raw);
    if (!parsed || parsed.version !== SCHEMA_VERSION || typeof parsed.peers !== "object") {
      return; // unknown version / bad shape → discard cleanly
    }
    for (const [peerId, rec] of Object.entries(parsed.peers as Record<string, unknown>)) {
      // Ignore individual bad peer records rather than trusting the whole blob.
      if (!rec || typeof rec !== "object") continue;
      const entries = (rec as PeerHistory).entries;
      const updatedAt = (rec as PeerHistory).updatedAt;
      if (!Array.isArray(entries)) continue;
      const clean = entries.filter((e) => typeof e === "string").slice(-HISTORY_CAP);
      if (clean.length === 0) continue;
      history.set(peerId, {
        entries: clean,
        updatedAt: typeof updatedAt === "number" ? updatedAt : 0,
      });
    }
  } catch {
    // Corrupt JSON / read error → start empty, never throw.
  }
}

function persist(): void {
  if (!hasStorage()) return;
  try {
    // Evict least-recently-written peers beyond MAX_PEERS before writing.
    if (history.size > MAX_PEERS) {
      const sorted = [...history.entries()].sort(
        (a, b) => a[1].updatedAt - b[1].updatedAt,
      );
      for (const [peerId] of sorted.slice(0, history.size - MAX_PEERS)) {
        history.delete(peerId);
      }
    }
    const peers: Record<string, PeerHistory> = {};
    for (const [peerId, rec] of history) peers[peerId] = rec;
    window.localStorage.setItem(
      STORAGE_KEY, JSON.stringify({ version: SCHEMA_VERSION, peers }),
    );
  } catch {
    // Quota / security error → keep the in-memory Map authoritative.
  }
}

/** Record a sent ask. Empty/whitespace-only and consecutive-duplicate sends
 *  are not stored, so ArrowUp recall doesn't cycle accidental repeats.
 *  ``now`` is injectable for deterministic tests. */
export function pushHistory(peerId: string, text: string, now: number = Date.now()): void {
  if (!peerId) return;
  ensureLoaded();
  const trimmed = text.trim();
  if (!trimmed) return;
  const rec = history.get(peerId) ?? { entries: [], updatedAt: 0 };
  if (rec.entries.length > 0 && rec.entries[rec.entries.length - 1] === trimmed) return;
  rec.entries.push(trimmed);
  if (rec.entries.length > HISTORY_CAP) {
    rec.entries.splice(0, rec.entries.length - HISTORY_CAP);
  }
  rec.updatedAt = now;
  history.set(peerId, rec);
  persist();
}

/** Sent entries for a peer, oldest first. Empty array when none. */
export function getHistory(peerId: string): string[] {
  ensureLoaded();
  return history.get(peerId)?.entries ?? [];
}

/** Clear one peer's history (the composer's "clear history" affordance). */
export function clearHistory(peerId: string): void {
  ensureLoaded();
  if (history.delete(peerId)) persist();
}

export function __resetHistoryForTests(): void {
  history.clear();
  loaded = false;
  // Also drop the persisted blob so a reset is a full wipe (memory + storage),
  // keeping test isolation simple — callers don't need to know the storage key.
  if (hasStorage()) {
    try {
      window.localStorage.removeItem(STORAGE_KEY);
    } catch {
      /* ignore */
    }
  }
}

/** Test-only: simulate a page reload — clear in-memory state but KEEP the
 *  persisted blob, so the next access re-hydrates from storage. */
export function __simulateReloadForTests(): void {
  history.clear();
  loaded = false;
}
