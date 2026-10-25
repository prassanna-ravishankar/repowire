import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { deliver, draft, reconcile, retrying } from "./composer";

describe("composer operation ids", () => {
  it("mints web- prefixed ids, unique per message", () => {
    const a = draft("helper", "hi");
    const b = draft("helper", "hi");
    expect(a.operationId).toMatch(/^web-[0-9a-f-]{36}$/);
    expect(a.operationId).not.toBe(b.operationId);
    expect(a.status).toBe("sending");
  });

  it("marks a message accepted on a receipt", async () => {
    const send = vi.fn().mockResolvedValue({ operationId: "x" });
    const message = draft("helper", "hi");
    const done = await deliver(message, send);
    expect(done.status).toBe("accepted");
    expect(send).toHaveBeenCalledWith("helper", message.operationId, "hi");
  });

  it("retries with the same operationId after a network failure", async () => {
    const send = vi.fn().mockRejectedValueOnce(new ApiError(0, "Failed to fetch")).mockResolvedValueOnce({ operationId: "x" });
    const message = draft("helper", "check foo_test");
    const failed = await deliver(message, send);
    expect(failed).toMatchObject({ status: "failed", error: "Failed to fetch", retryable: true });

    const again = retrying(failed);
    expect(again).toMatchObject({ status: "sending", error: null, operationId: message.operationId });
    const done = await deliver(again, send);
    expect(done.status).toBe("accepted");
    expect(send).toHaveBeenCalledTimes(2);
    expect(send.mock.calls[0][1]).toBe(message.operationId);
    expect(send.mock.calls[1][1]).toBe(message.operationId);
  });

  it("treats 5xx and 429 as retryable and 4xx as final", async () => {
    const fail = (status: number) => deliver(draft("a", "t"), vi.fn().mockRejectedValue(new ApiError(status, `e${status}`)));
    expect((await fail(502)).retryable).toBe(true);
    expect((await fail(429)).retryable).toBe(true);
    expect(await fail(410)).toMatchObject({ retryable: false, error: "e410" });
    expect((await fail(400)).retryable).toBe(false);
  });
});

describe("reconcile", () => {
  const sentAt = new Date("2026-10-25T14:00:00Z");
  const message = { ...draft("helper", "And the flaky one?", sentAt), status: "accepted" as const };

  it("drops accepted messages once the transcript carries them", () => {
    expect(reconcile([message], [{ kind: "human", text: "And the flaky one?", at: sentAt.getTime() + 500 }])).toEqual([]);
  });

  it("keeps them while the transcript does not", () => {
    expect(reconcile([message], [{ kind: "human", text: "something else", at: sentAt.getTime() }])).toEqual([message]);
    // Same text sent long before is a different message.
    expect(reconcile([message], [{ kind: "human", text: "And the flaky one?", at: sentAt.getTime() - 3_600_000 }])).toEqual([message]);
  });

  it("never drops sending or failed messages", () => {
    const failed = { ...message, status: "failed" as const };
    expect(reconcile([failed], [{ kind: "human", text: failed.text, at: sentAt.getTime() }])).toEqual([failed]);
  });
});
