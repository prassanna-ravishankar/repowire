import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fixtures } from "../managed/lib/fixtures";
import AdminPage from "./page";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function stub(routes: Record<string, () => Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      const key = Object.keys(routes).find((k) => url.startsWith(k));
      return key ? routes[key]() : json({ error: "unmocked" }, 404);
    }),
  );
}

class NoopObserver {
  observe() {}
  disconnect() {}
}

afterEach(() => vi.unstubAllGlobals());

describe("Admin page", () => {
  const f = fixtures();
  const all = {
    "/api/v1/managed/me": () => json(f.me),
    "/api/v1/admin/summary": () => json(f.summary),
    "/api/v1/admin/owners": () => json({ items: f.owners }),
    "/api/v1/admin/sandboxes": () => json({ items: f.sandboxes }),
    "/api/v1/admin/costs": () => json(f.costs),
  };

  it("renders tiles, tables and the cost chart", async () => {
    vi.stubGlobal("ResizeObserver", NoopObserver);
    stub(all);
    render(<AdminPage />);
    expect(await screen.findByText("$11.84")).toBeInTheDocument();
    expect(screen.getAllByText("NEEDS REPAIR").length).toBeGreaterThan(0);
    expect(screen.getAllByText("token-81bd07").length).toBeGreaterThan(0);
    expect(screen.getByRole("img", { name: /Daily estimated cost for 30 days/ })).toBeInTheDocument();
    expect(screen.getByText("$15 alert")).toBeInTheDocument();
    expect(screen.getByText("$30 alert")).toBeInTheDocument();
  });

  it("shows not authorised for non-admins", async () => {
    stub({ ...all, "/api/v1/managed/me": () => json({ ...f.me, admin: false }) });
    render(<AdminPage />);
    expect(await screen.findByText("Not authorised")).toBeInTheDocument();
  });

  it("shows not authorised when an admin route answers 403", async () => {
    vi.stubGlobal("ResizeObserver", NoopObserver);
    stub({ ...all, "/api/v1/admin/summary": () => json({ error: "forbidden" }, 403) });
    render(<AdminPage />);
    expect(await screen.findByText("Not authorised")).toBeInTheDocument();
  });

  it("names the missing secret when actual spend is unconfigured", async () => {
    vi.stubGlobal("ResizeObserver", NoopObserver);
    stub({ ...all, "/api/v1/admin/costs": () => json({ ...f.costs, actual: null }) });
    render(<AdminPage />);
    expect(await screen.findByText(/REPOWIRE_CF_ANALYTICS_TOKEN/)).toBeInTheDocument();
  });
});
