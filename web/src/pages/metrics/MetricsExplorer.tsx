import { useMemo } from "react";
import { useSearchParams } from "react-router";
import { TimeseriesChart } from "../../charts/TimeseriesChart";
import { alignSeries, seriesLabel } from "../../lib/chartData";
import {
  AGGREGATORS,
  parseExplorerState,
  serializeExplorerState,
  type Aggregator,
  type ExplorerState,
} from "../../lib/explorerState";
import type { QueryResult } from "../../lib/metricsApi";
import { REFRESH_INTERVAL_MS, shouldAutoRefresh, useExplorerQuery } from "../../lib/useMetricsApi";
import { FilterBar } from "./FilterBar";
import { GroupBySelect } from "./GroupBySelect";
import { MetricPicker } from "./MetricPicker";
import { TimeRangePicker } from "./TimeRangePicker";

/**
 * Metrics Explorer v0 (M1 spec §3.5): pick a metric, filter and group it by
 * tags, choose a cross-series aggregator and a time range, and chart it.
 *
 * All query state lives in the URL (see lib/explorerState.ts). This component
 * derives the state from the search params on every render and each control
 * writes a new URL; there is no useState copy of the query to drift out of
 * sync with the address bar.
 */
export function MetricsExplorer() {
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseExplorerState(params), [params]);
  const update = (patch: Partial<ExplorerState>) => setParams(serializeExplorerState({ ...state, ...patch }));
  const query = useExplorerQuery(state);
  const autoRefresh = shouldAutoRefresh(state);

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold">Metrics Explorer</h1>

      <section aria-label="Query" className="flex flex-col gap-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
        <div className="flex flex-wrap items-center gap-2">
          <select
            aria-label="Aggregator"
            value={state.agg}
            onChange={(e) => update({ agg: e.target.value as Aggregator })}
            className="rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-900"
          >
            {AGGREGATORS.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
          <span className="text-sm text-zinc-500">of</span>
          <MetricPicker key={state.metric} metric={state.metric} onChange={(metric) => update({ metric })} />
        </div>
        <FilterBar metric={state.metric} filters={state.filters} onChange={(filters) => update({ filters })} />
        <div className="flex flex-wrap items-center gap-4">
          <GroupBySelect metric={state.metric} by={state.by} onChange={(by) => update({ by })} />
          <TimeRangePicker
            key={JSON.stringify(state.range)}
            range={state.range}
            onChange={(range) => update({ range })}
          />
          <label
            className="flex items-center gap-2 text-sm"
            title={state.range.kind === "absolute" ? "A fixed time window doesn't change, so there is nothing to refresh." : undefined}
          >
            <input
              type="checkbox"
              checked={autoRefresh}
              disabled={state.range.kind === "absolute"}
              onChange={(e) => update({ live: e.target.checked })}
            />
            Auto-refresh every {REFRESH_INTERVAL_MS / 1000}s
          </label>
          {query.isFetching && <span className="text-xs text-zinc-500">Updating…</span>}
        </div>
      </section>

      <section aria-label="Chart" className="rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
        <ChartArea metric={state.metric} query={query} />
      </section>
    </div>
  );
}

interface ChartAreaProps {
  metric: string;
  query: ReturnType<typeof useExplorerQuery>;
}

/** The chart, or whichever empty / loading / error state applies. */
function ChartArea({ metric, query }: ChartAreaProps) {
  if (!metric) {
    return <p className="text-zinc-500">Choose a metric to chart it.</p>;
  }
  return (
    <>
      {query.isError && (
        <p role="alert" className="mb-3 text-red-600 dark:text-red-400">
          {query.error.message}
        </p>
      )}
      {query.isPending && !query.isError && (
        <p role="status" className="text-zinc-500">
          Loading {metric}…
        </p>
      )}
      {query.data && <ResultChart result={query.data} />}
    </>
  );
}

function ResultChart({ result }: { result: QueryResult }) {
  const data = useMemo(() => alignSeries(result.series), [result]);
  const labels = useMemo(() => result.series.map(seriesLabel), [result]);
  // Start at the first bucket, not at result.from: the server floors the range
  // start to the interval, so whenever from % interval != 0 the first bucket
  // begins *before* from and would fall outside the scale and never be drawn.
  const xRange = useMemo((): [number, number] => {
    const first = data[0]?.[0];
    return [first !== undefined ? first : result.from, result.to];
  }, [data, result]);
  if (result.series.length === 0) {
    return <p className="text-zinc-500">No data for this query in the selected time range.</p>;
  }
  return (
    <>
      <p className="mb-2 text-xs text-zinc-500">
        {result.series.length} series · {result.interval}s buckets
      </p>
      <TimeseriesChart data={data} labels={labels} xRange={xRange} />
    </>
  );
}
