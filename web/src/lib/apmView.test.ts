import type { MapEdge, RedRow, TraceSummary } from "./apmApi";
import { edgeColor, fmtCount, fmtMs, fmtPct, fmtRate, layoutMap, rowLabel, scatterLayout, sortRows, sparkPoints } from "./apmView";

const row = (service: string, over: Partial<RedRow> = {}): RedRow => ({
  service, name: "http.request", requests: 10, requests_per_second: 1, errors: 0, error_pct: 0, p50_ms: 5, p95_ms: 9, p99_ms: 20, sparkline: [], ...over,
});
const edge = (parent: string, child: string, calls = 10, errors = 0): MapEdge => ({ parent, child, calls, errors, avg_duration: 100 });
const trace = (id: string, startMs: number, duration: number, error = false): TraceSummary => ({
  trace_id: id, span_id: id, env: "dev", service: "s", name: "n", resource: "r", start: startMs * 1000, duration, error, trace_error: false,
});

describe("sortRows", () => {
  const rows = [row("b", { p95_ms: 30 }), row("a", { p95_ms: null }), row("c", { p95_ms: 10 }), row("d", { p95_ms: null })];
  it("puts rows with no value last, ascending and descending alike, never as zero", () => {
    expect(sortRows(rows, "p95", "asc").map((r) => r.service)).toEqual(["c", "b", "a", "d"]);
    expect(sortRows(rows, "p95", "desc").map((r) => r.service)).toEqual(["b", "c", "a", "d"]);
  });
  it("sorts by name, numbers and ties deterministically, without mutating the input", () => {
    const before = rows.map((r) => r.service);
    expect(sortRows(rows, "name", "asc").map((r) => r.service)).toEqual(["a", "b", "c", "d"]);
    expect(sortRows(rows, "name", "desc").map((r) => r.service)).toEqual(["d", "c", "b", "a"]);
    expect(sortRows([row("x", { requests: 5 }), row("y", { requests: 5 }), row("z", { requests: 9 })], "requests", "desc").map((r) => r.service)).toEqual(["z", "x", "y"]);
    expect(sortRows([row("x", { error_pct: 2 }), row("y", { error_pct: 7 })], "error_pct", "desc")[0]!.service).toBe("y");
    expect(sortRows([row("x", { requests_per_second: 2 }), row("y", { requests_per_second: 1 })], "rps", "asc")[0]!.service).toBe("y");
    expect(rows.map((r) => r.service)).toEqual(before);
  });
  it("labels a row by its resource when it has one", () => {
    expect(rowLabel(row("s", { resource: "get /x" }))).toBe("get /x");
    expect(rowLabel(row("s"))).toBe("s");
  });
});

describe("formatting", () => {
  it("shows no data as a dash, never as zero", () => {
    for (const f of [fmtMs, fmtRate, fmtPct, fmtCount]) {
      expect(f(null)).toBe("—");
      expect(f(undefined)).toBe("—");
      expect(f(NaN)).toBe("—");
    }
  });
  it("formats real values, including real zeros", () => {
    expect(fmtMs(0.45)).toBe("450µs");
    expect(fmtMs(0)).toBe("0µs");
    expect(fmtMs(4.321)).toBe("4.32 ms");
    expect(fmtMs(12.34)).toBe("12.3 ms");
    expect(fmtMs(1500)).toBe("1.50 s");
    expect(fmtRate(0.5)).toBe("0.50/s");
    expect(fmtRate(0.001)).toBe("<0.01/s");
    expect(fmtRate(0)).toBe("0.00/s");
    expect(fmtRate(250.4)).toBe("250/s");
    expect(fmtPct(0)).toBe("0.00%");
    expect(fmtPct(12.34)).toBe("12.3%");
    expect(fmtCount(12345.4)).toBe("12,345");
  });
});

describe("sparkPoints", () => {
  it("needs two points", () => {
    expect(sparkPoints([], 100, 20)).toBe("");
    expect(sparkPoints([3], 100, 20)).toBe("");
  });
  it("scales into the box, high values at the top", () => {
    const pts = sparkPoints([0, 10, 5], 100, 20).split(" ").map((p) => p.split(",").map(Number));
    expect(pts[0]).toEqual([1, 19]);
    expect(pts[1]).toEqual([50, 1]);
    expect(pts[2]![0]).toBe(100 - 1);
  });
  it("draws a flat series mid-height", () => {
    expect(sparkPoints([4, 4, 4], 100, 20).split(" ").every((p) => p.endsWith(",10.0"))).toBe(true);
  });
});

