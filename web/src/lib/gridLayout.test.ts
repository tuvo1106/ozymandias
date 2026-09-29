import { describe, expect, it } from "vitest";
import type { Layout, Widget } from "./dashboard";
import { cellDelta, clampLayout, moveBy, newWidgetLayout, overlaps, pushDown, resizeBy } from "./gridLayout";

const widget = (id: string, layout: Layout): Widget => ({ id, type: "note", layout, markdown: "x" });

/** A small deterministic PRNG, so a failing case is the same case next run. */
function rng(seed: number) {
  let s = seed >>> 0;
  return (n: number) => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s % n;
  };
}

describe("clampLayout", () => {
  it("keeps a widget on the grid, moving it left before narrowing it", () => {
    expect(clampLayout({ x: 10, y: 0, w: 4, h: 2 })).toEqual({ x: 8, y: 0, w: 4, h: 2 });
    expect(clampLayout({ x: -3, y: -1, w: 0, h: 0 })).toEqual({ x: 0, y: 0, w: 1, h: 1 });
    expect(clampLayout({ x: 0, y: 0, w: 40, h: 2 })).toEqual({ x: 0, y: 0, w: 12, h: 2 });
    expect(clampLayout({ x: 1.6, y: 2.4, w: 3.5, h: 1.2 })).toEqual({ x: 2, y: 2, w: 4, h: 1 });
  });
});

describe("moveBy / resizeBy", () => {
  it("moves without resizing, stopping at the edges", () => {
    expect(moveBy({ x: 8, y: 1, w: 4, h: 2 }, 3, -5)).toEqual({ x: 8, y: 0, w: 4, h: 2 });
    expect(moveBy({ x: 2, y: 1, w: 4, h: 2 }, -1, 2)).toEqual({ x: 1, y: 3, w: 4, h: 2 });
  });

  it("resizes from the corner, never moving the top-left", () => {
    expect(resizeBy({ x: 8, y: 1, w: 2, h: 2 }, 10, 1)).toEqual({ x: 8, y: 1, w: 4, h: 3 });
    expect(resizeBy({ x: 8, y: 1, w: 2, h: 2 }, -5, -5)).toEqual({ x: 8, y: 1, w: 1, h: 1 });
  });
});

describe("pushDown", () => {
  it("moves a widget the fixed one landed on to just below it", () => {
    const out = pushDown(
      [widget("a", { x: 0, y: 0, w: 6, h: 3 }), widget("b", { x: 3, y: 1, w: 6, h: 2 })],
      "b",
    );
    expect(out.map((w) => [w.id, w.layout])).toEqual([
      ["a", { x: 0, y: 3, w: 6, h: 3 }],
      ["b", { x: 3, y: 1, w: 6, h: 2 }],
    ]);
  });

  it("cascades: a pushed widget pushes what is under it", () => {
    const out = pushDown(
      [
        widget("drop", { x: 0, y: 0, w: 12, h: 2 }),
        widget("mid", { x: 0, y: 1, w: 6, h: 2 }),
        widget("low", { x: 0, y: 3, w: 6, h: 2 }),
      ],
      "drop",
    );
    expect(out.find((w) => w.id === "mid")?.layout.y).toBe(2);
    expect(out.find((w) => w.id === "low")?.layout.y).toBe(4);
  });

  it("returns the same objects where nothing moved, in the definition's order", () => {
    const ws = [widget("b", { x: 6, y: 0, w: 6, h: 2 }), widget("a", { x: 0, y: 0, w: 6, h: 2 })];
    const out = pushDown(ws, "a");
    expect(out[0]).toBe(ws[0]);
    expect(out[1]).toBe(ws[1]);
  });

  it("holds its invariants on random dashboards", () => {
    for (let seed = 1; seed <= 300; seed++) {
      const r = rng(seed);
      const n = 1 + r(12);
      const ws = Array.from({ length: n }, (_, i) =>
        widget(`w${i}`, clampLayout({ x: r(12), y: r(10), w: 1 + r(12), h: 1 + r(4) })),
      );
      const fixed = `w${r(n)}`;
      const out = pushDown(ws, fixed);
      const ctx = `seed ${seed}`;
      expect(out.map((w) => w.id), ctx).toEqual(ws.map((w) => w.id));
      for (let i = 0; i < out.length; i++) {
        const before = ws[i] as Widget;
        const after = out[i] as Widget;
        // Only y changes, and only downward; the fixed widget not at all.
        expect(after.layout.x, ctx).toBe(before.layout.x);
        expect(after.layout.w, ctx).toBe(before.layout.w);
        expect(after.layout.h, ctx).toBe(before.layout.h);
        expect(after.layout.y, ctx).toBeGreaterThanOrEqual(before.layout.y);
        if (after.id === fixed) expect(after.layout, ctx).toEqual(before.layout);
        for (let j = i + 1; j < out.length; j++) {
          expect(overlaps(after.layout, (out[j] as Widget).layout), `${ctx}: ${after.id}/${out[j]?.id}`).toBe(false);
        }
      }
    }
  });
});

describe("cellDelta", () => {
  const grid = { width: 1188, gap: 12, rowHeight: 64 }; // pitch 100 per column, 76 per row

  it("rounds to the nearest cell", () => {
    expect(cellDelta(149, 0, grid)).toEqual({ dx: 1, dy: 0 });
    expect(cellDelta(151, -114, grid)).toEqual({ dx: 2, dy: -2 });
    expect(cellDelta(-40, 37, grid)).toEqual({ dx: 0, dy: 0 });
  });

  it("moves nothing on a grid that has no width yet", () => {
    expect(cellDelta(500, 0, { width: 0, gap: 0, rowHeight: 64 })).toEqual({ dx: 0, dy: 0 });
  });
});

describe("newWidgetLayout", () => {
  it("puts a new widget below everything", () => {
    expect(newWidgetLayout([widget("a", { x: 6, y: 2, w: 6, h: 3 })], 6, 3)).toEqual({ x: 0, y: 5, w: 6, h: 3 });
    expect(newWidgetLayout([], 12, 1)).toEqual({ x: 0, y: 0, w: 12, h: 1 });
  });
});
