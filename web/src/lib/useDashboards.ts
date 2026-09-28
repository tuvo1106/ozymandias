/**
 * TanStack Query hooks for the dashboard pages.
 *
 * Same key discipline as useMetricsApi.ts: a key holds *what is being asked*,
 * never a resolved timestamp, so a relative range re-resolves against the
 * clock on every refetch instead of re-requesting the same frozen hour.
 *
 * The batch is issued as several queries rather than one, because a dashboard
 * can hold more queries than a batch may carry. They run in parallel and each
 * chunk is its own cache entry, so a dashboard that grows past a chunk
 * boundary does not invalidate the chunks before it.
 */
import { keepPreviousData, useQueries, useQuery, type UseQueryResult } from "@tanstack/react-query";
import type { Dashboard, StoredDashboard } from "./dashboard";
import {
  mergeWidgetResults,
  pairResults,
  requestsKey,
  type DashboardRequests,
} from "./dashboardQueries";
import { bindVars, type DashboardViewState } from "./dashboardState";
import {
  fetchBatch,
  fetchDashboard,
  fetchDashboards,
  fetchServiceDashboards,
  fetchServices,
  fetchSketch,
  type BatchResult,
  type DashboardList,
  type ServiceDashboard,
  type ServiceList,
  type SketchResponse,
} from "./dashboardsApi";
import { resolveTimeRange } from "./timeRange";
import { fetchTagValues } from "./metricsApi";
import { rangeKey, REFRESH_INTERVAL_MS, SUGGESTION_LIMIT } from "./useMetricsApi";

/** Every stored dashboard, for the picker. */
export function useDashboardList(): UseQueryResult<DashboardList> {
  return useQuery({ queryKey: ["dashboards", "list"], queryFn: ({ signal }) => fetchDashboards(fetch, signal) });
}

/** One stored dashboard by id. */
export function useDashboard(id: number | undefined): UseQueryResult<StoredDashboard> {
  return useQuery({
    queryKey: ["dashboards", "one", id],
    queryFn: ({ signal }) => fetchDashboard(id as number, fetch, signal),
    enabled: id !== undefined,
  });
}

/** The services any template covers. */
export function useServices(): UseQueryResult<ServiceList> {
  return useQuery({ queryKey: ["dashboards", "services"], queryFn: ({ signal }) => fetchServices(fetch, signal) });
}

/** Every template instantiated for one service. */
export function useServiceDashboards(service: string): UseQueryResult<ServiceDashboard[]> {
  return useQuery({
    queryKey: ["dashboards", "service", service],
    queryFn: ({ signal }) => fetchServiceDashboards(service, fetch, signal),
    enabled: service !== "",
  });
}

/** One heatmap's answer: the sketch, or why there isn't one. */
export interface SketchState {
  data?: SketchResponse;
  /**
   * The server's own sentence. A string rather than an Error because a widget
   * draws a message and nothing else, and `/api/v1/query/sketch` answers a bad
   * query with a 400 whose body explains it.
   */
  error?: string;
}

/** What a dashboard's widgets need to draw themselves. */
export interface DashboardData {
  /** Results by widget id, then by the query's index within that widget. */
  byWidget: Map<string, Map<number, BatchResult>>;
  /** Sketches by widget id; only heatmap widgets have an entry. */
  sketches: Map<string, SketchState>;
  /** True while nothing has arrived yet; a refetch keeps the old answer. */
  isPending: boolean;
  /** Set when a whole chunk failed — a widget-level failure is in its result. */
  error: Error | null;
  /** The window the server actually evaluated, for the charts' x-axis. */
  range: { from: number; to: number } | undefined;
}

/**
 * Runs a dashboard's queries and hands each widget its own answers.
 *
 * Whether a query *failed* is not this hook's business: `/api/v1/query/batch`
 * answers 200 with a per-query status, and a widget renders its own error
 * (ADR-0017). `error` here means the request failed — the server is down, or
 * the window was rejected — which is the case where no widget can draw.
 */
