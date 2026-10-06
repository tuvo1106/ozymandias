import { FULL_VIEW, formatDuration, hitTest, layoutTrace, missingId, panView, serviceColor, viewOf, zoomView, type FlameRect, type FlameSpan } from "./flame";

let n = 0;
/** A span; times are in microseconds relative to 0 so expectations read as plain numbers. */
function sp(id: string, parent: string | null, start: number, duration: number, extra: Partial<FlameSpan> = {}): FlameSpan {
  n++;
  return { span_id: id, parent_id: parent, service: "svc", name: `op${n}`, resource: "r", start, duration, error: 0, ...extra };
}
/** Rects by id: a lookup that fails loudly (the tsconfig makes a plain index possibly undefined). */
function byId(rs: FlameRect[]): ((id: string) => FlameRect) & { has: (id: string) => boolean } {
  const m = new Map(rs.map((r) => [r.id, r]));
  return Object.assign(
    (id: string) => {
      const r = m.get(id);
      if (!r) throw new Error(`no rect ${id}`);
      return r;
    },
    { has: (id: string) => m.has(id) },
  );
}

describe("layoutTrace", () => {
  it("lays one span across the whole trace", () => {
    const l = layoutTrace([sp("a", null, 100, 50)]);
    expect(l.rects).toHaveLength(1);
    expect(l.rects[0]).toMatchObject({ row: 0, level: 0, x: 0, w: 1, critical: true, clamped: false });
    expect(l.duration).toBe(50);
    expect(l.rows).toBe(1);
  });

  it("returns an empty layout for no spans", () => {
    const l = layoutTrace([]);
    expect(l.rects).toEqual([]);
    expect(l.rows).toBe(0);
  });

  it("expresses x and width as fractions of the trace and puts sequential siblings on one row", () => {
    const l = layoutTrace([sp("r", null, 0, 100), sp("b", "r", 50, 50), sp("a", "r", 0, 50)]);
    const r = byId(l.rects);
    expect(r("a")).toMatchObject({ row: 1, x: 0, w: 0.5 });
    expect(r("b")).toMatchObject({ row: 1, x: 0.5, w: 0.5 });
    expect(l.rects.map((x) => x.id)).toEqual(["r", "a", "b"]); // siblings sorted by start
  });

  it("stacks overlapping siblings in lanes and pushes the next level below the whole lane", () => {
    const l = layoutTrace([
      sp("r", null, 0, 100),
      sp("a", "r", 0, 60),
      sp("b", "r", 10, 60), // overlaps a
      sp("a1", "a", 5, 10),
      sp("a2", "a", 20, 10),
      sp("c", "r", 70, 30), // fits back into lane 0
    ]);
    const r = byId(l.rects);
    expect(r("a").row).toBe(1);
    expect(r("b").row).toBe(3); // lane 1 starts below lane 0's tallest subtree (a + a1/a2 = 2 rows)
    expect(r("c").row).toBe(1);
    expect(r("a1").row).toBe(2);
    // No two rects occupy the same cell.
    for (const x of l.rects) {
      for (const o of l.rects) {
        if (o !== x && o.row === x.row) expect(o.x >= x.x + x.w || x.x >= o.x + o.w).toBe(true);
      }
    }
    expect(l.rows).toBe(Math.max(...l.rects.map((x) => x.row)) + 1);
  });

  it("hangs an orphan under a synthetic missing-parent node, one per missing parent", () => {
    const l = layoutTrace([sp("r", null, 0, 100), sp("o1", "gone", 10, 20), sp("o2", "gone", 40, 20), sp("o3", "other", 5, 5)]);
    const r = byId(l.rects);
    expect(l.missing).toBe(2);
    expect(r(missingId("gone"))).toMatchObject({ synthetic: true, childCount: 2, x: 0.1, w: 0.5 });
    expect(r("o1").parentId).toBe(missingId("gone"));
    expect(r("o1").level).toBe(1);
    expect(l.rects.filter((x) => x.synthetic)).toHaveLength(2);
  });

  it("keeps a span whose parent is itself, or in a cycle, instead of losing it", () => {
    const l = layoutTrace([sp("s", "s", 0, 10), sp("a", "b", 0, 10), sp("b", "a", 2, 5)]);
    expect(l.rects.map((x) => x.id).sort()).toEqual(["a", "b", "s", missingId("b"), missingId("s")].sort());
  });

  it("keeps the first of two spans sharing an id", () => {
    const l = layoutTrace([sp("a", null, 0, 10), sp("a", null, 5, 99)]);
    expect(l.rects).toHaveLength(1);
    expect(l.rects[0]!.w).toBe(1);
  });

  it("clamps a skewed child into its parent and says so", () => {
    const l = layoutTrace([sp("r", null, 100, 100), sp("early", "r", 90, 30), sp("late", "r", 180, 50), sp("out", "r", 300, 10), sp("ok", "r", 120, 10)]);
    const r = byId(l.rects);
    expect(r("early")).toMatchObject({ start: 100, end: 120, clamped: true });
    expect(r("late")).toMatchObject({ start: 180, end: 200, clamped: true });
    expect(r("out")).toMatchObject({ start: 200, end: 200, w: 0, clamped: true }); // wholly outside: zero-width at the edge
    expect(r("ok").clamped).toBe(false);
    expect(l.clamped).toBe(3);
    for (const x of l.rects) {
      expect(x.x).toBeGreaterThanOrEqual(0);
      expect(x.x + x.w).toBeLessThanOrEqual(1 + 1e-12);
    }
  });

  it("clamps grandchildren against the clamped child, not the raw one", () => {
    const l = layoutTrace([sp("r", null, 0, 100), sp("c", "r", 50, 100), sp("g", "c", 60, 100)]);
    const r = byId(l.rects);
    expect(r("c").end).toBe(100);
    expect(r("g").end).toBe(100);
    expect(r("g").clamped).toBe(true);
  });

  it("draws a zero-duration span as a zero-width rect that still takes part", () => {
    const l = layoutTrace([sp("r", null, 0, 10), sp("z", "r", 5, 0), sp("z2", "r", 5, 0)]);
    const r = byId(l.rects);
    expect(r("z").w).toBe(0);
    expect(r("z").row).toBe(1);
    expect(r("z2").row).toBe(1); // 0-width spans at one instant do not overlap
    expect(layoutTrace([sp("only", null, 7, 0)]).rects[0]).toMatchObject({ w: 0, x: 0 }); // duration floors at 1, no NaN
  });

  it("marks the chain of last-finishing children as the critical path", () => {
    const l = layoutTrace([
      sp("r", null, 0, 100),
      sp("fast", "r", 0, 20),
      sp("slow", "r", 10, 90), // finishes last
      sp("s1", "slow", 10, 30),
      sp("s2", "slow", 40, 60), // finishes last under slow
      sp("x", "fast", 0, 5),
    ]);
    const crit = l.rects.filter((x) => x.critical).map((x) => x.id);
    expect(crit.sort()).toEqual(["r", "s2", "slow"]);
  });

  it("breaks a tie on end time by the longer span", () => {
    const l = layoutTrace([sp("r", null, 0, 100), sp("short", "r", 80, 20), sp("long", "r", 20, 80)]);
    expect(l.rects.filter((x) => x.critical).map((x) => x.id).sort()).toEqual(["long", "r"]);
  });

  it("folds a subtree into its parent and reports what it hid, shifting rows up", () => {
    const spans = [sp("r", null, 0, 100), sp("a", "r", 0, 40), sp("a1", "a", 0, 10), sp("a2", "a", 10, 10), sp("b", "r", 50, 40), sp("b1", "b", 50, 10)];
    const l = layoutTrace(spans, { collapsed: new Set(["a"]) });
    const r = byId(l.rects);
    expect(r.has("a1")).toBe(false);
    expect(r("a")).toMatchObject({ collapsed: true, hidden: 2, childCount: 2 });
    expect(r.has("b1")).toBe(true);
    expect(layoutTrace(spans).rects).toHaveLength(6);
  });

  it("folds overlapping lanes more tightly than the open tree", () => {
    const spans = [sp("r", null, 0, 100), sp("a", "r", 0, 60), sp("a1", "a", 0, 10), sp("b", "r", 10, 60)];
    expect(byId(layoutTrace(spans).rects)("b").row).toBe(3);
    expect(byId(layoutTrace(spans, { collapsed: new Set(["a"]) }).rects)("b").row).toBe(2);
  });

  it("shows the queue wait between arq.enqueue and its arq.job as a gap, and does not clamp the job", () => {
    const l = layoutTrace([
      sp("h", null, 0, 100, { name: "http.request" }),
      sp("e", "h", 10, 10, { name: "arq.enqueue" }),
      sp("j", "e", 60, 30, { name: "arq.job", service: "worker" }),
      sp("q", "j", 62, 5, { name: "postgres.query" }),
    ]);
    const r = byId(l.rects);
    expect(r("j")).toMatchObject({ start: 60, end: 90, clamped: false });
    expect(l.gaps).toEqual([{ jobId: "j", enqueueId: "e", row: r("j").row, x: 0.2, w: 0.4, waitUs: 40 }]);
    expect(r("q").clamped).toBe(false);
    expect(l.clamped).toBe(0);
  });

  it("draws no gap when the job starts inside its enqueue, and clamps nothing else for jobs elsewhere", () => {
    const l = layoutTrace([sp("e", null, 0, 50, { name: "arq.enqueue" }), sp("j", "e", 10, 10, { name: "arq.job" })]);
    expect(l.gaps).toEqual([]);
    const other = layoutTrace([sp("p", null, 0, 10, { name: "http.request" }), sp("j", "p", 50, 10, { name: "arq.job" })]);
    expect(other.gaps).toEqual([]);
    expect(byId(other.rects)("j").clamped).toBe(true);
  });

  it("lays out 5000 spans in under 50ms", () => {
    const spans: FlameSpan[] = [sp("0", null, 0, 1_000_000)];
    for (let i = 1; i < 5000; i++) spans.push(sp(String(i), String(Math.floor((i - 1) / 3)), (i * 37) % 900_000, 1000 + ((i * 13) % 5000)));
    layoutTrace(spans); // warm the JIT: the budget is for the steady state
    const t = performance.now();
    const l = layoutTrace(spans);
    const took = performance.now() - t;
    expect(l.rects).toHaveLength(5000);
    expect(took).toBeLessThan(50);
  });

  it("handles a 5000-deep chain without overflowing the stack", () => {
    const spans: FlameSpan[] = [];
    for (let i = 0; i < 5000; i++) spans.push(sp(`d${i}`, i === 0 ? null : `d${i - 1}`, i, 5000 - i));
    const l = layoutTrace(spans);
    expect(l.rows).toBe(5000);
    expect(l.rects.every((x) => x.critical)).toBe(true);
  });
});

