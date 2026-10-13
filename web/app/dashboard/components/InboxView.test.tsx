import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { InboxView } from "./InboxView";
import { useInbox } from "../lib/useInbox";
import type { InboxItem } from "../types";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const NOTIFY: InboxItem = {
  id: "inb-1",
  direction: "in",
  kind: "notify",
  from: "orchestrator",
  text: "PR is up: https://example.test/1",
  created_at: "2026-10-13T10:05:00Z",
  read: false,
  replied: false,
};

const ASK: InboxItem = {
  id: "inb-2",
  direction: "in",
  kind: "ask",
  from: "reviewer",
  text: "merge #12?",
  created_at: "2026-10-13T10:10:00Z",
  read: false,
  replied: false,
  correlation_id: "ask-2",
};

const CHOICE: InboxItem = {
  ...ASK,
  id: "inb-3",
  correlation_id: "ask-3",
  text: "ship it?",
  question: { kind: "choice", options: [{ id: "yes", title: "Yes" }, { id: "no", title: "No" }] },
};

const OLD_READ: InboxItem = { ...NOTIFY, id: "inb-0", text: "old news", read: true, created_at: "2026-10-12T09:00:00Z" };

function Harness({ signal = 0 }: { signal?: number }) {
  const inbox = useInbox("", signal);
  return <InboxView inbox={inbox} apiBase="" />;
}

/** fetch stub: GET /inbox returns items; every other call is recorded and answered with body. */
function stubFetch(items: InboxItem[], body: Record<string, unknown> = { ok: true }) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    if (url.startsWith("/inbox?")) return jsonResponse({ items });
    return jsonResponse(body);
  });
  vi.stubGlobal("fetch", fetchMock);
  return calls;
}

describe("InboxView", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("lists items newest first with kind, sender and unread state", async () => {
    stubFetch([ASK, NOTIFY, OLD_READ]);
    render(<Harness />);
    const list = await screen.findByRole("list", { name: "Inbox" });
    const rows = within(list).getAllByRole("button");
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("@reviewer"),
      expect.stringContaining("@orchestrator"),
      expect.stringContaining("old news"),
    ]);
    expect(rows[0]).toHaveTextContent("ask");
    expect(rows[1]).toHaveTextContent("message");
    expect(rows[0]).toHaveAttribute("data-unread", "true");
    expect(rows[2]).not.toHaveAttribute("data-unread");
    expect(screen.getByText("2")).toBeInTheDocument();
  });

  it("opening an item marks it read as the dashboard surface", async () => {
    const calls = stubFetch([NOTIFY]);
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: /@orchestrator/ }));
    await waitFor(() => expect(calls.some((c) => c.url === "/inbox/inb-1/read")).toBe(true));
    const read = calls.find((c) => c.url === "/inbox/inb-1/read")!;
    expect(read.init?.method).toBe("POST");
    expect(JSON.parse(String(read.init?.body))).toEqual({ surface: "dashboard" });
    expect(screen.getByRole("button", { name: /@orchestrator/ })).not.toHaveAttribute("data-unread");
    expect(screen.getByRole("link", { name: "https://example.test/1" })).toBeInTheDocument();
  });

  it("opening a read item does not mark it again", async () => {
    const calls = stubFetch([OLD_READ]);
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: /@orchestrator/ }));
    await screen.findByLabelText("Reply to @orchestrator");
    expect(calls.some((c) => c.url.endsWith("/read"))).toBe(false);
  });

  it("replies to an open ask through the inbox reply route", async () => {
    const calls = stubFetch([ASK], { ok: true, action: "acked", correlation_id: "ask-2" });
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: /@reviewer/ }));
    const box = await screen.findByLabelText("Reply to @reviewer");
    expect(box).toHaveAttribute("placeholder", expect.stringContaining("closes their ask"));
    fireEvent.change(box, { target: { value: "yes, merge" } });
    fireEvent.click(screen.getByRole("button", { name: "Send reply" }));
    await screen.findByText(/replied, ask closed/);
    const reply = calls.find((c) => c.url === "/inbox/inb-2/reply")!;
    expect(JSON.parse(String(reply.init?.body))).toEqual({ surface: "dashboard", text: "yes, merge" });
  });

  it("offers a choice question's options and answers with the option id", async () => {
    const calls = stubFetch([CHOICE], { ok: true, action: "answered" });
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: /@reviewer/ }));
    fireEvent.click(await screen.findByRole("button", { name: "No" }));
    await screen.findByText(/answered/);
    const reply = calls.find((c) => c.url === "/inbox/inb-3/reply")!;
    expect(JSON.parse(String(reply.init?.body))).toEqual({ surface: "dashboard", option_id: "no" });
  });

  it("shows the daemon's refusal instead of claiming the reply went out", async () => {
    const fetchMock = vi.fn(async (url: string) =>
      url.startsWith("/inbox?") ? jsonResponse({ items: [ASK] }) : jsonResponse({ detail: "ask already closed" }, 409),
    );
    vi.stubGlobal("fetch", fetchMock);
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: /@reviewer/ }));
    fireEvent.change(await screen.findByLabelText("Reply to @reviewer"), { target: { value: "ok" } });
    fireEvent.click(screen.getByRole("button", { name: "Send reply" }));
    expect(await screen.findByRole("status")).toHaveTextContent("ask already closed");
  });

  it("reloads when an inbox event arrives instead of polling", async () => {
    const calls = stubFetch([NOTIFY]);
    const { rerender } = render(<Harness signal={0} />);
    await screen.findByRole("list", { name: "Inbox" });
    const loads = () => calls.filter((c) => c.url.startsWith("/inbox?")).length;
    expect(loads()).toBe(1);
    rerender(<Harness signal={1} />);
    await waitFor(() => expect(loads()).toBe(2));
  });

  it("says so when the daemon has no inbox", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ detail: "human inbox not configured" }, 503)));
    render(<Harness />);
    expect(await screen.findByText("inbox unavailable")).toBeInTheDocument();
  });
});
