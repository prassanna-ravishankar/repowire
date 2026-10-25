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
  const storage = {
    sql,
    get: async (key: string) => kv.get(key),
    put: async (key: string | Record<string, unknown>, value?: unknown) => {
      if (typeof key === 'string') kv.set(key, value);
      else for (const [k, v] of Object.entries(key)) kv.set(k, v);
    },
    delete: async (key: string) => kv.delete(key),
    list: async ({ prefix = '' }: { prefix?: string } = {}) =>
      new Map([...kv].filter(([key]) => key.startsWith(prefix)).sort(([a], [b]) => a.localeCompare(b))),
  };
  return { storage, getWebSockets: () => [], acceptWebSocket: () => {}, getTags: () => [] };
}
