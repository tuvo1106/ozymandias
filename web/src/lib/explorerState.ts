/**
 * The Metrics Explorer's query state and its URL encoding.
 *
 * The URL is the single source of truth for what the explorer shows
 * (docs/plan/ui.md §1: "all view state is URL-encoded so any view is
 * shareable"). The page never keeps a second copy in React state: it parses
 * the search params on every render and writes a new URL to change anything.
 * That makes reload, back/forward and deep links correct by construction.
 *
 * The encoding mirrors the query API's own parameters where it can
 * (`metric`, `filter=k:v,!k2:v2`, `by=k1,k2`, `agg`) so a URL is readable and
 * easy to translate into a curl command. (A consequence of copying the API's
 * comma-joined lists: a filter value cannot contain a comma, here or in the
 * API.) Parsing is forgiving — a hand-edited or stale link degrades to
 * defaults field by field rather than failing — and serializing omits
 * defaults so ordinary links stay short.
 */
import { isRangePreset, type RangePreset, type TimeRange } from "./timeRange";

/** Cross-series aggregators the query API accepts. */
export const AGGREGATORS = ["avg", "sum", "min", "max"] as const;

/** One cross-series aggregator. */
export type Aggregator = (typeof AGGREGATORS)[number];

/**
 * One tag filter. `value` may contain `*` wildcards (the server matches
 * them); `negate` turns `key:value` into `!key:value` (not equal).
 */
export interface TagFilter {
  key: string;
  value: string;
  negate: boolean;
}

/** Everything that determines the explorer's view. */
export interface ExplorerState {
  /** Metric name; "" means none chosen yet. */
  metric: string;
  filters: TagFilter[];
  /** Tag keys to group by, in the order chosen. */
  by: string[];
  agg: Aggregator;
  range: TimeRange;
  /** Whether a relative range re-queries every 10 s. */
  live: boolean;
}

const DEFAULT_PRESET: RangePreset = "1h";

/** The state of a fresh explorer: nothing chosen, last hour, avg, live. */
export const DEFAULT_EXPLORER_STATE: ExplorerState = {
  metric: "",
  filters: [],
  by: [],
  agg: "avg",
  range: { kind: "relative", preset: DEFAULT_PRESET },
  live: true,
};

/** Formats one filter the way the API and URL spell it: `k:v` or `!k:v`. */
export function formatFilter(f: TagFilter): string {
  return `${f.negate ? "!" : ""}${f.key}:${f.value}`;
}

/**
 * Parses one `k:v` / `!k:v` term. The key ends at the *first* colon, so
 * values may contain colons (`url:http://x`). Returns undefined for a term
 * without a colon or with an empty key or value.
 */
export function parseFilter(term: string): TagFilter | undefined {
  const t = term.trim();
  const negate = t.startsWith("!");
  const body = negate ? t.slice(1) : t;
  const i = body.indexOf(":");
  if (i <= 0 || i === body.length - 1) return undefined;
  return { key: body.slice(0, i), value: body.slice(i + 1), negate };
}

/** Formats a filter list as the API's comma-joined `filter` parameter. */
export function formatFilters(filters: readonly TagFilter[]): string {
  return filters.map(formatFilter).join(",");
}

function splitList(s: string | null): string[] {
  if (!s) return [];
  return s
    .split(",")
    .map((x) => x.trim())
    .filter((x) => x !== "");
}

function parseRange(params: URLSearchParams): TimeRange {
  // Number("") is 0, not NaN, so a blank ?from= would otherwise parse as a
  // valid absolute range starting at the epoch and query January 1970.
  const int = (name: string): number => {
    const raw = params.get(name);
    return raw === null || raw.trim() === "" ? NaN : Number(raw);
  };
  const from = int("from");
  const to = int("to");
  if (Number.isInteger(from) && Number.isInteger(to) && from < to) {
    return { kind: "absolute", from, to };
  }
  const preset = params.get("range") ?? "";
  return isRangePreset(preset) ? { kind: "relative", preset } : DEFAULT_EXPLORER_STATE.range;
}

/**
 * Reads explorer state from URL search params. Unknown or malformed values
 * fall back to the default for that field alone; duplicate filters and
 * group-by keys are dropped.
 */
export function parseExplorerState(params: URLSearchParams): ExplorerState {
  const agg = params.get("agg") ?? "";
  const filters: TagFilter[] = [];
  const seen = new Set<string>();
  for (const term of splitList(params.get("filter"))) {
    const f = parseFilter(term);
    if (f && !seen.has(formatFilter(f))) {
      seen.add(formatFilter(f));
      filters.push(f);
    }
  }
  return {
    metric: (params.get("metric") ?? "").trim(),
    filters,
    by: [...new Set(splitList(params.get("by")))],
    agg: (AGGREGATORS as readonly string[]).includes(agg) ? (agg as Aggregator) : DEFAULT_EXPLORER_STATE.agg,
    range: parseRange(params),
    live: params.get("live") !== "0",
  };
}

/**
 * Writes explorer state as URL search params, omitting fields at their
 * default. `parseExplorerState(serializeExplorerState(s))` equals `s` for any
 * state parseExplorerState can produce.
 */
export function serializeExplorerState(state: ExplorerState): URLSearchParams {
  const p = new URLSearchParams();
  if (state.metric) p.set("metric", state.metric);
  if (state.filters.length) p.set("filter", formatFilters(state.filters));
  if (state.by.length) p.set("by", state.by.join(","));
  if (state.agg !== DEFAULT_EXPLORER_STATE.agg) p.set("agg", state.agg);
  if (state.range.kind === "absolute") {
    p.set("from", String(state.range.from));
    p.set("to", String(state.range.to));
  } else if (state.range.preset !== DEFAULT_PRESET) {
    p.set("range", state.range.preset);
  }
  if (!state.live) p.set("live", "0");
  return p;
}
