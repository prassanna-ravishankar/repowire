// Turns a Pi transcript (EntryRecord[] from GET /agents/:id/messages) into
// display items. Pi kinds: pi.user, pi.assistant, pi.tool-result, pi.system,
// pi.reset, pi.compaction. Repowire labels a submission's sender with a
// `repowire.message` entry, and the daemon wraps peer input as
// <peer-message from="@x" type="ask" correlation-id="..."> (untrusted) or
// prefixes human relays as "@dashboard: text".

export type TranscriptItem =
  | { kind: "human"; id: string; text: string; at: number | null; via: string | null }
  | { kind: "peer"; id: string; text: string; at: number | null; from: string; type: string; correlationId: string | null }
  | { kind: "assistant"; id: string; text: string; at: number | null; error: string | null; replyTo: string | null }
  | { kind: "tool"; id: string; name: string; args: unknown; output: string | null; isError: boolean; durationMs: number | null; exitCode: number | null; at: number | null }
  | { kind: "notice"; id: string; text: string; at: number | null };

interface Message {
  role?: string;
  content?: unknown;
  timestamp?: number;
  toolCallId?: string;
  toolName?: string;
  isError?: boolean;
  durationMs?: number;
  errorMessage?: string;
  stopReason?: string;
}

interface Entry {
  id?: string;
  kind?: string;
  model?: Message[];
  data?: unknown;
}

const HUMAN_SENDERS = new Set(["dashboard", "telegram", "slack", "human", "web"]);

function textOf(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .filter((part): part is { type: "text"; text: string } => part?.type === "text" && typeof part.text === "string")
    .map((part) => part.text)
    .join("\n");
}

function unescapeHtml(text: string): string {
  return text
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&#34;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, "&");
}

