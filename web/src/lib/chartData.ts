/**
 * Shaping query results for the timeseries chart.
 *
 * uPlot wants *columnar, aligned* data: one shared x array, and for each
 * series a y array of the same length (`[xs, ys1, ys2, …]`). The query API
 * returns *row* data per series, and series need not share timestamps (a
 * series that started mid-window, or a server that omits trailing buckets).
 * alignSeries bridges the two by taking the union of all timestamps and
 * filling holes with null — which uPlot draws as a gap, the same thing an
 * empty bucket from the server means. Interpolating across holes instead
 * would invent data the system never received.
 */
import type { Series } from "./metricsApi";

/** uPlot's aligned data: x values (unix seconds) then one y array per series. */
export type AlignedData = [number[], ...(number | null)[][]];

/**
 * Aligns series onto the union of their timestamps. Timestamps arrive in
 * milliseconds and leave in seconds, uPlot's native time unit. The result
 * has `series.length + 1` arrays, all the same length, x ascending.
 */
export function alignSeries(series: readonly Series[]): AlignedData {
  const xsMs = [...new Set(series.flatMap((s) => s.points.map(([t]) => t)))].sort((a, b) => a - b);
  const index = new Map(xsMs.map((t, i) => [t, i]));
  const ys = series.map((s) => {
    const y: (number | null)[] = new Array<number | null>(xsMs.length).fill(null);
    for (const [t, v] of s.points) y[index.get(t) as number] = v;
    return y;
  });
  return [xsMs.map((t) => t / 1000), ...ys];
}

/**
 * The legend label for a series: `metric{k:v,k2:v2}` with tag keys sorted so
 * a label never changes with the order the server listed them in. A series
 * with no group tags is the whole selection, spelled `{*}`.
 */
export function seriesLabel(series: Pick<Series, "metric" | "tags">): string {
  const tags = Object.keys(series.tags)
    .sort()
    .map((k) => `${k}:${series.tags[k]}`);
  return `${series.metric}{${tags.length ? tags.join(",") : "*"}}`;
}

/**
 * Formats a value for the legend and axis: up to 3 decimals for small
 * numbers, fewer as magnitude grows, with k/M/G/T suffixes past 10 000.
 * Null (an empty bucket) is an em dash.
 */
export function formatValue(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  const abs = Math.abs(v);
  if (abs >= 10_000) {
    const units: [number, string][] = [
      [1e12, "T"],
      [1e9, "G"],
      [1e6, "M"],
      [1e3, "k"],
    ];
    const [div, suffix] = units.find(([d]) => abs >= d) as [number, string];
    return `${trim((v / div).toFixed(2))}${suffix}`;
  }
  const decimals = abs >= 100 ? 1 : abs >= 1 ? 2 : 3;
  return trim(v.toFixed(decimals));
}

function trim(s: string): string {
  const t = s.includes(".") ? s.replace(/\.?0+$/, "") : s;
  return t === "-0" ? "0" : t;
}

/**
 * Categorical series colours, in fixed order, stepped separately for light
 * and dark surfaces (the dataviz reference palette; validated for colour-
 * vision deficiency on adjacent pairs). Colours are assigned by position and
 * never cycled: a ninth colour would repeat one and make two series
 * indistinguishable, so series past the eighth all share a neutral grey and
 * rely on the legend for identity.
 */
const PALETTE = {
  light: ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#4a3aa7", "#e34948"],
  dark: ["#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"],
} as const;

/** Colour for the series overflow past the palette. */
export const OVERFLOW_COLOR = { light: "#a1a1aa", dark: "#71717a" } as const;

/** Returns the line colour for the i-th series (0-based) in a theme. */
export function seriesColor(i: number, dark: boolean): string {
  const theme = dark ? "dark" : "light";
  return PALETTE[theme][i] ?? OVERFLOW_COLOR[theme];
}
