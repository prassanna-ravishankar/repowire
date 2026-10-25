import { describe, expect, it } from "vitest";
import { alertLevel, barPath, cumulativeMonth, linePath, niceCeil, stackSegments, ticks } from "./costs";

const day = (d: string, model: number, container = 0, durable = 0) => ({
  day: d,
  model_usd: model,
  container_usd: container,
  do_usd: durable,
  total_usd: model + container + durable,
});

describe("cumulativeMonth", () => {
  it("sums day totals and resets on the first of a month", () => {
    const days = [day("2026-09-29", 1), day("2026-09-30", 2), day("2026-10-01", 0.5), day("2026-10-02", 0.25, 0.25)];
    expect(cumulativeMonth(days)).toEqual([1, 3, 0.5, 1]);
  });

  it("rounds to cents without drift", () => {
    const days = Array.from({ length: 10 }, (_, i) => day(`2026-10-${String(i + 1).padStart(2, "0")}`, 0.1));
    expect(cumulativeMonth(days).at(-1)).toBe(1);
  });

  it("handles an empty window", () => {
    expect(cumulativeMonth([])).toEqual([]);
  });
});

describe("axis scale", () => {
  it("rounds up to 1, 2 or 5 times a power of ten", () => {
    expect(niceCeil(0.62)).toBe(1);
    expect(niceCeil(0.18)).toBe(0.2);
    expect(niceCeil(3.1)).toBe(5);
    expect(niceCeil(30)).toBe(50);
    expect(niceCeil(0)).toBe(1);
  });

  it("splits into even ticks from zero", () => {
    expect(ticks(0.62, 4)).toEqual([0, 0.25, 0.5, 0.75, 1]);
    expect(ticks(0.37, 2)).toEqual([0, 0.25, 0.5]);
  });
});

describe("alertLevel", () => {
  it("bands a month-to-date total against the $15 and $30 alerts", () => {
    expect(alertLevel(11.84, [15, 30])).toBe("ok");
    expect(alertLevel(15, [30, 15])).toBe("warning");
    expect(alertLevel(30.01, [15, 30])).toBe("danger");
    expect(alertLevel(100, [])).toBe("ok");
  });
});

describe("stackSegments", () => {
  it("stacks services bottom-up with a 2px gap between fills", () => {
    const segs = stackSegments(day("2026-10-25", 0.5, 0.25, 0.25), 1, 100);
    expect(segs.map((s) => s.key)).toEqual(["model_usd", "container_usd", "do_usd"]);
    expect(segs[0]).toMatchObject({ y: 50, height: 50 });
    expect(segs[1]).toMatchObject({ y: 25, height: 23 });
    expect(segs[2]).toMatchObject({ y: 0, height: 23 });
  });

  it("skips zero and sub-pixel segments", () => {
    expect(stackSegments(day("d", 0.5, 0, 0.01), 1, 100).map((s) => s.key)).toEqual(["model_usd"]);
    expect(stackSegments(day("d", 0), 1, 100)).toEqual([]);
    expect(stackSegments(day("d", 1), 0, 100)).toEqual([]);
  });
});

describe("paths", () => {
  it("spaces line points evenly and scales to the plot", () => {
    expect(linePath([0, 5, 10], 10, 200, 100)).toBe("M0,100 L100,50 L200,0");
    expect(linePath([], 10, 200, 100)).toBe("");
  });

  it("rounds bar tops and keeps the base square", () => {
    expect(barPath(0, 0, 10, 20, 4)).toBe("M0,20V4Q0,0 4,0H6Q10,0 10,4V20Z");
    expect(barPath(0, 0, 10, 20, 0)).toBe("M0,0h10v20h-10Z");
    // Radius never exceeds half the width or the height.
    expect(barPath(0, 0, 4, 1, 4)).toBe("M0,1V1Q0,0 1,0H3Q4,0 4,1V1Z");
  });
});
