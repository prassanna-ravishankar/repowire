import { useSyncExternalStore } from "react";

// Per-peer "is something dirty here" registry. Multiple sources (compose
// textarea today, editor buffers later) can independently mark a peer as
// protected; the peer stays protected until every source has cleared.
//
// Why: inbound SSE updates shouldn't yank the scroll position or clobber
// transient UI state while the user has unsaved input on that peer.

export type ProtectionSource = "compose" | "editor" | string;

type Listener = () => void;

const reasons = new Map<string, Set<ProtectionSource>>();
const frozenThreads = new Map<string, unknown[]>();
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
  set.add(source);
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

// Per-peer frozen thread snapshot. Captured at the moment protection first
// engages for a peer (so the captured snapshot reflects the thread the user
// was looking at when they started typing) and held until protection fully
// clears — survives the user switching to another peer and back.
export function setFrozenThread<T>(peerId: string, snapshot: T[]): void {
  if (!peerId) return;
  frozenThreads.set(peerId, snapshot as unknown[]);
  emit();
}

export function getFrozenThread<T>(peerId: string): T[] | null {
  const snap = frozenThreads.get(peerId);
  return (snap as T[] | undefined) ?? null;
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
  emit();
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
