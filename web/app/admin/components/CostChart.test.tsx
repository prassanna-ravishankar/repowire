import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CostChart } from "./CostChart";

class NoopObserver {
  observe() {}
  disconnect() {}
}

afterEach(() => vi.unstubAllGlobals());

const est = (day: string, total: number) => ({ day, model_usd: total, container_usd: 0, do_usd: 0, total_usd: total });

describe("CostChart", () => {
  it("reads month to date from the current month of as_of, not the last row", () => {
    vi.stubGlobal("ResizeObserver", NoopObserver);
    render(<CostChart costs={{ estimate: { days: [est("2026-09-30", 20)], month_usd: 0 }, actual: null, alerts: [15, 30], as_of: "2026-10-01T00:30:00Z" }} />);
    expect(screen.getByRole("img", { name: /month to date \$0\.00/ })).toBeInTheDocument();
    expect(screen.getByText(/under the \$15 alert/)).toBeInTheDocument();
  });

  it("plots actual spend on days the estimate never saw", () => {
    vi.stubGlobal("ResizeObserver", NoopObserver);
    render(
      <CostChart
        costs={{
          estimate: { days: [est("2026-10-25", 1)], month_usd: 1 },
          actual: {
            month: { base_usd: 5, ai_usd: 14, containers_usd: 0, do_usd: 0, workers_usd: 0, total_usd: 19 },
            projected_usd: 24,
            days: [
              { day: "2026-10-24", ai_usd: 12, containers_usd: 0, do_usd: 0, workers_usd: 0, total_usd: 12 },
              { day: "2026-10-25", ai_usd: 2, containers_usd: 0, do_usd: 0, workers_usd: 0, total_usd: 2 },
            ],
            as_of: "2026-10-25T12:00:00Z",
          },
          alerts: [15, 30],
          as_of: "2026-10-25T13:00:00Z",
        }}
      />,
    );
    expect(screen.getByText(/Actual MTD \$19\.00/)).toBeInTheDocument();
    expect(screen.getByText(/actual past the \$15 alert/)).toBeInTheDocument();
  });
});
