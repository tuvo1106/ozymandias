/**
 * The query editor's server state: completions from the store, and ozyd's
 * verdict on the text.
 *
 * Both follow the rule the rest of the editor does — an answer is shown only
 * for the question it answers. TanStack's `keepPreviousData` is the thing to
 * watch here: it hands back the *previous key's* data while the new key is
 * fetching, which is exactly what makes a list feel stable and exactly what
 * makes it wrong. So neither hook passes placeholder data on as an answer:
 * completions treat it as loading, and validation compares the text the
 * answer was for.
 */
import { useQuery } from "@tanstack/react-query";
import { storeCompletions, localCompletions, type CompletionList } from "./completions";
import { fetchMetricNames, fetchTagKeys, fetchTagValues } from "./metricsApi";
import type { CompletionContext } from "./queryContext";
import { fetchValidation, validationState, type ValidationState } from "./queryValidation";
import { useDebouncedValue } from "./useDebouncedValue";
import { SUGGESTION_LIMIT } from "./useMetricsApi";

/** How long typing must pause before the store or the validator is asked. */
export const EDITOR_DEBOUNCE_MS = 250;

/**
 * Names from one lookup, with a fallback to `<metric>.count` when the first
 * answer is empty — a distribution's tags live on its `.count` series (wire
 * protocol §D), so `p95:http.request.duration{` would otherwise offer no keys.
 * The same rule as the variable bar's [[useVariableValues]].
 */
function useWithCountFallback(
  key: readonly unknown[],
  fetcher: (metric: string, signal: AbortSignal) => Promise<string[]>,
  metric: string,
  enabled: boolean,
) {
  const direct = useQuery({
    queryKey: [...key, metric],
    queryFn: ({ signal }) => fetcher(metric, signal),
    enabled,
  });
  const viaCount = useQuery({
    queryKey: [...key, `${metric}.count`],
    queryFn: ({ signal }) => fetcher(`${metric}.count`, signal),
    enabled: enabled && direct.data?.length === 0,
  });
  if (direct.error) return { loading: false, error: direct.error as Error };
  if (direct.data === undefined) return { loading: direct.isFetching || direct.isPending };
  if (direct.data.length > 0) return { loading: false, data: direct.data };
  if (viaCount.error) return { loading: false, error: viaCount.error as Error };
  if (viaCount.data === undefined) return { loading: true };
  return { loading: false, data: viaCount.data };
}

/** The completion list for a context. */
export function useCompletions(ctx: CompletionContext, variables: readonly string[]): CompletionList {
  const local = localCompletions(ctx, variables);
  const prefix = ctx.kind === "metric" ? ctx.prefix : "";
  const debouncedPrefix = useDebouncedValue(prefix, EDITOR_DEBOUNCE_MS);
  const names = useQuery({
    queryKey: ["metrics", "names", debouncedPrefix],
    queryFn: ({ signal }) => fetchMetricNames(debouncedPrefix, SUGGESTION_LIMIT, fetch, signal),
    enabled: ctx.kind === "metric",
  });
  const metric = "metric" in ctx ? ctx.metric : "";
  const wantsKeys = (ctx.kind === "tagKey" || ctx.kind === "groupKey") && metric !== "";
  const keys = useWithCountFallback(
    ["metrics", "tagKeys"],
    (m, signal) => fetchTagKeys(m, fetch, signal),
    metric,
    wantsKeys,
  );
  const tagKey = ctx.kind === "tagValue" ? ctx.key : "";
  const values = useWithCountFallback(
    ["metrics", "tagValues", tagKey],
    (m, signal) => fetchTagValues(m, tagKey, SUGGESTION_LIMIT, fetch, signal),
    metric,
    ctx.kind === "tagValue" && metric !== "" && tagKey !== "",
  );

  if (local) return local;
  switch (ctx.kind) {
    case "metric":
      // Only an answer to the prefix on screen. While typing outruns the
      // debounce, the cached answer is for a shorter prefix and filtering it
      // would be right by luck; while a new prefix fetches, it may be for a
      // different one entirely.
      if (debouncedPrefix !== prefix) return { status: "loading" };
      return storeCompletions(ctx, {
        loading: names.isFetching || names.isPending,
        data: names.data,
        error: names.error as Error | null,
      });
    case "tagKey":
    case "groupKey":
      return storeCompletions(ctx, keys);
    case "tagValue":
      return storeCompletions(ctx, values);
    default:
      return { status: "none" };
  }
}

/**
 * ozyd's verdict on `text`, as a [[ValidationState]]. Asks once typing has
 * paused; until the answer for exactly this text arrives, the state is
 * `checking` — never the previous text's verdict.
 */
export function useQueryValidation(text: string): ValidationState {
  const asked = useDebouncedValue(text, EDITOR_DEBOUNCE_MS);
  const query = useQuery({
    queryKey: ["query", "validate", asked],
    queryFn: ({ signal }) => fetchValidation(asked, fetch, signal),
    enabled: asked.trim() !== "",
    // A verdict about a string never changes; asking again is only waste.
    staleTime: Infinity,
    retry: false,
  });
  return validationState(text, {
    asked,
    answer: query.data,
    error: query.error as Error | null,
  });
}
