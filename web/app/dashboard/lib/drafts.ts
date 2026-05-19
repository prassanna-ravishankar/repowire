import { useSyncExternalStore } from "react";
import { clearProtected, markProtected } from "./protection";

// Per-peer compose draft store. Draft text and attachments live here (not in
// ComposeBar local state) so they survive peer switches and don't leak across
// peers via shared component state.
//
// Setters mark/clear protection SYNCHRONOUSLY (not via useEffect) so that
// between the user keystroke and the next render, useIsPeerProtected already
// reflects the new state. This closes the "passive effect race" window where
// an SSE event delivered after onChange but before the dirty-effect runs
// could leak through into the rendered thread.

type Listener = () => void;

interface Draft {
  text: string;
  file: File | null;
}

const drafts = new Map<string, Draft>();
const listeners = new Map<string, Set<Listener>>();

function emit(peerId: string) {
  const set = listeners.get(peerId);
  if (!set) return;
  for (const l of set) l();
}

function getOrInit(peerId: string): Draft {
  let d = drafts.get(peerId);
  if (!d) {
    d = { text: "", file: null };
    drafts.set(peerId, d);
  }
  return d;
}

function isDirty(d: Draft): boolean {
  return d.text.trim().length > 0 || d.file !== null;
}

function syncProtection(peerId: string, before: boolean, after: boolean) {
  if (before === after) return;
  if (after) markProtected(peerId, "compose");
  else clearProtected(peerId, "compose");
}

export function getDraftText(peerId: string): string {
  return drafts.get(peerId)?.text ?? "";
}

export function getDraftFile(peerId: string): File | null {
  return drafts.get(peerId)?.file ?? null;
}

export function setDraftText(peerId: string, text: string): void {
  if (!peerId) return;
  const d = getOrInit(peerId);
  if (d.text === text) return;
  const wasDirty = isDirty(d);
  d.text = text;
  syncProtection(peerId, wasDirty, isDirty(d));
  emit(peerId);
}

export function setDraftFile(peerId: string, file: File | null): void {
  if (!peerId) return;
  const d = getOrInit(peerId);
  if (d.file === file) return;
  const wasDirty = isDirty(d);
  d.file = file;
  syncProtection(peerId, wasDirty, isDirty(d));
  emit(peerId);
}

export function clearDraft(peerId: string): void {
  if (!peerId) return;
  const d = drafts.get(peerId);
  if (!d) return;
  const wasDirty = isDirty(d);
  d.text = "";
  d.file = null;
  syncProtection(peerId, wasDirty, false);
  emit(peerId);
}

export function __resetDraftsForTests(): void {
  drafts.clear();
  for (const [peerId] of listeners) emit(peerId);
}

function subscribe(peerId: string) {
  return (listener: Listener) => {
    let set = listeners.get(peerId);
    if (!set) {
      set = new Set();
      listeners.set(peerId, set);
    }
    set.add(listener);
    return () => {
      set!.delete(listener);
      if (set!.size === 0) listeners.delete(peerId);
    };
  };
}

export function useDraftText(peerId: string): string {
  return useSyncExternalStore(
    subscribe(peerId),
    () => getDraftText(peerId),
    () => ""
  );
}

export function useDraftFile(peerId: string): File | null {
  return useSyncExternalStore(
    subscribe(peerId),
    () => getDraftFile(peerId),
    () => null
  );
}