describe("view math", () => {
  it("zooms about a focus point, keeping it where it was", () => {
    const v = zoomView(FULL_VIEW, 0.5, 2);
    expect(v).toEqual({ x0: 0.25, x1: 0.75 });
    const v2 = zoomView(v, 0.25, 2); // focus at the left edge stays at the left edge
    expect(v2.x0).toBeCloseTo(0.25);
    expect(v2.x1).toBeCloseTo(0.5);
  });
  it("never leaves the trace, zooms out to the whole of it, and floors the span", () => {
    expect(zoomView({ x0: 0.9, x1: 1 }, 1, 0.01)).toEqual({ x0: 0, x1: 1 });
    const tiny = zoomView(FULL_VIEW, 0.5, 1e12);
    expect(tiny.x1 - tiny.x0).toBeGreaterThan(0);
  });
  it("pans and stops at the edges", () => {
    expect(panView({ x0: 0.2, x1: 0.4 }, 0.1)).toEqual({ x0: 0.30000000000000004, x1: 0.5 });
    expect(panView({ x0: 0.2, x1: 0.4 }, 5)).toEqual({ x0: 0.8, x1: 1 });
    expect(panView({ x0: 0.2, x1: 0.4 }, -5)).toEqual({ x0: 0, x1: 0.2 });
  });
  it("frames a rect with margin, even a zero-width one", () => {
    const v = viewOf({ x: 0.5, w: 0.1 });
    expect(v.x0).toBeLessThan(0.5);
    expect(v.x1).toBeGreaterThan(0.6);
    const z = viewOf({ x: 1, w: 0 });
    expect(z.x1).toBeGreaterThan(z.x0);
  });
});

