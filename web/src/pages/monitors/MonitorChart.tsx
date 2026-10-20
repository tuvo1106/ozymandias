/**
 * The monitor chart, on its own so only the pages that draw it load uPlot.
 */
import { useMemo } from "react";
import { TimeseriesChart } from "../../charts/TimeseriesChart";
import type { ChartAnnotations, Marker } from "../../lib/annotations";
import { backtestData, levelAnnotations, transitionMarkers, windowSpan } from "../../lib/monitorChart";
import type { Backtest, Levels } from "../../lib/monitorsApi";

/**
 * The evaluated line with its bands, the window an evaluation reads, and a
 * marker per transition. `markers` overrides the backtest's own (the detail
 * page draws what really happened over the line the backtest replays).
 */
export function MonitorChart({
  backtest,
  levels,
  evalWindow,
  markers,
  height = 260,
}: {
  backtest: Backtest;
  levels?: Levels;
  /** The range one evaluation at now reads, shaded. */
  evalWindow?: { from: number; to: number };
  markers?: Marker[];
  height?: number;
}) {
  const { data, labels } = useMemo(() => backtestData(backtest), [backtest]);
  const annotations = useMemo<ChartAnnotations>(
    () => ({
      ...(levels ? levelAnnotations(levels) : {}),
      spans: evalWindow ? [windowSpan(evalWindow)] : [],
      markers: markers ?? transitionMarkers(backtest.events),
    }),
    [levels, evalWindow, markers, backtest.events],
  );
  const xRange = useMemo<[number, number]>(
    () => [Date.parse(backtest.from) / 1000, Math.max(Date.parse(backtest.to) / 1000, evalWindow?.to ?? 0)],
    [backtest.from, backtest.to, evalWindow?.to],
  );
  if (labels.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-zinc-300 p-4 text-sm text-zinc-500 dark:border-zinc-700">
        No group had a value over this range, so there is no line to draw.
      </p>
    );
  }
  return (
    <div>
      <TimeseriesChart data={data} labels={labels} xRange={xRange} height={height} annotations={annotations} />
      {backtest.series_omitted > 0 ? (
        <p className="text-xs text-zinc-500">
          {backtest.series_omitted} more group(s) not drawn; their transitions are in the list.
        </p>
      ) : null}
    </div>
  );
}

