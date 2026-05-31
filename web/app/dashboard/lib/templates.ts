// Named, reusable ask templates for the composer. Global (not per-peer): a
// template like "run the tests and report back" is reusable across peers, so
// scoping it per-peer would just fragment them.
//
// Persisted to localStorage with the same versioned-blob + defensive-hydrate
// pattern as the command-history store (lib/history.ts). The in-memory list
// stays authoritative for the session; any storage failure degrades to
// in-memory only and never throws.

export const MAX_TEMPLATES = 50;
export const MAX_NAME_LEN = 80;
export const MAX_TEXT_LEN = 16_000;

const STORAGE_KEY = "repowire:composer-templates";
const SCHEMA_VERSION = 1;

export interface Template {
  id: string;
  name: string;
  text: string;
  updatedAt: number;
}

let templates: Template[] = [];
let loaded = false;
let idCounter = 0;

function nextId(now: number): string {
  // Monotonic-ish id without Math.random/Date.now coupling in callers.
  idCounter += 1;
  return `tpl-${now}-${idCounter}`;
}

function hasStorage(): boolean {
  try {
    return typeof window !== "undefined" && !!window.localStorage;
  } catch {
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
    if (!parsed || parsed.version !== SCHEMA_VERSION || !Array.isArray(parsed.templates)) {
      return; // unknown version / bad shape → discard cleanly
    }
    const seen = new Set<string>();
    for (const t of parsed.templates) {
      // Skip bad records rather than trusting the whole blob.
      if (!t || typeof t.name !== "string" || typeof t.text !== "string") continue;
      const name = t.name.trim().slice(0, MAX_NAME_LEN);
      const key = name.toLowerCase();
      if (!name || seen.has(key)) continue;
      seen.add(key);
      templates.push({
        id: typeof t.id === "string" && t.id ? t.id : nextId(0),
        name,
        text: t.text.slice(0, MAX_TEXT_LEN),
        updatedAt: typeof t.updatedAt === "number" ? t.updatedAt : 0,
      });
      if (templates.length >= MAX_TEMPLATES) break;
    }
  } catch {
    templates = [];
  }
}

function persist(): void {
  if (!hasStorage()) return;
  try {
    window.localStorage.setItem(
      STORAGE_KEY, JSON.stringify({ version: SCHEMA_VERSION, templates }),
    );
  } catch {
    /* quota / security → in-memory list stays authoritative */
  }
}

/** Templates, most-recently-updated first. */
export function listTemplates(): Template[] {
  ensureLoaded();
  return [...templates].sort((a, b) => b.updatedAt - a.updatedAt);
}

export type SaveResult = "saved" | "overwritten" | "rejected";

/** Save (or overwrite by case-insensitive name) a template. Rejects an empty
 *  name/text or text over the cap (silent truncation would surprise the author).
 *  ``now`` is injectable for deterministic tests. */
export function saveTemplate(name: string, text: string, now: number = Date.now()): SaveResult {
  ensureLoaded();
  const trimmedName = name.trim().slice(0, MAX_NAME_LEN);
  const trimmedText = text.trim();
  if (!trimmedName || !trimmedText) return "rejected";
  if (trimmedText.length > MAX_TEXT_LEN) return "rejected";
  const key = trimmedName.toLowerCase();
  const existing = templates.findIndex((t) => t.name.toLowerCase() === key);
  if (existing >= 0) {
    templates[existing] = { ...templates[existing], name: trimmedName, text: trimmedText, updatedAt: now };
    persist();
    return "overwritten";
  }
  if (templates.length >= MAX_TEMPLATES) {
    // Drop the least-recently-updated to make room.
    const oldest = templates.reduce((min, t, i) => (t.updatedAt < templates[min].updatedAt ? i : min), 0);
    templates.splice(oldest, 1);
  }
  templates.push({ id: nextId(now), name: trimmedName, text: trimmedText, updatedAt: now });
  persist();
  return "saved";
}

export function deleteTemplate(id: string): void {
  ensureLoaded();
  const before = templates.length;
  templates = templates.filter((t) => t.id !== id);
  if (templates.length !== before) persist();
}

export function __resetTemplatesForTests(): void {
  templates = [];
  loaded = false;
  idCounter = 0;
  if (hasStorage()) {
    try {
      window.localStorage.removeItem(STORAGE_KEY);
    } catch {
      /* ignore */
    }
  }
}
