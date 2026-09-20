/**
 * TanStack Query hooks over metricsApi.ts — the explorer's server state.
 *
 * Query keys are built from *what is being asked*, never from resolved
 * timestamps. For a relative range the key holds the preset ("1h"), and the
 * query function resolves it against the clock each time it runs. That is
 * what makes auto-refresh work: a refetch re-runs the function, which asks
 * for the window ending *now*; if the key held from/to, every refresh would
 * re-request the same frozen hour. It also means changing any control is a
 * new key (and a new cache entry), while a refresh is not.
 */
import { keepPreviousData, useQuery, type UseQueryResult } from "@tanstack/react-query";
import { formatFilters, type ExplorerState } from "./explorerState";
import { fetchMetricNames, fetchQuery, fetchTagKeys, fetchTagValues, type QueryResult } from "./metricsApi";
import { resolveTimeRange, type TimeRange } from "./timeRange";

/** How often a live view re-queries. */
export const REFRESH_INTERVAL_MS = 10_000;

/** How many suggestions an autocomplete asks the server for. */
export const SUGGESTION_LIMIT = 50;

/** Metric names starting with `prefix`, for the metric autocomplete. */
export function useMetricNames(prefix: string): UseQueryResult<string[]> {
  return useQuery({
    queryKey: ["metrics", "names", prefix],
    queryFn: ({ signal }) => fetchMetricNames(prefix, SUGGESTION_LIMIT, fetch, signal),
    placeholderData: keepPreviousData,
  });
}

/** Tag keys of a metric; idle until a metric is chosen. */
export function useTagKeys(metric: string): UseQueryResult<string[]> {
  return useQuery({
    queryKey: ["metrics", "tagKeys", metric],
    queryFn: ({ signal }) => fetchTagKeys(metric, fetch, signal),
    enabled: metric !== "",
  });
}

/** Values of one tag key on a metric; idle until both are known. */
export function useTagValues(metric: string, key: string): UseQueryResult<string[]> {
  return useQuery({
    queryKey: ["metrics", "tagValues", metric, key],
    queryFn: ({ signal }) => fetchTagValues(metric, key, SUGGESTION_LIMIT, fetch, signal),
    enabled: metric !== "" && key !== "",
  });
}

/** A stable cache-key fragment for a range: the preset name, or both bounds. */
export function rangeKey(range: TimeRange): string {
  return range.kind === "relative" ? range.preset : `${range.from}-${range.to}`;
}

/**
 * Whether a view should poll: only a live, relative range changes over
 * time. An absolute window's answer is fixed (bar late-arriving data), so
 * polling it would only cost requests.
 */
export function shouldAutoRefresh(state: Pick<ExplorerState, "live" | "range">): boolean {
  return state.live && state.range.kind === "relative";
}

/**
 * The explorer's chart query. Idle without a metric; refetches every
 * REFRESH_INTERVAL_MS while shouldAutoRefresh holds — and TanStack Query
 * pauses interval refetches while the tab is hidden
 * (`refetchIntervalInBackground` defaults to false), so a background tab
 * costs the server nothing. The previous result stays on screen while a new
 * one loads, so the chart never blanks between refreshes.
 */
export function useExplorerQuery(state: ExplorerState, now: () => number = Date.now): UseQueryResult<QueryResult> {
  return useQuery({
    queryKey: ["metrics", "query", state.metric, formatFilters(state.filters), state.by.join(","), state.agg, rangeKey(state.range)],
    queryFn: ({ signal }) => fetchQuery(state, resolveTimeRange(state.range, now()), fetch, signal),
    enabled: state.metric !== "",
    refetchInterval: shouldAutoRefresh(state) ? REFRESH_INTERVAL_MS : false,
    placeholderData: keepPreviousData,
  });
}
