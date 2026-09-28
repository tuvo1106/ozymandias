/**
 * A React wrapper around uPlot for heatmaps.
 *
 * uPlot draws lines, not rectangles — so what it is used for here is
 * everything *around* the rectangles: a time axis identical to the line charts
 * beside it, a value axis that knows how to be logarithmic, the size handling,
 * and the shared cursor, since a dashboard's crosshair must cross the heatmap
 * too or it stops being a crosshair. The cells themselves are painted in a
 * `draw` hook, which uPlot calls with the canvas and the scales already set
 * up. That is uPlot's own recommended shape for this, and it is a great deal
 * less code than a second axis implementation that would drift from the first.
 *
 * The one series handed to uPlot carries no points. It exists because a uPlot
 * needs a y scale and a scale needs a series to belong to; its range is forced
 * to the range this chart was asked for. Showing it is pointless — there is
 * nothing to draw — so it is `show: false` and the legend is off.
 *
 * Same lifecycle discipline as [[TimeseriesChart]]: the instance lives in a
 * ref, anything uPlot cannot change in place is part of the effect's key, and
 * a ResizeObserver keeps the canvas as wide as its container.
 */
import { useEffect, useRef, useSyncExternalStore } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { formatValue } from "../lib/chartData";
import { cellsFor, heatColor } from "../lib/heatmap";
import type { SketchBucket } from "../lib/dashboardsApi";

/** Props for HeatmapChart. */
export interface HeatmapChartProps {
  /** The buckets to draw, in time order; an absent bucket is a gap. */
  buckets: readonly SketchBucket[];
  /** Bucket width in seconds, as the response reported it. */
  interval: number;
  /** The x extent in unix seconds — the window asked for, not the data's. */
  xRange: [number, number];
  /** The y extent in the metric's own units. */
  yRange: [number, number];
  /** Whether the value axis is logarithmic. */
  log?: boolean;
  /**
   * Canvas height in CSS pixels. Omit it to fill the container, which then has
   * to have a definite height — see [[TimeseriesChart]] for why.
   */
  height?: number;
  /** Charts sharing this key share a cursor. */
  syncKey?: string;
}

/** Shortest canvas worth drawing on. */
const MIN_HEIGHT = 40;

/** Canvas height when the container cannot say. */
const FALLBACK_HEIGHT = 240;

const DARK_QUERY = "(prefers-color-scheme: dark)";

function subscribeDark(onChange: () => void): () => void {
  const mql = window.matchMedia?.(DARK_QUERY);
  mql?.addEventListener("change", onChange);
  return () => mql?.removeEventListener("change", onChange);
}

function prefersDark(): boolean {
  return window.matchMedia?.(DARK_QUERY).matches ?? false;
}

/** Everything the draw hook needs, read fresh on every paint. */
interface DrawState {
  buckets: readonly SketchBucket[];
  interval: number;
}

function buildOptions(
  dark: boolean,
  width: number,
  height: number,
  ranges: React.RefObject<{ x: [number, number]; y: [number, number] }>,
  log: boolean,
  syncKey: string | undefined,
  state: React.RefObject<DrawState>,
): uPlot.Options {
  const ink = dark ? "#a1a1aa" : "#52525b";
  const grid = dark ? "#27272a" : "#e4e4e7";
  const axis: uPlot.Axis = { stroke: ink, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
  return {
    width,
    height,
    scales: {
      x: { time: true, range: () => ranges.current.x },
      // distr 3 is uPlot's log distribution. The caller decides: a log axis
      // has no position for zero, and a distribution's zero bin is real.
      y: { distr: log ? 3 : 1, range: () => ranges.current.y },
    },
    axes: [axis, { ...axis, size: 60, values: (_u, splits) => splits.map((v) => formatValue(v)) }],
    // One invisible series: uPlot needs one to own the y scale (see above).
    series: [{}, { show: false, scale: "y" }],
    legend: { show: false },
    cursor: {
      drag: { x: false, y: false },
      ...(syncKey ? { sync: { key: syncKey, scales: ["x", null] as [string | null, string | null] } } : {}),
    },
    hooks: {
      draw: [
        (u) => {
          const { buckets, interval } = state.current;
          const ctx = u.ctx;
          const { left, top, width: w, height: h } = u.bbox;
          ctx.save();
          // Clipped to the plot area: a bin can sit outside an axis the author
          // pinned with yaxis.min/max, and uPlot will happily hand back a
          // position in the margin where the labels are.
          ctx.beginPath();
          ctx.rect(left, top, w, h);
          ctx.clip();
          // `true` asks for canvas pixels: uPlot's positions are CSS pixels by
          // default, and u.bbox and the 2d context are both in device pixels.
          // Mixing the two draws a correct heatmap in the top-left quarter of
          // the chart on any display with a ratio above 1.
          const { cells, max } = cellsFor(
            buckets,
            interval * 1000,
            (tMs) => u.valToPos(tMs / 1000, "x", true),
            (v) => u.valToPos(v, "y", true),
          );
          for (const cell of cells) {
            ctx.fillStyle = heatColor(cell.count, max);
            ctx.fillRect(cell.x, cell.y, cell.w, cell.h);
          }
          ctx.restore();
        },
      ],
    },
  };
}

/**
 * Draws a distribution over time. Nothing is drawn for a bucket with no
 * observations, which is what makes a quiet period look quiet.
 *
 * How many observations the axis could not show is *not* reported from here.
 * Writing React state from inside a paint is how a redraw becomes a loop, and
 * the number does not need a canvas — [[offAxisCount]] computes it from the
 * data, and the widget says it beside the chart.
 */
export function HeatmapChart({ buckets, interval, xRange, yRange, log = false, height, syncKey }: HeatmapChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);
  const rangesRef = useRef({ x: xRange, y: yRange });
  const stateRef = useRef<DrawState>({ buckets, interval });
  const dark = useSyncExternalStore(subscribeDark, prefersDark, () => false);

  useEffect(() => {
    rangesRef.current = { x: xRange, y: yRange };
    stateRef.current = { buckets, interval };
  });

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const opts = buildOptions(dark, el.clientWidth || 600, height ?? FALLBACK_HEIGHT, rangesRef, log, syncKey, stateRef);
    // Two x values so uPlot has an axis to draw; the y column is all nulls
    // because the cells are painted by the hook, not by a series.
    const plot = new uPlot(opts, [rangesRef.current.x, [null, null]] as uPlot.AlignedData, el);
    plotRef.current = plot;
    // No legend to subtract here — the cells are painted, not seriesed — so
    // the container's box is the canvas.
    const fit = () => {
      const box = Math.floor(el.clientHeight);
      plot.setSize({
        width: Math.floor(el.clientWidth) || 600,
        height: height ?? (box > 0 ? Math.max(box, MIN_HEIGHT) : FALLBACK_HEIGHT),
      });
    };
    fit();
    const ro = new ResizeObserver(fit);
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot.destroy();
      plotRef.current = null;
    };
  }, [dark, height, log, syncKey]);

  // A redraw rather than a rebuild: the scales read their ranges from the ref
  // above, so new data or a new window is a repaint of the same chart.
  useEffect(() => {
    plotRef.current?.setData([xRange, [null, null]] as uPlot.AlignedData, true);
  }, [buckets, interval, xRange, yRange]);

  return <div ref={containerRef} className="h-full w-full" data-testid="heatmap-chart" />;
}
