// Per-peer command history for the composer. A bounded ring of sent ask texts,
// keyed by peer_id (the stable identity — same keying as the draft store, so
// history never leaks across peers that share a display name).
//
// In-memory only, matching the draft model: history does not survive a reload.
// Persistence (localStorage) is a deliberate follow-up, not this slice.

// Max remembered sends per peer. Oldest entries fall off the front.
export const HISTORY_CAP = 50;

const history = new Map<string, string[]>();

/** Record a sent ask. Empty/whitespace-only and consecutive-duplicate sends
 *  are not stored, so ArrowUp recall doesn't cycle accidental repeats. */
export function pushHistory(peerId: string, text: string): void {
  if (!peerId) return;
  const trimmed = text.trim();
  if (!trimmed) return;
  const entries = history.get(peerId) ?? [];
  if (entries.length > 0 && entries[entries.length - 1] === trimmed) return;
  entries.push(trimmed);
  if (entries.length > HISTORY_CAP) entries.splice(0, entries.length - HISTORY_CAP);
  history.set(peerId, entries);
}

/** Sent entries for a peer, oldest first. Empty array when none. */
export function getHistory(peerId: string): string[] {
  return history.get(peerId) ?? [];
}

export function __resetHistoryForTests(): void {
  history.clear();
}
