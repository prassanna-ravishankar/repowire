import { ApiError, type Receipt } from "./api";

/**
 * A message sent from this page, shown optimistically until the transcript
 * carries it. The operationId is minted once; a retry resends the same id so
 * the runtime deduplicates instead of running the message twice.
 */
export interface Outgoing {
  operationId: string;
  agent: string;
  text: string;
  status: "sending" | "accepted" | "failed";
  error: string | null;
  /** False when resending cannot help (bad request, retired agent). */
  retryable: boolean;
  createdAt: string;
}

/** Web operations carry the `web-` prefix so the daemon can ignore their reports. */
export function newOperationId(): string {
  return `web-${crypto.randomUUID()}`;
}

export function draft(agent: string, text: string, now = new Date()): Outgoing {
  return { operationId: newOperationId(), agent, text, status: "sending", error: null, retryable: true, createdAt: now.toISOString() };
}

const retryableStatus = (status: number) => status === 0 || status === 408 || status === 429 || status >= 500;

/** Send (or resend) one message. Never mints a new operationId. */
export async function deliver(
  message: Outgoing,
  send: (agent: string, operationId: string, text: string) => Promise<Receipt>,
): Promise<Outgoing> {
  try {
    await send(message.agent, message.operationId, message.text);
    return { ...message, status: "accepted", error: null };
  } catch (error) {
    const status = error instanceof ApiError ? error.status : 0;
    const text = error instanceof Error ? error.message : String(error);
    return { ...message, status: "failed", error: text, retryable: retryableStatus(status) };
  }
}

/** The retry copy of a failed message: same id, back to sending. */
export function retrying(message: Outgoing): Outgoing {
  return { ...message, status: "sending", error: null };
}

/**
 * Drop accepted messages the transcript now carries (a human entry with the
 * same text, recorded no earlier than a minute before it was sent). Sending
 * and failed messages always stay visible.
 */
export function reconcile(outgoing: Outgoing[], transcript: { kind: string; text?: string; at: number | null }[]): Outgoing[] {
  return outgoing.filter((message) => {
    if (message.status !== "accepted") return true;
    const since = Date.parse(message.createdAt) - 60_000;
    return !transcript.some((item) => item.kind === "human" && item.text === message.text && (item.at === null || item.at >= since));
  });
}
