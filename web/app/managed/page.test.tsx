import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fixtures } from "./lib/fixtures";
import ManagedPage from "./page";

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }
  addEventListener() {}
  close() {}
  emit(frame: unknown) {
    act(() => this.onmessage?.({ data: JSON.stringify(frame) }));
  }
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function stubApi(overrides: (url: string, init?: RequestInit) => Response | undefined = () => undefined) {
  const f = fixtures();
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      const override = overrides(url, init);
      if (override) return override;
      const path = url.replace("/api/v1/managed", "");
      if (path === "/me") return json(f.me);
      if (path === "/agents") return json({ items: f.agents });
      if (path === "/workspaces") return json({ items: f.workspaces });
      if (path === "/clients") return json(f.clients);
      if (/^\/agents\/[^/]+\/messages$/.test(path)) return json(f.messages);
      if (/^\/agents\/[^/]+\/runs$/.test(path)) return json({ items: f.runs });
      return json({ error: `unmocked ${url}` }, 404);
    }),
  );
  return calls;
}

beforeEach(() => {
  vi.stubGlobal("EventSource", FakeEventSource);
  Element.prototype.scrollTo = () => {}; // jsdom has no layout
});

afterEach(() => {
  FakeEventSource.instances = [];
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("Managed page", () => {
  it("lists agents by attention and hides retired ones", async () => {
    stubApi();
    render(<ManagedPage />);
    const list = await screen.findByRole("complementary");
    await waitFor(() => expect(within(list).getAllByRole("button", { name: /FAILED|WORKING|PARKED|IDLE/ }).length).toBe(5));
    const names = within(list).getAllByRole("button", { name: /FAILED|WORKING|PARKED|IDLE/ }).map((b) => b.textContent ?? "");
    expect(names[0]).toContain("nightly-digest");
    expect(names[1]).toContain("helper");
    expect(within(list).queryByText("old-experiment")).toBeNull();
    fireEvent.click(within(list).getByLabelText(/show retired/));
    expect(within(list).getByText("old-experiment")).toBeInTheDocument();
  });

  it("warns when no daemon is connected, and clears when one connects", async () => {
    const f = fixtures();
    stubApi((url) => (url.endsWith("/clients") ? json({ ...f.clients, gateway: [], outbox: 3 }) : undefined));
    render(<ManagedPage />);
    expect(await screen.findByText(/daemon is not connected/)).toHaveTextContent("3 results wait in the outbox");
    FakeEventSource.instances[0].emit({ type: "client", client: f.clients.gateway[0] });
    expect(screen.queryByText(/daemon is not connected/)).toBeNull();
  });

  it("retries a failed send with the same web- operationId", async () => {
    let posts = 0;
    const calls = stubApi((url, init) => {
      if (init?.method === "POST" && url.endsWith("/agents/helper/messages")) {
        posts += 1;
        return posts === 1 ? json({ error: "gateway timeout" }, 504) : json({ operationId: "ok" }, 202);
      }
    });
    window.location.hash = "#/agents/helper";
    render(<ManagedPage />);
    const box = await screen.findByLabelText("Message helper");
    fireEvent.change(box, { target: { value: "And the flaky one?" } });
    fireEvent.keyDown(box, { key: "Enter" });

    expect(await screen.findByText("gateway timeout")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));
    await waitFor(() => expect(posts).toBe(2));

    const bodies = calls.filter((c) => c.init?.method === "POST").map((c) => JSON.parse(String(c.init?.body)));
    expect(bodies[0].operationId).toMatch(/^web-/);
    expect(bodies[1].operationId).toBe(bodies[0].operationId);
    expect(bodies[1].text).toBe("And the flaky one?");
    await waitFor(() => expect(screen.queryByText("gateway timeout")).toBeNull());
  });

  it("renders peer input as untrusted and tool calls collapsed", async () => {
    stubApi();
    window.location.hash = "#/agents/helper";
    render(<ManagedPage />);
    expect(await screen.findByText("Untrusted peer")).toBeInTheDocument();
    const tool = screen.getByRole("button", { name: /workspace_exec/ });
    expect(tool).toHaveAttribute("aria-expanded", "false");
    expect(tool).toHaveTextContent("exit 1");
    expect(screen.queryByText(/go test/)).toBeNull();
    fireEvent.click(tool);
    expect(screen.getByText(/go test \.\/parser/)).toBeInTheDocument();
  });

  it("requires typing the id to retire", async () => {
    const f = fixtures();
    stubApi((_url, init) => (init?.method === "DELETE" ? json({ agent: { ...f.agents[1], state: "retired", retired_at: "x" } }) : undefined));
    window.location.hash = "#/agents/reviewer-hosted";
    render(<ManagedPage />);
    fireEvent.click(await screen.findByRole("button", { name: /retire/i }));
    const dialog = screen.getByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: "Retire agent" });
    expect(confirm).toBeDisabled();
    expect(dialog).toHaveTextContent("shared with helper and is kept");
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "reviewer-hosted" } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText(/Retired agents do not accept messages/)).toBeInTheDocument();
  });

  it("shows sign-in on 401", async () => {
    stubApi((url) => (url.endsWith("/me") ? json({ error: "unauthorized" }, 401) : undefined));
    render(<ManagedPage />);
    expect(await screen.findByText("Sign in to continue")).toBeInTheDocument();
  });
});