export function useDashboardData(
  requests: DashboardRequests,
  state: DashboardViewState,
  vars: Dashboard["template_vars"],
  now: () => number = Date.now,
): DashboardData {
  const key = requestsKey(requests);
  const bound = bindVars(vars, state);
  const live = state.live && state.range.kind === "relative";
  const results = useQueries({
    queries: requests.chunks.map((chunk, i) => ({
      queryKey: ["dashboards", "batch", key, i, rangeKey(state.range), JSON.stringify(bound)],
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        fetchBatch(chunk.map((s) => s.q), resolveTimeRange(state.range, now()), bound, fetch, signal),
      refetchInterval: live ? REFRESH_INTERVAL_MS : (false as const),
      placeholderData: keepPreviousData,
    })),
  });
  const byWidget = mergeWidgetResults(
    results.map((r, i) =>
      r.data ? pairResults(requests.chunks[i] ?? [], r.data.results) : new Map<string, Map<number, BatchResult>>(),
    ),
  );
  const sketches = useDashboardSketches(requests.heatmaps, state, vars, now);
  const answered = results.find((r) => r.data);
  const range = answered?.data ? { from: answered.data.from, to: answered.data.to } : undefined;
  // A dashboard of nothing but heatmaps has no batch to take the window from,
  // so it comes from the first sketch that answered. Without this the x-axis
  // would sit on the requested window forever, which is right to within a
  // bucket and wrong in exactly the way the server's own `from`/`to` exist to
  // correct.
  const fromSketch = [...sketches.values()].find((s) => s.data)?.data;
  return {
    byWidget,
    sketches,
    isPending: results.length > 0 && results.every((r) => r.isPending),
    error: (results.find((r) => r.error)?.error as Error | undefined) ?? null,
    range: range ?? (fromSketch ? { from: fromSketch.from, to: fromSketch.to } : undefined),
  };
}

/**
 * Runs each heatmap's `dist:` query.
 *
 * One request per heatmap, because the endpoint answers one query — there is
 * no batch for distributions, and there should not be: two of them are a
 * hundred times the payload of two lines, so the saving a batch exists for
 * (one selection shared, ADR-0018) is dwarfed by what it would carry.
 *
 * A failure is kept per widget rather than raised, because a `dist:` on a
 * metric that is not a distribution is a 400 about *that widget* and must not
 * blank the eleven beside it (ADR-0017). The batch gets this for free from its
 * per-query status; here it has to be caught.
 */
export function useDashboardSketches(
  heatmaps: DashboardRequests["heatmaps"],
  state: DashboardViewState,
  vars: Dashboard["template_vars"],
  now: () => number = Date.now,
): Map<string, SketchState> {
  const bound = bindVars(vars, state);
  const live = state.live && state.range.kind === "relative";
  const results = useQueries({
    queries: heatmaps.map((h) => ({
      queryKey: ["dashboards", "sketch", h.q, rangeKey(state.range), JSON.stringify(bound)],
      queryFn: ({ signal }: { signal: AbortSignal }) => fetchSketch(h.q, resolveTimeRange(state.range, now()), bound, fetch, signal),
      refetchInterval: live ? REFRESH_INTERVAL_MS : (false as const),
      placeholderData: keepPreviousData,
    })),
  });
  const out = new Map<string, SketchState>();
  heatmaps.forEach((h, i) => {
    const r = results[i];
    if (!r) return;
    // Last one wins where two heatmaps share a widget id, which the server's
    // validation makes impossible — the map is keyed by widget so the grid can
    // look one up without scanning.
    out.set(h.widgetId, { data: r.data, error: r.error ? (r.error as Error).message : undefined });
  });
  return out;
}

/**
 * The values a template variable's tag takes, for one selector.
 *
 * Falls back to `<metric>.count` when the metric itself has no such tag.
 * A distribution's tags live on its `.count` series — its own name addresses
 * sketches, which carry no tag index (wire protocol §D) — so a dashboard whose
 * first metric is a latency distribution would otherwise offer an empty
 * dropdown. The fallback only costs a second request in the case where the
 * first came back empty, which is also the only case where it can help.
 */
export function useVariableValues(metric: string, tag: string): string[] {
  const direct = useQuery({
    queryKey: ["metrics", "tagValues", metric, tag],
    queryFn: ({ signal }) => fetchTagValues(metric, tag, SUGGESTION_LIMIT, fetch, signal),
    enabled: metric !== "" && tag !== "",
  });
  const viaCount = useQuery({
    queryKey: ["metrics", "tagValues", `${metric}.count`, tag],
    queryFn: ({ signal }) => fetchTagValues(`${metric}.count`, tag, SUGGESTION_LIMIT, fetch, signal),
    enabled: metric !== "" && tag !== "" && direct.data?.length === 0,
  });
  return direct.data?.length ? direct.data : (viaCount.data ?? []);
}
