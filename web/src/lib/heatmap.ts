/**
 * Turning a sketch into rectangles: the arithmetic behind the heatmap widget,
 * with no canvas and no React in it.
 *
 * A heatmap is one column per time bucket and one rectangle per bin, coloured
 * by how many observations fell in it. The awkward parts are all here, because
 * they are the parts worth testing:
 *
 *   - **A bin can be thinner than a pixel.** DDSketch bins are geometric, so a
 *     latency distribution from 4ms to 2s has hundreds of them and, on a
 *     linear axis, the fast ones land on the same row of pixels. Drawing them
 *     one over the other would show the last one's count and lose the rest —
 *     so bins that collapse onto one band are *merged*, and their counts
 *     added. They are disjoint value ranges, so the sum is the number of
 *     observations that row really holds.
 *   - **Not every value fits on the axis.** A log axis has no room for zero or
 *     for a negative, and zero is its own bin by construction (log γ 0 is
 *     undefined). Those observations are counted out rather than silently
 *     dropped, so the widget can say how many it is not showing.
 *   - **The colour scale is logarithmic.** Traffic is heavily skewed — the
 *     mode of a latency distribution holds orders of magnitude more than its
 *     tail — and a linear ramp paints everything but the mode the same colour,
 *     which is the tail nobody can see and the reason the heatmap exists.
 *
 * Projection is passed in as two functions rather than computed here. The
 * widget gets its scales from uPlot, which owns the axes, the log distribution
 * and the shared cursor; this module needs to know only where a value lands.
 */
import type { SketchBucket, SketchSeries } from "./dashboardsApi";
import type { YAxis } from "./dashboard";

/**
 * The shortest a cell can be: one pixel, the smallest thing a canvas can show.
 *
 * Not two. A floor of two makes *contiguous* bins overlap — every bin's
 * rectangle would reach a pixel past its true bottom, into the bin below it —
 * and since [[cellsFor]] merges what overlaps, the merge cascades and a
 * hundred bins collapse into one cell holding the whole bucket. The floor is
 * what makes a sub-pixel bin visible; it must not make an ordinary one
 * collide with its neighbour.
 */
export const MIN_CELL_HEIGHT = 1;

/** One rectangle to fill, in CSS pixels, with the count that coloured it. */
export interface HeatCell {
  x: number;
  y: number;
  w: number;
  h: number;
  /** Observations in this band — the sum where bins were merged into it. */
  count: number;
}

/** What [[cellsFor]] produced, and what it could not place. */
export interface HeatCells {
  cells: HeatCell[];
  /**
   * Observations the axis has no room for: zero and negatives on a log axis.
   * Reported rather than dropped, because "no requests were that fast" and
   * "the axis cannot show a zero" look identical on the canvas.
   */
  offAxis: number;
  /** The largest count in any cell, which is the top of the colour scale. */
  max: number;
}

/**
 * The lowest and highest value any bin covers, or undefined for no bins.
 *
 * `positiveOnly` leaves out everything a log axis has no position for. It is a
 * parameter rather than a filter the caller applies because the alternative —
 * letting one zero-valued observation set `lo` to 0 — turns the whole axis
 * linear, and a latency heatmap ruined by a single zero is a worse answer than
 * one that draws the positive values and says what it left out.
 */
export function valueExtent(
  buckets: readonly SketchBucket[],
  positiveOnly = false,
): { lo: number; hi: number } | undefined {
  let lo = Infinity;
  let hi = -Infinity;
  for (const bucket of buckets) {
    for (const [lower, upper, count] of bucket.bins) {
      if (!(count > 0)) continue;
      if (positiveOnly && lower <= 0) continue;
      // No guard for a bound the server could not write as a number: it
      // arrives as NaN, and every comparison with NaN is false, so both tests
      // below reject it without being asked to. [[unboundedCount]] is where
      // that bin is counted.
      if (lower < lo) lo = lower;
      if (upper > hi) hi = upper;
    }
  }
  return Number.isFinite(lo) && Number.isFinite(hi) ? { lo, hi } : undefined;
}

/**
 * The axis range to draw over: the author's `min`/`max` where they gave them,
 * the data's extent otherwise.
 *
 * A single-valued distribution — every observation in one bin — would give a
 * zero-height axis, which no scale can divide by, so it is widened around the
 * value. Widened multiplicatively rather than by a constant, since the same
 * function serves a metric in seconds and one in bytes.
 */
