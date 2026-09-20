import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "./metricsApi";

/**
 * Decides whether TanStack Query should retry a failed request. A 4xx means
 * the request itself is wrong (a bad filter, an unknown aggregator), so
 * asking again only delays showing the server's explanation; network errors
 * and 5xx get two more tries.
 */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status !== undefined && error.status >= 400 && error.status < 500) {
    return false;
  }
  return failureCount < 2;
}

/**
 * Creates the app's QueryClient. TanStack Query owns all server state
 * (docs/plan/ui.md §1): caching, de-duplication of identical in-flight
 * requests, and interval refetching that pauses while the tab is hidden.
 * Window-focus refetching is off because live views already poll.
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: shouldRetry, refetchOnWindowFocus: false, staleTime: 5_000 },
    },
  });
}
