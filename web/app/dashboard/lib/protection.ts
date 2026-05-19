import { useSyncExternalStore } from "react";

// Per-peer "is something dirty here" registry. Multiple sources (compose
// textarea today, editor buffers later) can independently mark a peer as
// protected; the peer stays protected until every source has cleared.
//
// Why: inbound SSE updates shouldn't yank the scroll position or clobber
// transient UI state while the user has unsaved input on that peer.

export type ProtectionSource = "compose" | "editor" | string;

type Listener = () => void;

type SnapshotProvider = () => unknown[];

const reasons = new Map<string, Set<ProtectionSource>>();
const frozenThreads = new Map<string, unknown[]>();
const snapshotProviders = new Map<string, SnapshotProvider>();
const listeners = new Set<Listener>();

function emit() {
  for (const l of listeners) l();
}

export function markProtected(peerId: string, source: ProtectionSource): void {
  if (!peerId) return;
  let set = reasons.get(peerId);
  if (!set) {
    set = new Set();
    reasons.set(peerId, set);
  }
  if (set.has(source)) return;
  const isFirstSource = set.size === 0;
  set.add(source);
  // On the protected → protected transition (first source for this peer),
  // capture a snapshot from the registered provider. This runs from the
  // synchronous input/store-update path, never from a component render.
  if (isFirstSource && !frozenThreads.has(peerId)) {
    const provider = snapshotProviders.get(peerId);
    if (provider) frozenThreads.set(peerId, provider());
  }
  emit();
}

export function clearProtected(peerId: string, source: ProtectionSource): void {
  if (!peerId) return;
  const set = reasons.get(peerId);
  if (!set || !set.has(source)) return;
  set.delete(source);
  if (set.size === 0) {
    reasons.delete(peerId);
    // When the last source releases, drop the frozen snapshot so the next
    // protection cycle captures fresh.
    frozenThreads.delete(peerId);
  }
  emit();
}

// Per-peer frozen thread snapshot. Captured automatically by markProtected
// from a registered provider, held until the last protection source clears.
// Lives next to the protection state so it survives the user navigating
// between peers while their draft is still dirty.
export function getFrozenThread<T>(peerId: string): T[] | null {
  const snap = frozenThreads.get(peerId);
  return (snap as T[] | undefined) ?? null;
}

// Register/update the snapshot provider for a peer. Called from the
// displayed PeerView via useEffect so the closure always sees the latest
// liveThread; markProtected calls it at the moment protection first engages.
// Returns an unregister function.
export function registerSnapshotProvider<T>(
  peerId: string,
  provider: () => T[]
): () => void {
  snapshotProviders.set(peerId, provider as SnapshotProvider);
  return () => {
    // Only unregister if still the same provider (peer-switch races).
    if (snapshotProviders.get(peerId) === (provider as SnapshotProvider)) {
      snapshotProviders.delete(peerId);
    }
  };
}

export function isProtected(peerId: string): boolean {
  const set = reasons.get(peerId);
  return !!set && set.size > 0;
}

export function getProtectionSources(peerId: string): ProtectionSource[] {
  const set = reasons.get(peerId);
  return set ? Array.from(set) : [];
}

export function __resetProtectionForTests(): void {
  reasons.clear();
  frozenThreads.clear();
  snapshotProviders.clear();
  // Intentionally no emit() — tests call this between renders when no
  // components are subscribed and an emit would trip the React "update
  // outside act()" warning during teardown.
}

function subscribe(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function useIsPeerProtected(peerId: string | null | undefined): boolean {
  return useSyncExternalStore(
    subscribe,
    () => (peerId ? isProtected(peerId) : false),
    () => false
  );
}

export function useFrozenThread<T>(peerId: string | null | undefined): T[] | null {
  return useSyncExternalStore(
    subscribe,
    () => (peerId ? getFrozenThread<T>(peerId) : null),
    () => null
  );
}
