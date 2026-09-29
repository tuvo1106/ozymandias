/**
 * The Metrics Explorer's query state and its URL encoding.
 *
 * The URL is the single source of truth for what the explorer *charts*
 * (docs/plan/ui.md §1: "all view state is URL-encoded so any view is
 * shareable"): the query that was last run, the time range, and whether it
 * refreshes. The query box holds a draft beside it, and running the draft is
 * what writes it here — so a link is always the question its chart answers,
 * never half of one that was being typed (ADR-0022).
 *
 * `q` is the query language as `/api/v1/query` takes it. M1's explorer wrote
 * the structured parameters instead (`metric`, `filter=k:v,!k2:v2`, `by`,
 * `agg`); a link from then is translated into the same query the server would
 * have run for it, so it still charts what it charted, and the first change
 * rewrites it as `q`. Parsing is forgiving — a hand-edited or stale link
 * degrades to defaults field by field rather than failing — and serializing
 * omits defaults so ordinary links stay short.
 */
import { isRangePreset, type RangePreset, type TimeRange } from "./timeRange";

/** Everything that determines the explorer's view. */
export interface ExplorerState {
  /** The metricql query that is charted; "" means none has been run. */
  q: string;
  range: TimeRange;
  /** Whether a relative range re-queries every 10 s. */
  live: boolean;
}

const DEFAULT_PRESET: RangePreset = "1h";

/** The state of a fresh explorer: no query, last hour, live. */
export const DEFAULT_EXPLORER_STATE: ExplorerState = {
  q: "",
  range: { kind: "relative", preset: DEFAULT_PRESET },
  live: true,
};

/** The aggregators M1's `agg` parameter produced. Anything else read as avg then, and still does. */
const LEGACY_AGGREGATORS = new Set(["avg", "sum", "min", "max"]);

function splitList(s: string | null): string[] {
  if (!s) return [];
  return s
    .split(",")
    .map((x) => x.trim())
    .filter((x) => x !== "");
}

/**
 * The query an M1 link (`?metric=…&filter=…&by=…&agg=…`) asked for, or ""
 * when it names no metric.
 *
 * The same translation the server makes for those parameters (api.md, "The
 * M1 structured parameters"), done here so the text lands in the query box
 * where it can be read and edited. A filter term without a key and a value
 * is dropped, as M1's explorer dropped it; anything the parser would refuse
 * — a brace in a value — is kept, so the box shows the parse error rather
 * than the page quietly charting a different query.
 */
export function legacyQuery(params: URLSearchParams): string {
  const metric = (params.get("metric") ?? "").trim();
  if (!metric) return "";
  const agg = params.get("agg") ?? "";
  const filters: string[] = [];
  for (const term of splitList(params.get("filter"))) {
    const negate = term.startsWith("!");
    const body = negate ? term.slice(1) : term;
    const i = body.indexOf(":");
    if (i <= 0 || i === body.length - 1) continue;
    const f = `${negate ? "!" : ""}${body}`;
    if (!filters.includes(f)) filters.push(f);
  }
  const by = [...new Set(splitList(params.get("by")))];
  return (
    `${LEGACY_AGGREGATORS.has(agg) ? agg : "avg"}:${metric}{${filters.length ? filters.join(",") : "*"}}` +
    (by.length ? ` by {${by.join(",")}}` : "")
  );
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
 * Reads explorer state from URL search params. `q` wins over M1's
 * parameters when a link carries both: it is the newer spelling, and the
 * only one this explorer writes.
 */
export function parseExplorerState(params: URLSearchParams): ExplorerState {
  const q = (params.get("q") ?? "").trim();
  return {
    q: q || legacyQuery(params),
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
  if (state.q) p.set("q", state.q);
  if (state.range.kind === "absolute") {
    p.set("from", String(state.range.from));
    p.set("to", String(state.range.to));
  } else if (state.range.preset !== DEFAULT_PRESET) {
    p.set("range", state.range.preset);
  }
  if (!state.live) p.set("live", "0");
  return p;
}