describe("hitTest and formatting", () => {
  it("finds the narrowest rect under a point on a row, with slack for zero-width ones", () => {
    const l = layoutTrace([sp("r", null, 0, 100), sp("z", "r", 50, 0)]);
    expect(hitTest(l.rects, 0.2, 0)?.id).toBe("r");
    expect(hitTest(l.rects, 0.2, 1)).toBeUndefined();
    expect(hitTest(l.rects, 0.505, 1)).toBeUndefined();
    expect(hitTest(l.rects, 0.505, 1, 0.01)?.id).toBe("z");
  });
  it("formats durations and never turns a missing value into 0", () => {
    expect(formatDuration(850)).toBe("850µs");
    expect(formatDuration(12_400)).toBe("12.4ms");
    expect(formatDuration(2500)).toBe("2.50ms");
    expect(formatDuration(1_200_000)).toBe("1.20s");
    expect(formatDuration(null)).toBe("—");
    expect(formatDuration(undefined)).toBe("—");
    expect(formatDuration(NaN)).toBe("—");
  });
  it("gives a service one stable colour, different in dark mode", () => {
    expect(serviceColor("web")).toBe(serviceColor("web"));
    expect(serviceColor("web")).not.toBe(serviceColor("worker"));
    expect(serviceColor("web", true)).not.toBe(serviceColor("web"));
  });
});
