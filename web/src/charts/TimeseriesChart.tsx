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
 */
import { useEffect, useRef, useSyncExternalStore, type RefObject } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { formatValue, seriesColor, type AlignedData } from "../lib/chartData";

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
  /** Canvas height in CSS pixels. */
  height?: number;
}

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
      })),
    ],
    legend: { live: true },
    cursor: { drag: { x: false, y: false } },
  };
}

/** A uPlot line chart that follows its container's width. */
export function TimeseriesChart({ data, labels, xRange, height = 320 }: TimeseriesChartProps) {
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
    const opts = buildOptions(JSON.parse(seriesKey) as string[], dark, el.clientWidth || 600, height, xRangeRef);
    const plot = new uPlot(opts, dataRef.current as uPlot.AlignedData, el);
    plotRef.current = plot;
    const ro = new ResizeObserver(([entry]) => {
      if (entry) plot.setSize({ width: Math.floor(entry.contentRect.width), height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      plot.destroy();
      plotRef.current = null;
    };
  }, [seriesKey, dark, height]);

  useEffect(() => {
    plotRef.current?.setData(data as uPlot.AlignedData, true);
  }, [data, xRange]);

  return <div ref={containerRef} className="w-full" data-testid="timeseries-chart" />;
}
