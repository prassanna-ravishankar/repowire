// A Durable Object state for tests: SQL on node:sqlite, KV in memory, no sockets.
import { DatabaseSync, type SQLInputValue } from 'node:sqlite';

function cursor(rows: Record<string, unknown>[], rowsWritten: number) {
  return {
    rowsWritten,
    toArray: () => rows,
    one: () => {
      if (rows.length !== 1) throw new Error(`Expected exactly one row, got ${rows.length}`);
      return rows[0];
    },
  };
}

export function fakeState() {
  const db = new DatabaseSync(':memory:');
  const kv = new Map<string, unknown>();
  const sql = {
    exec(query: string, ...args: SQLInputValue[]) {
      if (!args.length && query.split(';').filter((part) => part.trim()).length > 1) {
        db.exec(query);
        return cursor([], 0);
      }
      const statement = db.prepare(query);
      if (/^\s*(SELECT|PRAGMA|WITH)\b/i.test(query)) return cursor(statement.all(...args) as Record<string, unknown>[], 0);
      return cursor([], Number(statement.run(...args).changes));
    },
  };
  // SQLite-backed objects share one store between the async and sync KV APIs.
  const list = (prefix = '', startAfter?: string, limit?: number) => [...kv]
    .filter(([key]) => key.startsWith(prefix) && (startAfter === undefined || key > startAfter))
    .sort(([a], [b]) => a.localeCompare(b))
    .slice(0, limit);
  const storage = {
    sql,
    kv: {
      get: (key: string) => kv.get(key),
      put: (key: string, value: unknown) => void kv.set(key, value),
      delete: (key: string) => kv.delete(key),
      list: ({ prefix, startAfter, limit }: { prefix?: string; startAfter?: string; limit?: number } = {}) => list(prefix, startAfter, limit),
    },
    transactionSync: <T>(closure: () => T) => closure(),
    get: async (key: string) => kv.get(key),
    put: async (key: string | Record<string, unknown>, value?: unknown) => {
      if (typeof key === 'string') kv.set(key, value);
      else for (const [k, v] of Object.entries(key)) kv.set(k, v);
    },
    delete: async (key: string) => kv.delete(key),
    deleteAlarm: async () => {},
    // Everything this object stores: its key-value pairs and its tables.
    deleteAll: async () => {
      kv.clear();
      for (const { name } of db.prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").all() as { name: string }[]) {
        db.exec(`DROP TABLE "${name}"`);
      }
    },
    list: async ({ prefix }: { prefix?: string } = {}) => new Map(list(prefix)),
  };
  // waitUntil keeps background work alive; here it runs and its failure is the test's to see.
  const background: Promise<unknown>[] = [];
  return { storage, blockConcurrencyWhile: <T>(fn: () => Promise<T>) => fn(), getWebSockets: () => [], acceptWebSocket: () => {}, getTags: () => [], background, waitUntil: (work: Promise<unknown>) => void background.push(work) };
}
