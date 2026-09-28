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
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { useMemo } from "react";
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
import {
  rangeKey,
  REFRESH_INTERVAL_MS,
  SUGGESTION_LIMIT,
} from "./useMetricsApi";

/** Every stored dashboard, for the picker. */
export function useDashboardList(): UseQueryResult<DashboardList> {
  return useQuery({
    queryKey: ["dashboards", "list"],
    queryFn: ({ signal }) => fetchDashboards(fetch, signal),
  });
}

/** One stored dashboard by id. */
export function useDashboard(
  id: number | undefined,
): UseQueryResult<StoredDashboard> {
  return useQuery({
    queryKey: ["dashboards", "one", id],
    queryFn: ({ signal }) => fetchDashboard(id as number, fetch, signal),
    enabled: id !== undefined,
  });
}

/** The services any template covers. */
export function useServices(): UseQueryResult<ServiceList> {
  return useQuery({
    queryKey: ["dashboards", "services"],
    queryFn: ({ signal }) => fetchServices(fetch, signal),
  });
}

/** Every template instantiated for one service. */
export function useServiceDashboards(
  service: string,
): UseQueryResult<ServiceDashboard[]> {
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

  /**
   * True while the answer on screen belongs to a *previous* window or variable
   * binding. Not set by a background auto-refresh, which asks the same
   * question again and whose answer nobody is waiting for.
   */
  isRefreshing: boolean;
  /** Set when a request failed — a widget-level failure is in its result. */
  error: Error | null;
  /** The window the server actually evaluated, for the charts' x-axis. */
  range: { from: number; to: number } | undefined;
}

/**
 * Runs a dashboard's queries and hands each widget its own answers.
 *
 * **One cache entry for the whole dashboard, not one per chunk.** The chunks
 * only exist because a batch carries at most 50 queries; they were never
 * independently cacheable, since every chunk's key already contains
 * [[requestsKey]] — every query on the page — so editing one query invalidated
 * all of them anyway. Issuing them as one `useQuery` costs nothing and buys
 * the thing `useQueries` does not give: measured against 5.103.1,
 * `placeholderData: keepPreviousData` keeps data across a key change for
 * `useQuery` and **not** for `useQueries`, where the result comes back pending
 * with no data and `isPlaceholderData` false. Without it every widget on the
 * page blanks the moment the time range or a variable changes — the flicker
 * the option was there to prevent, quietly not happening.
 *
 * The chunks still run in parallel, and one that fails does not take the
 * others with it: `allSettled`, not `all`, for the same reason a batch reports
 * per query (ADR-0017). Only a dashboard whose every chunk failed has nothing
 * to draw, and that is the case that throws.
 *
 * Whether a *query* failed is not this hook's business: `/api/v1/query/batch`
 * answers 200 with a per-query status and a widget renders its own error.
 * `error` here is about the request.
 */
export function useDashboardData(
  requests: DashboardRequests,
  state: DashboardViewState,
  vars: Dashboard["template_vars"],
  id: string,
  now: () => number = Date.now,
): DashboardData {
  const key = requestsKey(requests);
  const bound = bindVars(vars, state);
  const live = state.live && state.range.kind === "relative";
  const chunks = requests.chunks;
  const batch = useQuery({
    // `id` is in the key because the cached *value* is keyed by widget id. Two
    // dashboards can hold the same queries in the same order under different
    // widget ids — a saved copy of another one, which this build invites by
    // making a template instance storable — and without this they share an
    // entry, `pairResults` files the answer under the first one's ids, and
    // every widget on the second draws nothing at all. Nothing corrects it
    // either: the entry is fresh for five seconds, and an absolute range has no
    // refetch interval to come back with the right answer.
    queryKey: [
      "dashboards",
      "batch",
      id,
      key,
      rangeKey(state.range),
      JSON.stringify(bound),
    ],
    queryFn: async ({ signal }) => {
      const range = resolveTimeRange(state.range, now());
      const settled = await Promise.allSettled(
        // The slots travel *with* the answer, so the pairing below never uses
        // the chunk a later render happens to hold. A kept answer outlives the
        // request that asked for it, and pairing it against a definition that
        // has since lost a widget would attribute result n to whichever widget
        // now sits at slot n — one widget showing another's numbers, which is
        // the worst failure a dashboard has.
        chunks.map(async (chunk) => ({
          slots: chunk,
          answer: await fetchBatch(
            chunk.map((s) => s.q),
            range,
            bound,
            fetch,
            signal,
          ),
        })),
      );
      const ok = settled.flatMap((r) =>
        r.status === "fulfilled" ? [r.value] : [],
      );
      const failed = settled.find((r) => r.status === "rejected");
      if (ok.length === 0 && failed) throw failed.reason;
      return { ok, error: failed ? (failed.reason as Error) : null };
    },
    enabled: chunks.length > 0,
    refetchInterval: live ? REFRESH_INTERVAL_MS : (false as const),
    // `keepPreviousData`, but only within one dashboard. Plain
    // `keepPreviousData` keeps whatever *this observer* last saw, whichever key
    // produced it — and an observer outlives a change of dashboard, because
    // `/dashboards/1 → /dashboards/2` is one route match and the second
    // definition is usually already cached, so nothing unmounts. The previous
    // dashboard's answer would then stand in for this one and be paired by
    // widget id, and two dashboards share ids far more often than they should:
    // the same template instantiated for two services has identical ids by
    // construction, so service B would briefly show service A's numbers.
    //
    // Naming the dashboard in the key is what makes this checkable — and is
    // also what created the hazard, since before it the two shared one entry
    // outright. The key says whose answer this is; this says whose answer may
    // stand in for it.
    placeholderData: (previous, previousQuery) =>
      previousQuery?.queryKey[ID_IN_KEY] === id ? previous : undefined,
  });
  const sketches = useDashboardSketches(
    requests.heatmaps,
    state,
    vars,
    id,
    now,
  );
  // Memoized so the widgets' own memos can hit. An unmemoized Map here is a new
  // object every render, and every `useMemo` keyed on it — down to the
  // `setData` that redraws each chart — fires with it.
  const byWidget = useMemo(
    () =>
      mergeWidgetResults(
        (batch.data?.ok ?? []).map((a) =>
          pairResults(a.slots, a.answer.results),
        ),
      ),
    [batch.data],
  );
  const answered = batch.data?.ok[0]?.answer;
  // A dashboard of nothing but heatmaps has no batch to take the window from,
  // so it comes from the first sketch that answered. Without this the x-axis
  // would sit on the requested window forever, which is right to within a
  // bucket and wrong in exactly the way the server's own `from`/`to` exist to
  // correct.
  const fromSketch = [...sketches.sketches.values()].find((s) => s.data)?.data;
  return {
    byWidget,
    sketches: sketches.sketches,
    // Either half of the page: a dashboard of nothing but heatmaps has no
    // batch at all, so reading only that one would leave it undimmed with
    // nothing to say it is showing the previous window — and on a mixed one
    // the dim would clear the moment the lines landed, while the heatmaps were
    // still answering the old question.
    isRefreshing: batch.isPlaceholderData || sketches.isRefreshing,
    error: (batch.error as Error | undefined) ?? batch.data?.error ?? null,
    range: answered
      ? { from: answered.from, to: answered.to }
      : fromSketch
        ? { from: fromSketch.from, to: fromSketch.to }
        : undefined,
  };
}

