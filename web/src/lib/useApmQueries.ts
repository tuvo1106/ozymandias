/**
 * The APM service table as a query, shared by the APM pages and the
 * service_summary widget. One definition, so the cache key and the window
 * resolution cannot drift apart between copies: they share a cache entry.
 */
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { fetchServices, type RedTable } from "./apmApi";
import { apmWindow } from "./apmState";
import type { TimeRange } from "./timeRange";
import { rangeKey } from "./useMetricsApi";

/** GET /api/v1/services for an env ("" for every env) over a range. */
export function useServiceTable(env: string, range: TimeRange, opts: { refetchInterval?: number } = {}): UseQueryResult<RedTable> {
  return useQuery({
    queryKey: ["apm", "services", env, rangeKey(range)],
    queryFn: ({ signal }) => fetchServices(env, apmWindow(range, Date.now()), fetch, signal),
    retry: false,
    refetchInterval: opts.refetchInterval,
  });
}
