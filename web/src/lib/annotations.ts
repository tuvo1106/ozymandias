/**
 * Things drawn on a timeseries chart that are not series: threshold bands,
 * recovery lines, a shaded time span, vertical event markers.
 *
 * The chart draws them on its canvas in uPlot hooks (TimeseriesChart.tsx); the
 * geometry is here, as pure functions, because it is where the mistakes are
 * (a band whose far edge is infinite, a y axis that leaves the threshold off
 * screen) and jsdom has no canvas to catch them on.
 */

/** A horizontal band in y units. ±Infinity runs to the plot's edge. */
export interface Band {
  from: number;
  to: number;
  color: string;
}

/** A horizontal line in y units. */
export interface HLine {
  y: number;
  color: string;
  dash?: boolean;
}

/** A shaded span of time, unix seconds. */
export interface Span {
  from: number;
  to: number;
  color: string;
}

/** A vertical line at a time, unix seconds. */
export interface Marker {
  x: number;
  color: string;
}

/** Everything a chart may draw besides its series. */
export interface ChartAnnotations {
  bands?: Band[];
  hlines?: HLine[];
  spans?: Span[];
  markers?: Marker[];
  /**
   * Values the y axis must include. A threshold of 20 over a line at 5 is the
   * picture a monitor editor exists to show ("nowhere near"), and uPlot's
   * auto-range would crop the threshold away.
   */
  yInclude?: number[];
}

/**
 * The part of a band inside the visible range [min, max], or null when none
 * of it is. Infinite edges clamp to the range.
 */
export function clipBand(from: number, to: number, min: number, max: number): [number, number] | null {
  const lo = Math.max(Math.min(from, to), min);
  const hi = Math.min(Math.max(from, to), max);
  return hi > lo ? [lo, hi] : null;
}

/**
 * The y range covering the data's [min, max] and every finite value in
 * `include`, with 5% padding on each side so a line on a threshold is not
 * drawn on the frame. Null min/max (no data) leaves only `include`; with
 * nothing at all, or a single value, the range is widened by 1 so it has
 * height.
 */
export function yRangeIncluding(
  min: number | null | undefined,
  max: number | null | undefined,
  include: readonly number[] = [],
): [number, number] {
  const vals = [min, max, ...include].filter((v): v is number => typeof v === "number" && Number.isFinite(v));
  if (vals.length === 0) return [0, 1];
  let lo = Math.min(...vals);
  let hi = Math.max(...vals);
  if (hi === lo) {
    lo -= 1;
    hi += 1;
  }
  const pad = (hi - lo) * 0.05;
  return [lo - pad, hi + pad];
}

/** Whether x (unix seconds) is inside [from, to], for culling markers. */
export function inXRange(x: number, from: number, to: number): boolean {
  return x >= from && x <= to;
}
