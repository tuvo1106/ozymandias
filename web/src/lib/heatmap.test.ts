import { describe, expect, it } from "vitest";
import {
  axisRange,
  cellsFor,
  heatColor,
  wantsLogAxis,
  MIN_CELL_HEIGHT,
  offAxisCount,
  primarySeries,
  relativeAccuracy,
  unboundedCount,
  valueExtent,
} from "./heatmap";
import type { SketchBin, SketchBucket, SketchSeries } from "./dashboardsApi";

function bucket(t: number, bins: SketchBin[], gamma = 1.02): SketchBucket {
  return { t, gamma, count: bins.reduce((n, b) => n + b[2], 0), sum: null, min: null, max: null, bins };
}

/** 1px per second, so a 60s bucket is 60px wide. */
const toX = (tMs: number) => tMs / 1000;

/** A linear value axis: 0 at the bottom of a 100px canvas, 100 at the top. */
const linearY = (v: number) => 100 - v;

/** A log value axis; a value at or below zero has no position on it. */
const logY = (v: number) => 100 - Math.log10(v) * 20;

describe("valueExtent", () => {
  it("spans the lowest lower bound and the highest upper bound", () => {
    expect(valueExtent([bucket(0, [[1, 2, 3]]), bucket(1000, [[8, 16, 1]])])).toEqual({ lo: 1, hi: 16 });
  });

  it("ignores bins nothing landed in, which would widen the axis for no data", () => {
    expect(valueExtent([bucket(0, [[0.001, 0.002, 0], [4, 8, 2]])])).toEqual({ lo: 4, hi: 8 });
  });

  it("leaves out what a log axis cannot hold, when asked", () => {
    const buckets = [bucket(0, [[-8, -4, 2], [0, 0, 5], [4, 8, 2]])];
    expect(valueExtent(buckets)).toEqual({ lo: -8, hi: 8 });
    expect(valueExtent(buckets, true)).toEqual({ lo: 4, hi: 8 });
  });

  it("is undefined when there is nothing to draw", () => {
    expect(valueExtent([])).toBeUndefined();
    expect(valueExtent([bucket(0, [])])).toBeUndefined();
  });
});

describe("axisRange", () => {
  it("prefers the author's bounds over the data's", () => {
    expect(axisRange([bucket(0, [[1, 2, 3]])], { min: 0, max: 100 })).toEqual({ lo: 0, hi: 100 });
  });

  it("takes one bound from the author and the other from the data", () => {
    expect(axisRange([bucket(0, [[1, 2, 3]])], { min: 0 })).toEqual({ lo: 0, hi: 2 });
  });

  it("widens an axis a single-valued distribution would collapse", () => {
    const range = axisRange([bucket(0, [[5, 5, 9]])], undefined);
    expect(range?.lo).toBeLessThan(5);
    expect(range?.hi).toBeGreaterThan(5);
  });

  it("widens by a constant when the single value is zero, since 10% of it is not a width", () => {
    expect(axisRange([bucket(0, [[0, 0, 9]])], undefined)).toEqual({ lo: -1, hi: 1 });
  });

  it("is undefined with no data and no author bounds", () => {
    expect(axisRange([], undefined)).toBeUndefined();
  });

  it("is undefined for a log axis with nothing positive on it", () => {
    expect(axisRange([bucket(0, [[0, 0, 7]])], { scale: "log" }, true)).toBeUndefined();
  });
});

describe("wantsLogAxis", () => {
  it("is on only when the author asked for it", () => {
    expect(wantsLogAxis({ scale: "log" })).toBe(true);
    expect(wantsLogAxis({ scale: "linear" })).toBe(false);
    expect(wantsLogAxis(undefined)).toBe(false);
  });

  it("refuses a log axis pinned to start at or below zero, which has no log", () => {
    expect(wantsLogAxis({ scale: "log", min: 0 })).toBe(false);
    expect(wantsLogAxis({ scale: "log", min: -5 })).toBe(false);
    expect(wantsLogAxis({ scale: "log", min: 0.001 })).toBe(true);
  });

  // Data reaching zero does not turn the axis linear. One zero-valued
  // observation would otherwise flatten a three-decade latency chart into a
  // line along the bottom, silently.
  it("does not depend on the data", () => {
    const zeroed = [bucket(0, [[0, 0, 7], [1, 10, 3]])];
    expect(wantsLogAxis({ scale: "log" })).toBe(true);
    expect(axisRange(zeroed, { scale: "log" }, true)).toEqual({ lo: 1, hi: 10 });
    expect(offAxisCount(zeroed, true)).toBe(7);
  });
});

