export const escape = (text: string) => text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

// work's result, or null once ms pass; work itself is not cancelled.
export async function within<T>(work: Promise<T>, ms: number): Promise<T | null> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  return Promise.race([work, new Promise<null>((resolve) => { timer = setTimeout(() => resolve(null), Math.max(ms, 0)); })])
    .finally(() => clearTimeout(timer));
}

export class HttpError extends Error {
  constructor(public status: number, message: string) { super(message); }
}

export function identifier(value: unknown, name: string): string {
  if (typeof value !== 'string' || !/^[a-zA-Z0-9_-]{1,80}$/.test(value)) {
    throw new HttpError(400, `${name} must contain 1–80 letters, digits, underscores or hyphens`);
  }
  return value;
}

// Constant-time comparison of the request's bearer token with token.
async function bearerIs(request: Request, token: string): Promise<boolean> {
  const supplied = request.headers.get('Authorization') ?? '';
  const encode = new TextEncoder();
  const [actual, expected] = await Promise.all([
    crypto.subtle.digest('SHA-256', encode.encode(supplied)),
    crypto.subtle.digest('SHA-256', encode.encode(`Bearer ${token}`)),
  ]);
  const a = new Uint8Array(actual), b = new Uint8Array(expected);
  let difference = 0;
  for (let i = 0; i < a.length; i++) difference |= a[i] ^ b[i];
  return difference === 0;
}

export async function authenticate(request: Request, token?: string): Promise<void> {
  if (!token) throw new HttpError(503, 'Hosted authentication is not configured');
  if (!await bearerIs(request, token)) throw new HttpError(401, 'Unauthorized');
}

// Who is calling. The daemon's token is bound to one owner on the server.
// The relay's control token may name an owner per request; without one it
// reaches only the admin routes. End users never choose the tenant: they
// reach this service through the relay, which names them.
export interface Caller { role: 'daemon' | 'control'; owner: string | null }

export async function authorize(request: Request, env: { HOSTED_TOKEN?: string; HOSTED_OWNER?: string; CONTROL_TOKEN?: string }): Promise<Caller> {
  if (!env.HOSTED_TOKEN && !env.CONTROL_TOKEN) throw new HttpError(503, 'Hosted authentication is not configured');
  if (env.HOSTED_TOKEN && await bearerIs(request, env.HOSTED_TOKEN)) {
    if (!env.HOSTED_OWNER) throw new HttpError(503, 'Hosted owner is not configured');
    return { role: 'daemon', owner: env.HOSTED_OWNER };
  }
  if (env.CONTROL_TOKEN && await bearerIs(request, env.CONTROL_TOKEN)) {
    const owner = request.headers.get('X-Repowire-Owner');
    return { role: 'control', owner: owner ? identifier(owner, 'owner') : null };
  }
  throw new HttpError(401, 'Unauthorized');
}

// A JSON object body. With optional, an empty body reads as {}; anything else
// still has to be a valid object.
export async function body(request: Request, optional = false): Promise<Record<string, unknown>> {
  // Bound the streamed body too: Content-Length is optional and untrusted.
  const reader = request.body?.getReader();
  if (!reader) {
    if (optional) return {};
    throw new HttpError(400, 'JSON body required');
  }
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
  if (optional && size === 0) return {};
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
