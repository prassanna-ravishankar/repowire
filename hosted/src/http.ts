export class HttpError extends Error {
  constructor(public status: number, message: string) { super(message); }
}

export function identifier(value: unknown, name: string): string {
  if (typeof value !== 'string' || !/^[a-zA-Z0-9_-]{1,80}$/.test(value)) {
    throw new HttpError(400, `${name} must contain 1–80 letters, digits, underscores or hyphens`);
  }
  return value;
}

export async function authenticate(request: Request, token?: string): Promise<void> {
  if (!token) throw new HttpError(503, 'Hosted authentication is not configured');
  const supplied = request.headers.get('Authorization') ?? '';
  const encode = new TextEncoder();
  const [actual, expected] = await Promise.all([
    crypto.subtle.digest('SHA-256', encode.encode(supplied)),
    crypto.subtle.digest('SHA-256', encode.encode(`Bearer ${token}`)),
  ]);
  const a = new Uint8Array(actual), b = new Uint8Array(expected);
  let difference = 0;
  for (let i = 0; i < a.length; i++) difference |= a[i] ^ b[i];
  if (difference !== 0) throw new HttpError(401, 'Unauthorized');
}

export async function body(request: Request): Promise<Record<string, unknown>> {
  // Bound the streamed body too: Content-Length is optional and untrusted.
  const reader = request.body?.getReader();
  if (!reader) throw new HttpError(400, 'JSON body required');
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const next = await reader.read();
    if (next.done) break;
    size += next.value.byteLength;
    if (size > 65536) {
      await reader.cancel();
      throw new HttpError(413, 'Body exceeds 64 KiB');
    }
    chunks.push(next.value);
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  try {
    const value = JSON.parse(new TextDecoder().decode(bytes));
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error();
    return value;
  } catch { throw new HttpError(400, 'JSON object required'); }
}

// The sender a mesh delivery names, bounded and shape-checked. Absent for
// direct API callers.
export function source(value: unknown) {
  if (value == null) return undefined;
  if (typeof value !== 'object' || Array.isArray(value)) throw new HttpError(400, 'source must be an object');
  const input = value as Record<string, unknown>;
  const field = (key: string, required: boolean) => {
    const item = input[key];
    if (item == null && !required) return undefined;
    if (typeof item !== 'string' || !item || item.length > 200) throw new HttpError(400, `source.${key} must be 1–200 characters`);
    return item;
  };
  const result: Record<string, string> = { from: field('from', true)!, type: field('type', true)! };
  for (const key of ['fromPeerId', 'correlationId']) {
    const item = field(key, false);
    if (item) result[key] = item;
  }
  return result;
}

export function scopedName(owner: string, id: string): string {
  return JSON.stringify([owner, id]);
}