describe("cellsFor", () => {
  it("places a bin as the rectangle its bounds and its bucket describe", () => {
    const { cells, max, offAxis } = cellsFor([bucket(0, [[10, 20, 5]])], 60_000, toX, linearY);
    expect(cells).toEqual([{ x: 0, y: 80, w: 60, h: 10, count: 5 }]);
    expect(max).toBe(5);
    expect(offAxis).toBe(0);
  });

  it("takes a column's width from the interval, not from the gap to the next bucket", () => {
    // Two buckets 10 minutes apart on a 60s interval: nothing was recorded in
    // between, and the gap is the point.
    const { cells } = cellsFor([bucket(0, [[1, 2, 1]]), bucket(600_000, [[1, 2, 1]])], 60_000, toX, linearY);
    expect(cells.map((c) => [c.x, c.w])).toEqual([
      [0, 60],
      [600, 60],
    ]);
  });

  it("gives a bin thinner than a pixel a floor, so it is visible at all", () => {
    const { cells } = cellsFor([bucket(0, [[10, 10.0001, 3]])], 60_000, toX, linearY);
    expect(cells[0]?.h).toBe(MIN_CELL_HEIGHT);
    expect(MIN_CELL_HEIGHT).toBe(1);
  });

  it("adds up the bins that collapse onto one band instead of overdrawing them", () => {
    // Three disjoint ranges, all within a pixel of each other. Drawing them
    // one over the other would show the last one's 4 and lose the 3 and the 2.
    const flat = (v: number) => 100 - v * 0.1;
    const { cells, max } = cellsFor([bucket(0, [[1, 2, 3], [2, 3, 4], [3, 4, 2]])], 60_000, toX, flat);
    expect(cells).toHaveLength(1);
    expect(cells[0]?.count).toBe(9);
    expect(max).toBe(9);
  });

  it("keeps contiguous bins apart, however many of them there are", () => {
    // A floor of two pixels instead of one made every bin's rectangle reach a
    // pixel into the one below it, so the merge cascaded and thirty bins came
    // back as one cell holding the whole bucket. This is that regression.
    const bins: SketchBin[] = Array.from({ length: 30 }, (_, i) => [i, i + 1, i + 1]);
    const { cells } = cellsFor([bucket(0, bins)], 60_000, toX, linearY);
    expect(cells).toHaveLength(30);
    expect(cells.every((c) => c.h === 1)).toBe(true);
    expect(cells.map((c) => c.count)).toEqual(bins.map((b) => b[2]));
  });

  it("does not merge two bins that only touch at a bound", () => {
    const { cells } = cellsFor([bucket(0, [[10, 11, 1], [11, 30, 1]])], 60_000, toX, linearY);
    expect(cells).toEqual([
      { x: 0, y: 89, w: 60, h: 1, count: 1 },
      { x: 0, y: 70, w: 60, h: 19, count: 1 },
    ]);
  });

  it("does not merge across buckets, however close two columns' values are", () => {
    const { cells } = cellsFor([bucket(0, [[1, 2, 3]]), bucket(60_000, [[1, 2, 4]])], 60_000, toX, linearY);
    expect(cells).toHaveLength(2);
    expect(cells.map((c) => c.count)).toEqual([3, 4]);
  });

  it("skips a bin nothing landed in", () => {
    const { cells } = cellsFor([bucket(0, [[1, 2, 0]])], 60_000, toX, linearY);
    expect(cells).toEqual([]);
  });

  it("counts out what the axis cannot hold rather than dropping it silently", () => {
    const { cells, offAxis } = cellsFor([bucket(0, [[0, 0, 9], [1, 10, 4]])], 60_000, toX, logY);
    expect(offAxis).toBe(9);
    expect(cells).toHaveLength(1);
    expect(cells[0]?.count).toBe(4);
  });

  it("reports nothing for a series with no buckets", () => {
    expect(cellsFor([], 60_000, toX, linearY)).toEqual({ cells: [], offAxis: 0, max: 0 });
  });
});

describe("offAxisCount", () => {
  const buckets = [bucket(0, [[-4, -2, 3], [0, 0, 9], [1, 10, 4]])];

  it("agrees with what cellsFor could not place on a log axis", () => {
    expect(offAxisCount(buckets, true)).toBe(cellsFor(buckets, 60_000, toX, logY).offAxis);
    expect(offAxisCount(buckets, true)).toBe(12);
  });

  it("is zero on a linear axis, which has a position for every value", () => {
    expect(offAxisCount(buckets, false)).toBe(0);
    expect(cellsFor(buckets, 60_000, toX, linearY).offAxis).toBe(0);
  });
});