describe("scatterLayout", () => {
  const win = { from: 0, to: 1000 };
  it("is empty for no traces", () => {
    expect(scatterLayout([], win).points).toEqual([]);
  });
  it("places time linearly and duration on a log axis, slowest at the top", () => {
    const s = scatterLayout([trace("a", 0, 10), trace("b", 500, 100), trace("c", 1000, 1000, true)], win);
    expect(s.points.map((p) => p.x)).toEqual([0, 0.5, 1]);
    expect(s.points.map((p) => p.y)).toEqual([1, 0.5, 0]);
    expect(s.points[2]!.error).toBe(true);
    expect([s.minUs, s.maxUs]).toEqual([10, 1000]);
  });
  it("keeps equal durations on a middle line, lifts a zero duration, and clamps times outside the window", () => {
    expect(scatterLayout([trace("a", 5, 7), trace("b", 6, 7)], win).points.map((p) => p.y)).toEqual([0.5, 0.5]);
    const s = scatterLayout([trace("z", -50, 0), trace("q", 5000, 10)], win);
    expect(s.points.map((p) => p.x)).toEqual([0, 1]);
    expect(s.minUs).toBe(1);
    expect(s.points.every((p) => Number.isFinite(p.y))).toBe(true);
  });
  it("counts a trace error as an error dot", () => {
    expect(scatterLayout([{ ...trace("a", 1, 5), trace_error: true }], win).points[0]!.error).toBe(true);
  });
});

describe("edgeColor", () => {
  it("fades grey to red and saturates", () => {
    expect(edgeColor(0)).toBe("rgb(113 113 122)");
    expect(edgeColor(0.2)).toBe("rgb(220 38 38)");
    expect(edgeColor(5)).toBe(edgeColor(0.2));
    expect(edgeColor(-1)).toBe(edgeColor(0));
  });
});

describe("layoutMap", () => {
  const node = (s: string) => ({ service: s, calls_in: 1, errors_in: 0 });
  const at = (m: ReturnType<typeof layoutMap>, s: string) => m.nodes.find((n) => n.service === s)!;
  it("puts callers left of callees, by longest chain", () => {
    const m = layoutMap([node("web"), node("api"), node("db")], [edge("web", "api"), edge("api", "db"), edge("web", "db")], 800, 400);
    expect([at(m, "web").layer, at(m, "api").layer, at(m, "db").layer]).toEqual([0, 1, 2]);
    expect(at(m, "web").x).toBeLessThan(at(m, "api").x);
    expect(at(m, "api").x).toBeLessThan(at(m, "db").x);
  });
  it("stacks a layer's services vertically inside the box", () => {
    const m = layoutMap([node("a"), node("b"), node("c")], [edge("a", "b"), edge("a", "c")], 800, 300);
    const [b, c] = [at(m, "b"), at(m, "c")];
    expect(b.x).toBe(c.x);
    expect(new Set([b.y, c.y]).size).toBe(2);
    expect(Math.min(b.y, c.y)).toBeGreaterThan(0);
    expect(Math.max(b.y, c.y)).toBeLessThan(300);
  });
  it("terminates on a cycle and on a self-call", () => {
    const m = layoutMap([node("a"), node("b")], [edge("a", "b"), edge("b", "a"), edge("a", "a")], 800, 300);
    expect(m.nodes).toHaveLength(2);
    expect(m.edges.find((e) => e.loop)).toBeDefined();
    expect(m.nodes.every((n) => Number.isFinite(n.x) && Number.isFinite(n.y))).toBe(true);
  });
  it("adds a service that appears only in an edge, and places a lone service", () => {
    const m = layoutMap([], [edge("a", "ghost")], 800, 300);
    expect(m.nodes.map((n) => n.service).sort()).toEqual(["a", "ghost"]);
    const one = layoutMap([node("solo")], [], 800, 300);
    expect(at(one, "solo")).toMatchObject({ x: 400, y: 150, layer: 0 });
    expect(layoutMap([], [], 800, 300)).toEqual({ nodes: [], edges: [] });
  });
  it("sizes edges by call rate and tints them by error rate", () => {
    const m = layoutMap([], [edge("a", "b", 100, 0), edge("a", "c", 1, 1)], 800, 300);
    const [big, small] = m.edges;
    expect(big!.width).toBeGreaterThan(small!.width);
    expect(big!.width).toBe(8);
    expect(big!.errorRate).toBe(0);
    expect(small!.errorRate).toBe(1);
    expect(small!.color).toBe(edgeColor(1));
    expect(layoutMap([], [edge("a", "b", 0, 0)], 1, 1).edges[0]!.errorRate).toBe(0);
  });
});