export function axisRange(
  buckets: readonly SketchBucket[],
  yaxis: YAxis | undefined,
  log = false,
): { lo: number; hi: number } | undefined {
  const extent = valueExtent(buckets, log);
  const lo = yaxis?.min ?? extent?.lo;
  const hi = yaxis?.max ?? extent?.hi;
  if (lo === undefined || hi === undefined) return undefined;
  if (hi > lo) return { lo, hi };
  const pad = Math.abs(lo) * 0.1 || 1;
  return { lo: lo - pad, hi: hi + pad };
}

/**
 * Whether to draw the value axis logarithmically.
 *
 * The author's `scale` decides; `linear` is the definition's documented
 * default (dashboards.md) and is honoured even though a geometric sketch
 * usually wants `log`, because a widget that ignores what it was told is worse
 * than one that draws badly.
 *
 * It depends on the definition alone and not on the data. A log axis whose
 * *range* reached zero would be incoherent, but that is answered by leaving
 * the non-positive bins off the axis ([[valueExtent]]) and saying how many
 * ([[offAxisCount]]) — not by quietly making the chart linear, which would
 * change what the reader is looking at without telling them. A `min` at or
 * below zero is the one thing that does turn it off, and the server refuses
 * that combination anyway.
 */
export function wantsLogAxis(yaxis: YAxis | undefined): boolean {
  return yaxis?.scale === "log" && (yaxis.min === undefined || yaxis.min > 0);
}

/**
 * Lays a series' buckets out as rectangles.
 *
 * `toX` maps a unix-millisecond instant to a pixel and `toY` a value; both
 * come from the chart's own scales. `intervalMs` is how wide one bucket is —
 * taken from the response rather than from the gap between buckets, which is
 * larger wherever nothing was recorded.
 *
 * Whatever pixels the projections return are the pixels that come back — on a
 * canvas those are device pixels, not CSS ones.
 */
export function cellsFor(
  buckets: readonly SketchBucket[],
  intervalMs: number,
  toX: (tMs: number) => number,
  toY: (value: number) => number,
): HeatCells {
  const cells: HeatCell[] = [];
  let offAxis = 0;
  let max = 0;
  for (const bucket of buckets) {
    const left = Math.round(toX(bucket.t));
    const w = Math.max(Math.round(toX(bucket.t + intervalMs)) - left, 1);
    // Reset per bucket: merging is about pixels sharing a column, and two
    // columns' bins never overlap however close their values are.
    let open: HeatCell | undefined;
    for (const [lower, upper, count] of bucket.bins) {
      if (!(count > 0)) continue;
      const a = toY(upper);
      const b = toY(lower);
      if (!Number.isFinite(a) || !Number.isFinite(b)) {
        offAxis += count;
        continue;
      }
      // Bins arrive in ascending value order and a bigger value is a smaller
      // y, so `top` walks up the canvas as the loop runs — which is why the
      // merge below grows the open cell upwards.
      const top = Math.round(Math.min(a, b));
      const bottom = Math.max(
        Math.round(Math.max(a, b)),
        top + MIN_CELL_HEIGHT,
      );
      // `bottom > open.y`, strictly: two bins that merely touch — which every
      // pair of neighbouring bins does, one's upper bound being the next one's
      // lower — do not overlap, and merging them would be merging everything.
      if (open && bottom > open.y) {
        open.h = open.y + open.h - top;
        open.y = top;
        open.count += count;
        if (open.count > max) max = open.count;
        continue;
      }
      open = { x: left, y: top, w, h: bottom - top, count };
      if (count > max) max = count;
      cells.push(open);
    }
  }
  return { cells, offAxis, max };
}

/**
 * How many observations a log axis has no room for: everything at or below
 * zero.
 *
 * The same number [[cellsFor]] reports, arrived at without a canvas — the
 * widget needs it to write a sentence, and making that depend on a paint would
 * mean React state written from inside a draw. Zero for a linear axis, which
 * can show every value a sketch holds.
 */
