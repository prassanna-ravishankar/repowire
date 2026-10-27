import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fixtures } from "./lib/fixtures";
import ManagedPage from "./page";

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static get latest() {
    return FakeEventSource.instances[FakeEventSource.instances.length - 1];
  }
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  listeners = new Map<string, (e: { data: string }) => void>();
  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, fn: (e: { data: string }) => void) {
    this.listeners.set(type, fn);
  }
  close() {}
  open() {
    act(() => this.onopen?.());
  }
  fail() {
    act(() => this.onerror?.());
  }
  emit(frame: unknown) {
    act(() => this.onmessage?.({ data: JSON.stringify(frame) }));
  }
  relay(frame: unknown) {
    act(() => this.listeners.get("relay")?.({ data: JSON.stringify(frame) }));
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function stubApi(overrides: (url: string, init?: RequestInit) => Response | Promise<Response> | undefined = () => undefined) {
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
    // source is a mesh-delivery label object the Worker validates; web sends never set it.
    expect(Object.keys(bodies[0]).sort()).toEqual(["operationId", "text"]);
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

describe("Managed page live state", () => {
  const listed = (name: string) => within(screen.getByRole("complementary")).getByText(name).closest("button")!;

  it("does not let a slow HTTP bootstrap overwrite a newer stream frame", async () => {
    const f = fixtures();
    const agents = deferred<Response>();
    stubApi((url, init) => (url.endsWith("/api/v1/managed/agents") && !init?.method ? agents.promise : undefined));
    render(<ManagedPage />);
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
    const es = FakeEventSource.latest;
    es.open();
    // The stream says helper settled while the slow /agents read still says working.
    es.emit({ type: "agent", agent: { ...f.agents[0], state: "idle", state_at: new Date().toISOString() } });
    await act(async () => agents.resolve(json({ items: f.agents })));
    await waitFor(() => expect(listed("helper")).toHaveTextContent("IDLE"));
    expect(listed("helper")).not.toHaveTextContent("WORKING");
  });

  it("resyncs over HTTP on every reconnect", async () => {
    const f = fixtures();
    let agentReads = 0;
    stubApi((url, init) => {
      if (url.endsWith("/api/v1/managed/agents") && !init?.method) {
        agentReads += 1;
        return json({ items: agentReads === 1 ? f.agents : [...f.agents, { ...f.agents[4], id: "made-while-away" }] });
      }
    });
    render(<ManagedPage />);
    await screen.findByText("helper");
    FakeEventSource.latest.open();
    expect(agentReads).toBe(1);
    FakeEventSource.latest.fail();
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2), { timeout: 3000 });
    FakeEventSource.latest.open();
    await waitFor(() => expect(agentReads).toBe(2));
    expect(await screen.findByText("made-while-away")).toBeInTheDocument();
  });

  it("shows DEGRADED while the relay has lost the Worker, and resyncs when it returns", async () => {
    let agentReads = 0;
    stubApi((url, init) => {
      if (url.endsWith("/api/v1/managed/agents") && !init?.method) agentReads += 1;
      return undefined;
    });
    render(<ManagedPage />);
    await screen.findByText("helper");
    const es = FakeEventSource.latest;
    es.open();
    expect(screen.getByText("LIVE")).toBeInTheDocument();
    es.relay({ type: "gateway", connected: false, error: "upstream closed" });
    expect(screen.getByText("DEGRADED")).toHaveAttribute("title", expect.stringContaining("upstream closed"));
    es.relay({ type: "gateway", connected: true });
    expect(screen.getByText("LIVE")).toBeInTheDocument();
    await waitFor(() => expect(agentReads).toBe(2));
  });

  it("refetches open runs when a snapshot invalidates them", async () => {
    const f = fixtures();
    let runReads = 0;
    stubApi((url) => {
      if (url.endsWith("/agents/helper/runs")) {
        runReads += 1;
        return json({ items: runReads === 1 ? f.runs : [{ ...f.runs[0], status: "done", settled_at: new Date().toISOString() }, ...f.runs.slice(1)] });
      }
    });
    window.location.hash = "#/agents/helper/runs";
    render(<ManagedPage />);
    expect((await screen.findAllByText("WORKING")).length).toBeGreaterThan(0);
    await waitFor(() => expect(runReads).toBeGreaterThanOrEqual(1));
    const before = runReads;
    FakeEventSource.latest.emit({ type: "snapshot", agents: [{ ...f.agents[0], state: "idle" }], workspaces: f.workspaces, clients: f.clients.gateway, outbox: 0, at: "now" });
    await waitFor(() => expect(runReads).toBe(before + 1));
    await waitFor(() => expect(screen.getAllByText("ANSWERED").length).toBeGreaterThanOrEqual(2));
  });

  it("edits a managed agent's circle and shows the Worker's refusal verbatim", async () => {
    const patches: string[] = [];
    stubApi((url, init) => {
      if (init?.method !== "PATCH" || !url.endsWith("/agents/nightly-digest")) return undefined;
      const circle = JSON.parse(String(init.body)).circle as string;
      patches.push(circle);
      if (circle === "default") return json({ error: "circle default is machine-local (default, *, a number or window-N); name a shared circle" }, 422);
      return json({ agent: { ...fixtures().agents[2], circle } });
    });
    window.location.hash = "#/agents/nightly-digest";
    render(<ManagedPage />);
    fireEvent.click(await screen.findByRole("button", { name: "circle managed" }));
    const input = screen.getByLabelText("Circle");
    fireEvent.change(input, { target: { value: "default" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(await screen.findByRole("alert")).toHaveTextContent("machine-local");
    fireEvent.change(input, { target: { value: "research" } });
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(screen.queryByLabelText("Circle")).toBeNull());
    expect(patches).toEqual(["default", "research"]);
    // The saved agent is applied without any stream frame.
    expect(screen.getByRole("button", { name: "circle research" })).toBeInTheDocument();
    window.location.hash = "";
  });

  it("offers no circle for a daemon's agent", async () => {
    stubApi();
    window.location.hash = "#/agents/helper";
    render(<ManagedPage />);
    await screen.findAllByText(/home/);
    expect(screen.queryByRole("button", { name: /^circle / })).toBeNull();
    window.location.hash = "";
  });

  it("lists a managed agent's open cross-machine asks", async () => {
    let asksBody: unknown = { items: [{ id: "ask-1793000000-" + "a".repeat(32), to: "bob@laptop", state: "uncertain", close_reason: null, replied: false, created_at: new Date().toISOString() }] };
    stubApi((url) => (url.endsWith("/agents/nightly-digest/asks") ? json(asksBody) : undefined));
    window.location.hash = "#/agents/nightly-digest/asks";
    render(<ManagedPage />);
    expect(await screen.findByText("@bob@laptop")).toBeInTheDocument();
    expect(screen.getByText("outcome unknown")).toBeInTheDocument();
    // A settlement no run follows still moves asks_at; the tab reloads from that frame.
    asksBody = { items: [] };
    FakeEventSource.latest.emit({ type: "agent", agent: { ...fixtures().agents[2], asks_at: new Date().toISOString() } });
    expect(await screen.findByText("No open asks")).toBeInTheDocument();
    window.location.hash = "";
  });
});
