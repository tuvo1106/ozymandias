import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { UNAUTHORIZED_EVENT } from "./authApi";
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

function onUnauthorized(error: unknown): void {
  if (error instanceof ApiError && error.status === 401) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
}

/**
 * Creates the app's QueryClient. TanStack Query owns all server state
 * (docs/plan/ui.md §1): caching, de-duplication of identical in-flight
 * requests, and interval refetching that pauses while the tab is hidden.
 * Window-focus refetching is off because live views already poll.
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    // A 401 from any query means the session is gone: tell the login gate, which
    // is the one place that knows how to ask for a password again.
    queryCache: new QueryCache({ onError: onUnauthorized }),
    // Saving a dashboard or a monitor is a mutation: without this a session that
    // ended since the page loaded shows "save failed" and no way back in.
    mutationCache: new MutationCache({ onError: onUnauthorized }),
    defaultOptions: {
      queries: { retry: shouldRetry, refetchOnWindowFocus: false, staleTime: 5_000 },
    },
  });
}