describe("a bin whose bounds are past what a number can hold", () => {
  // The server writes such a bound as null and the client reads it as NaN,
  // which is reachable: a bucket index near the wire format's limit overflows
  // gamma^k. Refusing the bin would have thrown away the whole distribution.
  // Either bound, not just the lower one — and the upper is the realistic
  // case, since gamma^k overflows at the top of the index range first.
  const buckets = [bucket(0, [[1, 10, 4], [1e307, NaN, 3], [NaN, NaN, 2]])];

  it("cannot widen the axis, on either scale", () => {
    expect(valueExtent(buckets)).toEqual({ lo: 1, hi: 10 });
    expect(valueExtent(buckets, true)).toEqual({ lo: 1, hi: 10 });
  });

  it("is counted out rather than drawn somewhere wrong", () => {
    expect(unboundedCount(buckets)).toBe(5);
    expect(cellsFor(buckets, 60_000, toX, linearY).offAxis).toBe(5);
    expect(cellsFor(buckets, 60_000, toX, linearY).cells).toHaveLength(1);
  });

  it("is not confused with a zero a log axis cannot show", () => {
    // Different reasons, different sentences: one is a shape the axis cannot
    // hold, the other a number the format cannot hold.
    expect(offAxisCount(buckets, true)).toBe(0);
    expect(unboundedCount([bucket(0, [[0, 0, 9]])])).toBe(0);
  });

  it("is ignored when nothing landed in it", () => {
    expect(unboundedCount([bucket(0, [[NaN, NaN, 0]])])).toBe(0);
  });
});

describe("heatColor", () => {
  it("puts an empty cell at the bottom of the ramp and the fullest at the top", () => {
    expect(heatColor(0, 100)).toBe("rgb(68, 1, 84)");
    expect(heatColor(100, 100)).toBe("rgb(253, 231, 37)");
  });

  it("never goes backwards as the count rises", () => {
    const lightness = (n: number) => {
      const [r, g, b] = (heatColor(n, 1000).match(/\d+/g) ?? []).map(Number) as [number, number, number];
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };
    const steps = [0, 1, 5, 20, 100, 500, 1000].map(lightness);
    for (let i = 1; i < steps.length; i++) expect(steps[i]).toBeGreaterThan(steps[i - 1] as number);
  });

  it("separates the tail from the mode, which a linear ramp would not", () => {
    // One observation against a mode of a million: on a linear scale that is
    // 0.0001% of the way up the ramp and indistinguishable from empty.
    expect(heatColor(1, 1_000_000)).not.toBe(heatColor(0, 1_000_000));
  });

  it("treats a chart whose fullest cell holds one observation as full", () => {
    expect(heatColor(1, 1)).toBe("rgb(253, 231, 37)");
    expect(heatColor(1, 0)).toBe("rgb(253, 231, 37)");
  });

  it("clamps a count past the maximum rather than running off the ramp", () => {
    expect(heatColor(500, 10)).toBe(heatColor(10, 10));
  });
});

describe("relativeAccuracy", () => {
  it("is the worst bucket's, whichever order they arrive in", () => {
    // Both orders, because with the worst last "the largest" and "the last
    // one" are the same number and the test would pin neither.
    expect(relativeAccuracy([bucket(0, [], 1.02), bucket(1, [], 1.5)])).toBeCloseTo(0.2, 10);
    expect(relativeAccuracy([bucket(0, [], 1.5), bucket(1, [], 1.02)])).toBeCloseTo(0.2, 10);
  });

  it("is zero when no bucket says, and ignores a gamma that cannot be one", () => {
    expect(relativeAccuracy([])).toBe(0);
    expect(relativeAccuracy([bucket(0, [], 1), bucket(1, [], 0)])).toBe(0);
  });
});

describe("primarySeries", () => {
  const series = (scope: string): SketchSeries => ({ metric: "lat", tags: {}, scope, buckets: [] });

  it("draws the first and counts the rest", () => {
    const { series: first, others } = primarySeries([series("a"), series("b"), series("c")]);
    expect(first?.scope).toBe("a");
    expect(others).toBe(2);
  });

  it("has nothing to draw and nothing to report for an empty answer", () => {
    expect(primarySeries([])).toEqual({ series: undefined, others: 0 });
  });
});
