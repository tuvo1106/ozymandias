import { describe, expect, it } from "vitest";
import { alignSeries, formatValue, OVERFLOW_COLOR, seriesColor, seriesLabel } from "./chartData";
import type { Series } from "./metricsApi";

const s = (points: [number, number | null][], tags: Record<string, string> = {}): Series => ({
  metric: "m",
  tags,
  points,
});

describe("alignSeries", () => {
  it("returns just an empty x axis for no series", () => {
    expect(alignSeries([])).toEqual([[]]);
  });

  it("converts ms to seconds and keeps server nulls", () => {
    expect(
      alignSeries([
        s([
          [10_000, 1],
          [20_000, null],
        ]),
      ]),
    ).toEqual([
      [10, 20],
      [1, null],
    ]);
  });

  it("aligns series on the union of timestamps, filling holes with null", () => {
    const got = alignSeries([
      s([
        [30_000, 3],
        [10_000, 1],
      ]),
      s([
        [20_000, 2],
        [30_000, 4],
      ]),
    ]);
    expect(got).toEqual([
      [10, 20, 30],
      [1, null, 3],
      [null, 2, 4],
    ]);
  });

  it("produces equal-length columns for any input", () => {
    let seed = 7;
    const rnd = (n: number) => (seed = (seed * 48_271) % 2_147_483_647) % n;
    for (let i = 0; i < 200; i++) {
      const series = Array.from({ length: rnd(5) }, () =>
        s(Array.from({ length: rnd(20) }, (): [number, number | null] => [rnd(50) * 10_000, rnd(3) ? rnd(100) : null])),
      );
      const [xs, ...ys] = alignSeries(series);
      expect(ys).toHaveLength(series.length);
      for (const y of ys) expect(y).toHaveLength(xs.length);
      expect([...xs].sort((a, b) => a - b)).toEqual(xs);
      expect(new Set(xs).size).toBe(xs.length);
    }
  });
});

describe("seriesLabel", () => {
  it("renders group tags sorted by key", () => {
    expect(seriesLabel({ metric: "http.request.count", tags: { route: "/a", env: "dev" } })).toBe(
      "http.request.count{env:dev,route:/a}",
    );
  });

  it("spells the whole selection as {*}", () => {
    expect(seriesLabel({ metric: "m", tags: {} })).toBe("m{*}");
  });
});

describe("formatValue", () => {
  it.each([
    [null, "—"],
    [undefined, "—"],
    [Number.NaN, "—"],
    [0, "0"],
    [0.1234, "0.123"],
    [-0.0001, "0"],
    [1.5, "1.5"],
    [12.345, "12.35"],
    [123.45, "123.5"],
    [9999, "9999"],
    [10_000, "10k"],
    [12_345, "12.35k"],
    [-2_500_000, "-2.5M"],
    [3e9, "3G"],
    [4.2e13, "42T"],
  ])("formats %s as %s", (v, want) => {
    expect(formatValue(v)).toBe(want);
  });
});

describe("seriesColor", () => {
  it("gives the first eight series distinct colours in both themes", () => {
    for (const dark of [false, true]) {
      const colours = Array.from({ length: 8 }, (_, i) => seriesColor(i, dark));
      expect(new Set(colours).size).toBe(8);
      expect(colours).not.toContain(dark ? OVERFLOW_COLOR.dark : OVERFLOW_COLOR.light);
    }
  });

  it("never cycles: series past the palette share the neutral overflow colour", () => {
    expect(seriesColor(8, false)).toBe(OVERFLOW_COLOR.light);
    expect(seriesColor(20, true)).toBe(OVERFLOW_COLOR.dark);
  });
});

describe("the first bucket is inside the chart's x range", () => {
  // The server floors the range start to the interval, so with from=1005 and
  // a 10s interval the first bucket is stamped 1000 — before result.from.
  // Using [result.from, result.to] as the uPlot scale clipped it, and at a
  // live "1h" range that is nearly every refresh.
  it("alignSeries reports a first timestamp at or before the requested from", () => {
    const data = alignSeries([
      { metric: "m", tags: {}, points: [[1000_000, 1], [1010_000, 2]] },
    ]);
    const firstX = data[0][0];
    expect(firstX).toBe(1000);
    expect(firstX).toBeLessThan(1005); // the `from` a caller would have asked for
  });
});