/**
 * Runs every heatmap's `dist:` query.
 *
 * One request per heatmap, because the endpoint answers one query — there is
 * no batch for distributions, and there should not be: two of them are a
 * hundred times the payload of two lines, so the saving a batch exists for
 * (one selection shared, ADR-0018) is dwarfed by what it would carry. They are
 * gathered under one `useQuery` for the reason the batch is: that is where
 * `keepPreviousData` works, so a heatmap does not blank on every change of
 * window.
 *
 * A failure is kept per widget rather than raised, because a `dist:` on a
 * metric that is not a distribution is a 400 about *that widget* and must not
 * blank the eleven beside it (ADR-0017) — which is what `allSettled` is for.
 * Unlike the batch, one heatmap's failure *is* a whole request failing, so
 * there is no per-query status to read it out of.
 */
export function useDashboardSketches(
  heatmaps: DashboardRequests["heatmaps"],
  state: DashboardViewState,
  vars: Dashboard["template_vars"],
  id: string,
  now: () => number = Date.now,
): { sketches: Map<string, SketchState>; isRefreshing: boolean } {
  const bound = bindVars(vars, state);
  const live = state.live && state.range.kind === "relative";
  const key = JSON.stringify(heatmaps.map((h) => h.q));
  const query = useQuery({
    // `id` for the same reason as the batch: the value is keyed by widget id.
    queryKey: [
      "dashboards",
      "sketches",
      id,
      key,
      rangeKey(state.range),
      JSON.stringify(bound),
    ],
    queryFn: async ({ signal }) => {
      const range = resolveTimeRange(state.range, now());
      const settled = await Promise.allSettled(
        heatmaps.map((h) => fetchSketch(h.q, range, bound, fetch, signal)),
      );
      // Keyed by widget so the grid can look one up without scanning. Last one
      // wins where two heatmaps share a widget id, which the server's own
      // validation makes impossible.
      const out = new Map<string, SketchState>();
      heatmaps.forEach((h, i) => {
        const r = settled[i];
        if (!r) return;
        out.set(
          h.widgetId,
          r.status === "fulfilled"
            ? { data: r.value }
            : { error: (r.reason as Error).message },
        );
      });
      return out;
    },
    enabled: heatmaps.length > 0,
    refetchInterval: live ? REFRESH_INTERVAL_MS : (false as const),
    // Only within one dashboard — see the batch above for what plain
    // `keepPreviousData` does across two.
    placeholderData: (previous, previousQuery) =>
      previousQuery?.queryKey[ID_IN_KEY] === id ? previous : undefined,
  });
  return {
    sketches: query.data ?? EMPTY_SKETCHES,
    isRefreshing: query.isPlaceholderData,
  };
}

/** What a dashboard with no heatmaps, or one still waiting, hands the grid. */
const EMPTY_SKETCHES: Map<string, SketchState> = new Map();

/** Where the dashboard id sits in both query keys above. */
const ID_IN_KEY = 2;
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
    queryFn: ({ signal }) =>
      fetchTagValues(metric, tag, SUGGESTION_LIMIT, fetch, signal),
    enabled: metric !== "" && tag !== "",
  });
  const viaCount = useQuery({
    queryKey: ["metrics", "tagValues", `${metric}.count`, tag],
    queryFn: ({ signal }) =>
      fetchTagValues(`${metric}.count`, tag, SUGGESTION_LIMIT, fetch, signal),
    enabled: metric !== "" && tag !== "" && direct.data?.length === 0,
  });
  return direct.data?.length ? direct.data : (viaCount.data ?? []);
}
