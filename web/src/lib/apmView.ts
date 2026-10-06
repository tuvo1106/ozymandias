/**
 * Pure view helpers for the APM pages: sorting and formatting the RED tables,
 * the sparkline path, the duration scatter's coordinates and the service map's
 * layered layout. Kept out of the components so the rules (what a missing
 * latency looks like, how a cycle is laid out) are unit-tested, as the Log
 * Explorer's are in logsView.ts.
 *
 * One rule runs through all of it: a missing value is not zero. A service whose
 * window holds no latency sketches has `p95_ms: null`, and every function here
 * shows that as "—" and sorts it after every real number, in either direction.
 * A "0 ms" there would read as the fastest service in the fleet.
 */
import type { MapEdge, MapNode, RedRow, TraceSummary } from "./apmApi";

/** The columns the service tables sort by. */
export type SortKey = "name" | "requests" | "rps" | "error_pct" | "p50" | "p95" | "p99";

const SORT_VALUE: Record<Exclude<SortKey, "name">, (r: RedRow) => number | null> = {
  requests: (r) => r.requests,
  rps: (r) => r.requests_per_second,
  error_pct: (r) => r.error_pct,
  p50: (r) => r.p50_ms,
  p95: (r) => r.p95_ms,
  p99: (r) => r.p99_ms,
};

/** The label a row is known by: service (and entry kind), or resource on a service page. */
export function rowLabel(r: RedRow): string {
  return r.resource ?? r.service;
}

/** Sorts a copy of `rows`; rows without a value for the key go last whichever way it sorts. */
export function sortRows(rows: readonly RedRow[], key: SortKey, dir: "asc" | "desc"): RedRow[] {
  const sign = dir === "asc" ? 1 : -1;
  const tie = (a: RedRow, b: RedRow) => rowLabel(a).localeCompare(rowLabel(b)) || a.name.localeCompare(b.name) || (a.env ?? "").localeCompare(b.env ?? "");
  return [...rows].sort((a, b) => {
    if (key === "name") return sign * (rowLabel(a).localeCompare(rowLabel(b)) || a.name.localeCompare(b.name)) || tie(a, b);
    const av = SORT_VALUE[key](a);
    const bv = SORT_VALUE[key](b);
    if (av === null && bv === null) return tie(a, b);
    if (av === null) return 1;
    if (bv === null) return -1;
    return sign * (av - bv) || tie(a, b);
  });
}

/** Milliseconds for a table cell: `—` for no data, `450µs`, `12.4 ms`, `1.20 s`. */
export function fmtMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return "—";
  if (ms < 1) return `${Math.round(ms * 1000)}µs`;
  if (ms < 1000) return `${ms < 10 ? ms.toFixed(2) : ms.toFixed(1)} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

/** A request rate: `0.52/s`, `<0.01/s` for a trickle, `—` for no number. */
export function fmtRate(rps: number | null | undefined): string {
  if (rps === null || rps === undefined || !Number.isFinite(rps)) return "—";
  if (rps > 0 && rps < 0.01) return "<0.01/s";
  return `${rps >= 100 ? Math.round(rps) : rps.toFixed(2)}/s`;
}

/** A percentage, `—` when it is not a number. */
export function fmtPct(p: number | null | undefined): string {
  if (p === null || p === undefined || !Number.isFinite(p)) return "—";
  return `${p >= 10 ? p.toFixed(1) : p.toFixed(2)}%`;
}

/** A count with thousands separators, `—` when it is not a number. */
export function fmtCount(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return "—";
  return Math.round(n).toLocaleString("en-US");
}

/**
 * Points for an SVG polyline of `values` in a `w` x `h` box. Needs two points
 * to be a line, so fewer give "" and the caller shows its empty state. A flat
 * series is drawn mid-height rather than dividing by a zero range.
 */
export function sparkPoints(values: readonly number[], w: number, h: number, pad = 1): string {
  if (values.length < 2) return "";
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min;
  const step = (w - 2 * pad) / (values.length - 1);
  return values
    .map((v, i) => {
      const y = span === 0 ? h / 2 : h - pad - ((v - min) / span) * (h - 2 * pad);
      return `${(pad + i * step).toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
}

/** One dot of the duration scatter. */
export interface ScatterPoint {
  traceId: string;
  /** 0..1 across the window. */
  x: number;
  /** 0..1, 0 at the top (slowest). */
  y: number;
  error: boolean;
  durationUs: number;
}

/** The scatter's points and the duration range its y axis spans (µs, log scale). */
export interface Scatter {
  points: ScatterPoint[];
  minUs: number;
  maxUs: number;
}

