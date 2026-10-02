/**
 * A React wrapper around uPlot for line charts.
 *
 * uPlot is an imperative canvas library: you build a chart once with options
 * and then push data into it. React wants to re-render declaratively. The
 * bridge is the usual one for imperative widgets:
 *
 * - the uPlot instance lives in a ref, created in an effect on mount and
 *   destroyed in that effect's cleanup;
 * - new data for the *same* set of series goes through `setData`, which
 *   redraws without rebuilding the DOM, legend or cursor — so a 10-second
 *   auto-refresh doesn't flicker or lose the hover position;
 * - anything uPlot cannot change after construction (the number and labels
 *   of series, their colours, the theme) is part of the effect's key, so a
 *   change rebuilds the chart;
 * - a ResizeObserver keeps the canvas as wide as its container.
 *
 * The legend is uPlot's own "live" legend, which shows each series' value
 * under the cursor as you hover. Nulls in the data are drawn as gaps.
 *
 * `syncKey` is how a dashboard gets one crosshair across a dozen charts:
 * uPlot's own `cursor.sync` puts every chart sharing a key in one group and
 * moves their cursors together, matching on the x *value*. Doing it through
 * React state instead would re-render the grid on every mouse move, which is
 * the thing that makes a dashboard feel broken — and this is a library feature,
 * so there is nothing to hand-roll. Synced on x only: the charts share a time
 * axis and nothing else, and syncing y would drag unrelated scales around.
 */
import { useEffect, useRef, useSyncExternalStore, type RefObject } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { formatValue, hasIsolatedValues, seriesColor, type AlignedData } from "../lib/chartData";

/** Props for TimeseriesChart. */
export interface TimeseriesChartProps {
  /** Aligned columns: x in unix seconds, then one y array per label. */
  data: AlignedData;
  /** Series labels, in the same order as `data`'s y arrays. */
  labels: string[];
  /**
   * The x extent in unix seconds, normally the query window. Without it
   * the axis would shrink to the first and last point, hiding the fact that
   * a series has no data at the start or end of the range.
   */
  xRange?: [number, number];
  /**
   * Canvas height in CSS pixels. **Omit it to fill the container**, which then
   * has to have a definite height of its own — a dashboard widget does, a
   * page that grows with its content does not, and auto-fitting inside one
   * that does not is a resize loop.
   */
  height?: number;
  /**
   * Charts sharing a key share a cursor. Undefined leaves the chart out of
   * every group, which is what a standalone chart like the explorer's wants.
   */
  syncKey?: string;
}

/** Shortest canvas worth drawing on; below this an axis has no room for a tick. */
const MIN_HEIGHT = 40;

/** The most of a chart's box the legend may take before it scrolls. */
const MAX_LEGEND_SHARE = 0.4;

/** Canvas height when the container cannot say, e.g. before the first layout. */
const FALLBACK_HEIGHT = 320;

const DARK_QUERY = "(prefers-color-scheme: dark)";

function subscribeDark(onChange: () => void): () => void {
  const mql = window.matchMedia?.(DARK_QUERY);
  mql?.addEventListener("change", onChange);
  return () => mql?.removeEventListener("change", onChange);
}

function prefersDark(): boolean {
  return window.matchMedia?.(DARK_QUERY).matches ?? false;
}