export function offAxisCount(
  buckets: readonly SketchBucket[],
  log: boolean,
): number {
  if (!log) return 0;
  let total = 0;
  for (const bucket of buckets) {
    for (const [lower, , count] of bucket.bins) {
      // The lower bound, not the upper: a bin is off the axis as soon as one
      // of its edges has no log, and it is the lower edge that reaches zero
      // first. Testing `upper` instead would agree with [[cellsFor]] on the
      // zero bin and disagree on a first positive bin whose lower bound is 0.
      //
      // A bound that is NaN is not counted here — it is [[unboundedCount]]'s,
      // because it is off *any* axis and for a different reason worth saying.
      if (count > 0 && lower <= 0) total += count;
    }
  }
  return total;
}

/**
 * How many observations sit in a bin whose bounds the server could not write
 * as numbers — "past what a float64 can say", which a bucket index near the
 * wire format's limit produces.
 *
 * Separate from [[offAxisCount]] because it is a different thing to tell the
 * reader: a zero on a log axis is a shape the axis cannot hold, and this is a
 * number the format cannot hold. It is off a *linear* axis too, which is why
 * it does not take the scale.
 */
export function unboundedCount(buckets: readonly SketchBucket[]): number {
  let total = 0;
  for (const bucket of buckets) {
    for (const [lower, upper, count] of bucket.bins) {
      if (count > 0 && (!Number.isFinite(lower) || !Number.isFinite(upper)))
        total += count;
    }
  }
  return total;
}

/**
 * The colour ramp, dark to light: viridis' five anchors.
 *
 * Viridis because it is monotonic in perceived lightness, so "darker means
 * fewer" holds whether the page is light or dark and whether the reader sees
 * red and green as different colours. One ramp for both themes: a palette that
 * changed with the theme would make two screenshots of the same distribution
 * uncomparable.
 */
type Rgb = readonly [number, number, number];

const RAMP: readonly Rgb[] = [
  [68, 1, 84],
  [59, 82, 139],
  [33, 145, 140],
  [94, 201, 98],
  [253, 231, 37],
];

/** The ramp's i-th anchor, clamped to its ends. */
function stop(i: number): Rgb {
  return RAMP[Math.min(Math.max(i, 0), RAMP.length - 1)] ?? [0, 0, 0];
}

/**
 * The colour for a cell holding `count` observations, where `max` is the
 * fullest cell on the chart.
 *
 * Scaled by log1p rather than linearly: see the file comment. A `max` below
 * one is treated as one, so a chart whose fullest cell holds a single
 * observation paints it at the top of the ramp instead of dividing by zero.
 */
export function heatColor(count: number, max: number): string {
  // `max(max, 1)` two lines up is what makes the divisor non-zero, so there is
  // no guard for it here: the check would be dead code, and a dead check reads
  // as though the case were possible.
  const span = Math.log1p(Math.max(max, 1));
  const fraction = Math.min(
    Math.max(Math.log1p(Math.max(count, 0)) / span, 0),
    1,
  );
  const pos = fraction * (RAMP.length - 1);
  const i = Math.min(Math.floor(pos), RAMP.length - 2);
  const t = pos - i;
  const from = stop(i);
  const to = stop(i + 1);
  const mix = (k: 0 | 1 | 2) => Math.round(from[k] + (to[k] - from[k]) * t);
  return `rgb(${mix(0)}, ${mix(1)}, ${mix(2)})`;
}

/**
 * The widest error bar on the chart: α = (γ-1)/(γ+1) for the least accurate
 * bucket.
 *
 * The largest rather than an average, because the number is there to tell the
 * reader how much to trust a band's position, and the honest answer to "how
 * wrong can this be" is the worst case (api.md). Zero when nothing said.
 */
export function relativeAccuracy(buckets: readonly SketchBucket[]): number {
  let worst = 0;
  for (const { gamma } of buckets) {
    if (!(gamma > 1)) continue;
    const alpha = (gamma - 1) / (gamma + 1);
    if (alpha > worst) worst = alpha;
  }
  return worst;
}

/**
 * The series a heatmap draws, and how many it is leaving out.
 *
 * One, because two distributions drawn over each other are unreadable and the
 * definition says so (dashboards.md). A `dist:` query with a `by` is still
 * legal and still returns several groups, so the extras are counted and the
 * widget says how many rather than pretending the first is the whole answer.
 */
export function primarySeries(series: readonly SketchSeries[]): {
  series: SketchSeries | undefined;
  others: number;
} {
  return { series: series[0], others: Math.max(series.length - 1, 0) };
}