const PEER_RE = /^\s*<peer-message([^>]*)>\n?([\s\S]*?)\n?<\/peer-message>\s*$/;
const ATTR_RE = /([\w-]+)="([^"]*)"/g;
const ACK_RE = /^\[ack #([\w-]+) from (@[^\]\s]+)\]\s*([\s\S]*)$/;
const HUMAN_RELAY_RE = /^@([\w.-]+)(?: → @[\w.-]+)?: ([\s\S]*)$/;

type Source = { from: string; type: string; correlationId: string | null };

/** Classify one piece of user-role input: human, or a peer (untrusted). */
export function classifyInput(text: string, source: Source | null): { kind: "human"; text: string; via: string | null } | ({ kind: "peer"; text: string } & Source) {
  const peer = PEER_RE.exec(text);
  if (peer) {
    const attrs: Record<string, string> = {};
    for (const [, key, value] of peer[1].matchAll(ATTR_RE)) attrs[key] = unescapeHtml(value);
    return {
      kind: "peer",
      text: unescapeHtml(peer[2]),
      from: attrs.from || source?.from || "@unknown",
      type: attrs.type || source?.type || "notify",
      correlationId: attrs["correlation-id"] || source?.correlationId || null,
    };
  }
  const ack = ACK_RE.exec(text);
  if (ack) return { kind: "peer", text: ack[3], from: ack[2], type: "ack", correlationId: ack[1] };
  const relay = HUMAN_RELAY_RE.exec(text);
  if (relay && HUMAN_SENDERS.has(relay[1].toLowerCase())) return { kind: "human", text: relay[2], via: `@${relay[1]}` };
  if (source) {
    const name = source.from.replace(/^@/, "").toLowerCase();
    if (!HUMAN_SENDERS.has(name)) return { kind: "peer", text, ...source };
    return { kind: "human", text, via: source.from.startsWith("@") ? source.from : `@${source.from}` };
  }
  return { kind: "human", text, via: null };
}

function sourceOf(data: unknown): Source | null {
  if (!data || typeof data !== "object") return null;
  const d = data as Record<string, unknown>;
  if (typeof d.from !== "string" || !d.from) return null;
  return {
    from: d.from.startsWith("@") ? d.from : `@${d.from}`,
    type: typeof d.type === "string" ? d.type : "notify",
    correlationId: typeof d.correlationId === "string" ? d.correlationId : null,
  };
}

function exitCodeOf(output: string | null): number | null {
  if (!output) return null;
  try {
    const parsed = JSON.parse(output) as Record<string, unknown>;
    const code = parsed.exitCode ?? parsed.exit_code ?? parsed.code;
    return typeof code === "number" ? code : null;
  } catch {
    return null;
  }
}

/** Accepts EntryRecord[], bare Message[], or either wrapped in {items}/{messages}. */
function entriesOf(raw: unknown): Entry[] {
  let value = raw;
  if (typeof value === "string") {
    try {
      value = JSON.parse(value);
    } catch {
      return [];
    }
  }
  if (value && !Array.isArray(value) && typeof value === "object") {
    const wrapped = value as { items?: unknown; messages?: unknown };
    value = wrapped.items ?? wrapped.messages;
  }
  if (!Array.isArray(value)) return [];
  return value.map((item, index): Entry => {
    if (item && typeof item === "object" && "role" in item) {
      const message = item as Message;
      const kind = message.role === "toolResult" ? "pi.tool-result" : `pi.${message.role}`;
      return { id: `m${index}`, kind, model: [message] };
    }
    return item as Entry;
  });
}

export function parseTranscript(raw: unknown): TranscriptItem[] {
  const items: TranscriptItem[] = [];
  const tools = new Map<string, Extract<TranscriptItem, { kind: "tool" }>>();
  let source: Source | null = null;
  let lastPeer: string | null = null;

  entriesOf(raw).forEach((entry, index) => {
    const id = entry.id ?? `e${index}`;
    const message = entry.model?.[0];
    const at = typeof message?.timestamp === "number" ? message.timestamp : null;
    switch (entry.kind) {
      case "repowire.message":
        source = sourceOf(entry.data);
        return;
      case "pi.user": {
        const text = textOf(message?.content);
        const input = classifyInput(text, source);
        source = null;
        if (input.kind === "peer") {
          lastPeer = input.type === "ask" ? input.from : null;
          items.push({ id, at, ...input });
        } else {
          lastPeer = null;
          items.push({ id, at, kind: "human", text: input.text, via: input.via });
        }
        return;
      }
      case "pi.assistant": {
        const content = Array.isArray(message?.content) ? message.content : [];
        for (const part of content as { type?: string; id?: string; name?: string; arguments?: unknown }[]) {
          if (part?.type !== "toolCall" || !part.id) continue;
          const tool = { kind: "tool" as const, id: `${id}:${part.id}`, name: part.name ?? "tool", args: part.arguments ?? {}, output: null, isError: false, durationMs: null, exitCode: null, at };
          tools.set(part.id, tool);
          items.push(tool);
        }
        const text = textOf(content).trim();
        const error = message?.stopReason === "error" || message?.stopReason === "aborted" ? message.errorMessage || message.stopReason : null;
        if (text || error) items.push({ kind: "assistant", id, text, at, error, replyTo: text ? lastPeer : null });
        return;
      }
      case "pi.tool-result": {
        const tool = message?.toolCallId ? tools.get(message.toolCallId) : undefined;
        const output = textOf(message?.content);
        if (tool) {
          tool.output = output;
          tool.isError = Boolean(message?.isError);
          tool.durationMs = typeof message?.durationMs === "number" ? message.durationMs : null;
          tool.exitCode = exitCodeOf(output);
        } else {
          items.push({ kind: "tool", id, name: message?.toolName ?? "tool", args: null, output, isError: Boolean(message?.isError), durationMs: message?.durationMs ?? null, exitCode: exitCodeOf(output), at });
        }
        return;
      }
      case "pi.reset":
        items.push({ kind: "notice", id, text: "Context reset", at });
        return;
      case "pi.compaction":
        items.push({ kind: "notice", id, text: "Earlier conversation compacted", at });
        return;
      default:
        return;
    }
  });
  return items;
}
