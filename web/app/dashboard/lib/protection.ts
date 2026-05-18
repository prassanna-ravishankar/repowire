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
  if (set.size === 0) reasons.delete(peerId);
  emit();
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