/**
 * Places traces on a time (linear) by duration (log) plane. Durations span
 * orders of magnitude (a cache hit and a timeout share a service), so a linear
 * axis would flatten all but the outliers onto the floor. A zero or missing
 * duration is lifted to 1µs for the log; times outside the window are clamped
 * to its edge rather than dropped, since a trace the search returned is in it.
 */
export function scatterLayout(traces: readonly TraceSummary[], win: { from: number; to: number }): Scatter {
  if (traces.length === 0) return { points: [], minUs: 1, maxUs: 1 };
  const durs = traces.map((t) => Math.max(1, t.duration));
  const minUs = Math.min(...durs);
  const maxUs = Math.max(...durs);
  const lo = Math.log10(minUs);
  const range = Math.log10(maxUs) - lo;
  const wspan = Math.max(1, win.to - win.from);
  return {
    minUs,
    maxUs,
    points: traces.map((t, i) => ({
      traceId: t.trace_id,
      x: Math.min(1, Math.max(0, (t.start / 1000 - win.from) / wspan)),
      y: range === 0 ? 0.5 : 1 - (Math.log10(durs[i]!) - lo) / range,
      error: t.error || t.trace_error,
      durationUs: t.duration,
    })),
  };
}

/** A node placed on the map. */
export interface PlacedNode extends MapNode {
  x: number;
  y: number;
  layer: number;
}

/** An edge ready to draw. */
export interface PlacedEdge extends MapEdge {
  /** Stroke width in px: call rate. */
  width: number;
  /** errors / calls, 0..1. */
  errorRate: number;
  /** Stroke colour: grey fading to red with the error rate. */
  color: string;
  /** Parent and child are the same service (a call within itself). */
  loop: boolean;
}

/** The laid-out map. */
export interface MapLayout {
  nodes: PlacedNode[];
  edges: PlacedEdge[];
}

/** Edge colour: neutral at 0% errors, fully red from `FULL_RED` of calls failing. */
const FULL_RED = 0.2;

/** Grey to red by error rate; a tint rather than a threshold, so a trickle of errors is visible but not alarming. */
export function edgeColor(errorRate: number): string {
  const t = Math.min(1, Math.max(0, errorRate / FULL_RED));
  const mix = (a: number, b: number) => Math.round(a + (b - a) * t);
  return `rgb(${mix(113, 220)} ${mix(113, 38)} ${mix(122, 38)})`;
}

/**
 * Lays the service graph out in layers, callers left of callees: a service's
 * layer is the longest call chain that reaches it. A force-directed layout was
 * the alternative; layers are deterministic (the same data draws the same map,
 * so a refresh does not make it dance), need no iteration to test, and read
 * as the direction of the calls. Cycles (A calls B calls A) would make "longest
 * chain" infinite, so the relaxation runs at most one pass per node and an
 * edge that keeps lengthening a cycle simply stops. Services that appear
 * only in an edge are added as nodes; a service nothing calls and that calls
 * nothing still gets a place, in layer 0.
 */
export function layoutMap(nodes: readonly MapNode[], edges: readonly MapEdge[], width: number, height: number): MapLayout {
  const byName = new Map<string, MapNode>(nodes.map((n) => [n.service, n]));
  for (const e of edges) {
    for (const s of [e.parent, e.child]) if (!byName.has(s)) byName.set(s, { service: s, calls_in: 0, errors_in: 0 });
  }
  const names = [...byName.keys()].sort();
  const layer = new Map<string, number>(names.map((n) => [n, 0]));
  const real = edges.filter((e) => e.parent !== e.child);
  for (let pass = 0; pass < names.length; pass++) {
    let changed = false;
    for (const e of real) {
      const want = (layer.get(e.parent) ?? 0) + 1;
      if (want > (layer.get(e.child) ?? 0) && want < names.length) {
        layer.set(e.child, want);
        changed = true;
      }
    }
    if (!changed) break;
  }
  const layers: string[][] = [];
  for (const n of names) (layers[layer.get(n)!] ??= []).push(n);
  const cols = Math.max(1, layers.length);
  const placed: PlacedNode[] = [];
  layers.forEach((members, li) => {
    (members ?? []).forEach((name, i, all) => {
      placed.push({
        ...byName.get(name)!,
        layer: li,
        x: cols === 1 ? width / 2 : 60 + (li * (width - 120)) / (cols - 1),
        y: ((i + 1) * height) / (all.length + 1),
      });
    });
  });
  const maxCalls = Math.max(1, ...edges.map((e) => e.calls));
  return {
    nodes: placed,
    edges: edges.map((e) => {
      const errorRate = e.calls > 0 ? Math.min(1, e.errors / e.calls) : 0;
      return { ...e, width: 1 + 7 * Math.sqrt(e.calls / maxCalls), errorRate, color: edgeColor(errorRate), loop: e.parent === e.child };
    }),
  };
}
