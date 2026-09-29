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
import type { ExplorerState } from "./explorerState";
import { fetchLegacyQuery, fetchQuery, type QueryResult } from "./metricsApi";
import { resolveTimeRange, type TimeRange } from "./timeRange";

/** How often a live view re-queries. */
export const REFRESH_INTERVAL_MS = 10_000;

/** How many suggestions an autocomplete asks the server for. */
export const SUGGESTION_LIMIT = 50;

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
 * An answer, with the query text it answers.
 *
 * `keepPreviousData` hands the *previous* key's answer to a new query while
 * that one loads, which keeps the chart from blanking — and would draw one
 * query's lines under another's text. Carrying `asked` lets the page tell the
 * two apart without trusting a flag about which render it is in.
 */
export interface ExplorerAnswer {
  asked: string;
  result: QueryResult;
}

/** The cache key of the explorer's answer to `q` over `range`. */
export function explorerKey(q: string, range: TimeRange): readonly unknown[] {
  return ["metrics", "query", q, rangeKey(range)];
}

/**
 * The explorer's chart query. Idle without a query; refetches every
 * REFRESH_INTERVAL_MS while shouldAutoRefresh holds — and TanStack Query
 * pauses interval refetches while the tab is hidden
 * (`refetchIntervalInBackground` defaults to false), so a background tab
 * costs the server nothing. The previous answer stays available while a new
 * one loads, so the chart never blanks between queries.
 *
 * Not retried: a 400 is the query's fault and says so the first time, and a
 * 503 is "out of time", which asking again at once makes more likely.
 */
export function useExplorerQuery(
  state: ExplorerState,
  now: () => number = Date.now,
): UseQueryResult<ExplorerAnswer> {
  return useQuery({
    queryKey: explorerKey(state.q, state.range),
    queryFn: async ({ signal }) => ({
      asked: state.q,
      result: await fetchQuery(state.q, resolveTimeRange(state.range, now()), fetch, signal),
    }),
    enabled: state.q !== "",
    retry: false,
    refetchInterval: shouldAutoRefresh(state) ? REFRESH_INTERVAL_MS : false,
    placeholderData: keepPreviousData,
  });
}

/**
 * The server's translation of an M1 link: its answer to the link's
 * structured parameters, whose `query` is what they mean in the query
 * language. Idle unless the link has them and no `q`. Not retried, for the
 * same reasons as [[useExplorerQuery]], and not refetched: the page replaces
 * the parameters with the translation as soon as it has one.
 */
export function useLegacyTranslation(
  state: ExplorerState,
  now: () => number = Date.now,
): UseQueryResult<QueryResult> {
  return useQuery({
    queryKey: ["metrics", "legacy", state.legacy, rangeKey(state.range)],
    queryFn: ({ signal }) => fetchLegacyQuery(state.legacy, resolveTimeRange(state.range, now()), fetch, signal),
    enabled: state.q === "" && state.legacy !== "",
    retry: false,
    staleTime: Infinity,
  });
}