function buildOptions(
  labels: string[],
  dark: boolean,
  width: number,
  height: number,
  xRange: RefObject<[number, number] | undefined>,
  syncKey: string | undefined,
): uPlot.Options {
  const ink = dark ? "#a1a1aa" : "#52525b";
  const grid = dark ? "#27272a" : "#e4e4e7";
  const axis: uPlot.Axis = { stroke: ink, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
  return {
    width,
    height,
    scales: {
      x: {
        time: true,
        range: (_u, min, max) => xRange.current ?? [min, max],
      },
    },
    axes: [axis, { ...axis, size: 60, values: (_u, splits) => splits.map((v) => formatValue(v)) }],
    series: [
      {},
      ...labels.map((label, i) => ({
        label,
        stroke: seriesColor(i, dark),
        width: 2,
        value: (_u: uPlot, v: number | null) => formatValue(v),
        // Markers only where the line cannot draw the value on its own. Always
        // showing them clutters a dense chart; never showing them makes a
        // series of isolated samples look like no data at all.
        points: {
          show: (u: uPlot, seriesIdx: number) => hasIsolatedValues(u.data[seriesIdx] as (number | null)[]),
        },
      })),
    ],
    legend: { live: true },
    cursor: {
      drag: { x: false, y: false },
      // "x" and null: match the x scale by value, leave y alone. Two widgets
      // on one dashboard share a time axis and nothing else, so syncing y
      // would drag a percentage chart onto a byte chart's scale.
      ...(syncKey ? { sync: { key: syncKey, scales: ["x", null] as [string | null, string | null] } } : {}),
    },
  };
}

/**
 * Sizes the plot to its container.
 *
 * The legend is DOM *below* the canvas, not part of it, so filling the
 * container means giving the canvas what is left after the legend — otherwise
 * the legend hangs out of the widget it belongs to, which is what a fixed
 * "chrome height" guess got wrong: it could not know whether a widget had a
 * warnings row above the chart.
 */
function fit(plot: uPlot, el: HTMLElement, explicit: number | undefined) {
  const width = Math.floor(el.clientWidth) || 600;
  if (explicit !== undefined) {
    plot.setSize({ width, height: explicit });
    return;
  }
  const box = Math.floor(el.clientHeight);
  const legendEl = plot.root.querySelector<HTMLElement>(".u-legend");
  if (legendEl && box > 0) {
    // A chart with a dozen series wraps its legend onto many lines, and since the plot gets
    // what the legend leaves, an uncapped legend squeezed the plot to a strip (a widget six
    // rows tall drew a 20px chart under six lines of key). The legend scrolls instead, so the
    // picture always keeps the larger share of the box.
    legendEl.style.maxHeight = `${Math.floor(box * MAX_LEGEND_SHARE)}px`;
    legendEl.style.overflowY = "auto";
  }
  const legend = legendEl?.offsetHeight ?? 0;
  plot.setSize({ width, height: box > 0 ? Math.max(box - legend, MIN_HEIGHT) : FALLBACK_HEIGHT });
}

/** A uPlot line chart that follows its container's size. */
export function TimeseriesChart({ data, labels, xRange, height, syncKey }: TimeseriesChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const plotRef = useRef<uPlot | null>(null);
  const xRangeRef = useRef(xRange);
  const dataRef = useRef(data);
  const dark = useSyncExternalStore(subscribeDark, prefersDark, () => false);
  const seriesKey = JSON.stringify(labels);

  useEffect(() => {
    xRangeRef.current = xRange;
    dataRef.current = data;
  });

  // Build (and rebuild) the chart whenever something uPlot can't change in
  // place does. The latest data is read from a ref so this effect doesn't
  // depend on it; the effect below handles data-only updates.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const opts = buildOptions(JSON.parse(seriesKey) as string[], dark, el.clientWidth || 600, height ?? FALLBACK_HEIGHT, xRangeRef, syncKey);
    const plot = new uPlot(opts, dataRef.current as uPlot.AlignedData, el);
    plotRef.current = plot;
    // Once, now that there is a legend to measure; then on every resize.
    fit(plot, el, height);
    const ro = new ResizeObserver(() => fit(plot, el, height));
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot.destroy();
      plotRef.current = null;
    };
  }, [seriesKey, dark, height, syncKey]);

  useEffect(() => {
    plotRef.current?.setData(data as uPlot.AlignedData, true);
  }, [data, xRange]);

  return <div ref={containerRef} className="h-full w-full" data-testid="timeseries-chart" />;
}
