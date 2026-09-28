import { describe, expect, it } from "vitest";
import {
  COLUMNS,
  gridArea,
  gridRows,
  matchConditionalFormat,
  reduceSeries,
  widgetsInReadingOrder,
  type Reducer,
  type Widget,
} from "./dashboard";

function widget(id: string, x: number, y: number, w = 3, h = 2): Widget {
  return { id, type: "timeseries", layout: { x, y, w, h } };
}

describe("widgetsInReadingOrder", () => {
  it("orders top to bottom, then left to right", () => {
    const widgets = [widget("c", 0, 4), widget("b", 6, 0), widget("a", 0, 0)];
    expect(widgetsInReadingOrder(widgets).map((w) => w.id)).toEqual(["a", "b", "c"]);
  });

  it("does not reorder the caller's array", () => {
    const widgets = [widget("b", 6, 0), widget("a", 0, 0)];
    widgetsInReadingOrder(widgets);
    expect(widgets.map((w) => w.id)).toEqual(["b", "a"]);
  });

  // Two widgets can legitimately share a cell while one is being dragged over
  // another; the order still has to be stable or React remounts them.
  it("breaks a tie on id so the order is stable", () => {
    const a = widgetsInReadingOrder([widget("z", 0, 0), widget("a", 0, 0)]).map((w) => w.id);
    const b = widgetsInReadingOrder([widget("a", 0, 0), widget("z", 0, 0)]).map((w) => w.id);
    expect(a).toEqual(b);
  });
});

describe("gridArea", () => {
  it("converts 0-based cells to 1-based grid lines", () => {
    expect(gridArea({ x: 0, y: 0, w: 6, h: 3 })).toEqual({ gridColumn: "1 / span 6", gridRow: "1 / span 3" });
    expect(gridArea({ x: 6, y: 2, w: 6, h: 1 })).toEqual({ gridColumn: "7 / span 6", gridRow: "3 / span 1" });
  });

  // The server refuses these, so they only arrive from an unsaved paste — but
  // a widget drawn half off-screen beats one that breaks the grid under it.
  it("clamps a layout that would spill past the grid", () => {
    expect(gridArea({ x: 10, y: 0, w: 6, h: 2 }).gridColumn).toBe(`${COLUMNS - 6 + 1} / span 6`);
    expect(gridArea({ x: -3, y: -3, w: 99, h: 0 })).toEqual({ gridColumn: "1 / span 12", gridRow: "1 / span 1" });
  });
});

describe("gridRows", () => {
  it("is the lowest edge of any widget", () => {
    expect(gridRows([widget("a", 0, 0, 3, 2), widget("b", 3, 3, 3, 4)])).toBe(7);
  });

  it("is zero for an empty dashboard", () => {
    expect(gridRows([])).toBe(0);
  });
});

describe("reduceSeries", () => {
  const points = [1, null, 5, 3];

  it("reduces each way over the non-null points", () => {
    expect(reduceSeries(points, "last")).toBe(3);
    expect(reduceSeries(points, "sum")).toBe(9);
    expect(reduceSeries(points, "avg")).toBe(3);
    expect(reduceSeries(points, "min")).toBe(1);
    expect(reduceSeries(points, "max")).toBe(5);
  });

  // The bug this prevents: an empty bucket is "not measured", and counting it
  // as a zero drags an average toward the floor on exactly the sparse series
  // where nobody would notice.
  it("skips nulls rather than treating them as zero", () => {
    expect(reduceSeries([4, null, null, 4], "avg")).toBe(4);
    expect(reduceSeries([4, null, null, 4], "min")).toBe(4);
  });

  // "last" means the last thing measured, not the last bucket, which is
  // usually empty while the current one is still filling.
  it("takes the last value that exists, not the last bucket", () => {
    expect(reduceSeries([1, 2, null], "last")).toBe(2);
  });

  it("is null for a line with nothing in it", () => {
    for (const r of ["last", "avg", "sum", "min", "max"] as const) {
      expect(reduceSeries([null, null], r)).toBeNull();
      expect(reduceSeries([], r)).toBeNull();
    }
  });

  // NaN and Infinity reach the UI as JSON nulls, but a hand-built definition
  // or a future encoder change should not turn a chart into NaN.
  it("ignores values that are not finite", () => {
    expect(reduceSeries([1, NaN, 3], "sum")).toBe(4);
    expect(reduceSeries([Infinity], "max")).toBeNull();
  });
});

// `Reducer` is this bundle's idea of the set and a stored definition is served
// back without being re-validated, so a hand-edited row or a newer ozyd
// delivers one this build has never heard of. Falling off the end of the
// switch returns undefined, which the signature says is impossible and the
// first `.toFixed` on it turns into a blank application.
describe("reduceSeries with a reducer this build does not know", () => {
  it("is null, not undefined", () => {
    const unknown = "median" as Reducer;
    expect(reduceSeries([1, 2, 3], unknown)).toBeNull();
  });
});

describe("matchConditionalFormat", () => {
  const formats = [
    { op: ">=" as const, value: 5, color: "red" },
    { op: ">=" as const, value: 1, color: "yellow" },
    { op: ">=" as const, value: 0, color: "green" },
  ];

  it("takes the first match, so the rules read as a cascade", () => {
    expect(matchConditionalFormat(9, formats)?.color).toBe("red");
    expect(matchConditionalFormat(2, formats)?.color).toBe("yellow");
    expect(matchConditionalFormat(0, formats)?.color).toBe("green");
  });

  it("matches every comparison", () => {
    expect(matchConditionalFormat(1, [{ op: "<", value: 2, color: "a" }])?.color).toBe("a");
    expect(matchConditionalFormat(2, [{ op: "<=", value: 2, color: "a" }])?.color).toBe("a");
    expect(matchConditionalFormat(2, [{ op: "=", value: 2, color: "a" }])?.color).toBe("a");
    expect(matchConditionalFormat(3, [{ op: "!=", value: 2, color: "a" }])?.color).toBe("a");
    expect(matchConditionalFormat(3, [{ op: ">", value: 2, color: "a" }])?.color).toBe("a");
    expect(matchConditionalFormat(1, [{ op: ">", value: 2, color: "a" }])).toBeUndefined();
  });

  // A widget with no data is not a widget that is green.
  it("colours nothing when there is no value", () => {
    expect(matchConditionalFormat(null, formats)).toBeUndefined();
    expect(matchConditionalFormat(1, undefined)).toBeUndefined();
  });
});
