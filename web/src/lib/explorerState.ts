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
 * `agg`). Those are carried as they are, in `legacy`, for the server to
 * translate — it is the one translator, and a second one here would be a
 * second place to get "which of these does ozyd refuse?" wrong (ADR-0022).
 * The range and `live` are forgiving — a hand-edited or stale value degrades
 * to its default rather than failing — and serializing omits defaults so
 * ordinary links stay short.
 */
import { isRangePreset, type RangePreset, type TimeRange } from "./timeRange";

/** Everything that determines the explorer's view. */
export interface ExplorerState {
  /** The metricql query that is charted; "" means none has been run. */
  q: string;
  /**
   * M1's structured parameters, as a query string, when the link has them
   * and no `q`: "" otherwise. The page asks the server to translate them.
   */
  legacy: string;
  range: TimeRange;
  /** Whether a relative range re-queries every 10 s. */
  live: boolean;
}

const DEFAULT_PRESET: RangePreset = "1h";

/** The state of a fresh explorer: no query, last hour, live. */
export const DEFAULT_EXPLORER_STATE: ExplorerState = {
  q: "",
  legacy: "",
  range: { kind: "relative", preset: DEFAULT_PRESET },
  live: true,
};

/** The M1 parameters, in the order M1 wrote them. */
const LEGACY_PARAMS = ["metric", "filter", "by", "agg"] as const;

/**
 * An M1 link's parameters, exactly as sent, or "" when it has none. Not
 * trimmed or checked: the server refuses what it refuses, and the page says
 * so in the server's words.
 */
export function legacyParams(params: URLSearchParams): string {
  const out = new URLSearchParams();
  for (const k of LEGACY_PARAMS) {
    const v = params.get(k);
    if (v !== null && v !== "") out.set(k, v);
  }
  return out.toString();
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
    q,
    legacy: q ? "" : legacyParams(params),
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
  // Kept until the server has translated them, so a range change while it
  // does — or after it refused them — still names the same question.
  else for (const [k, v] of new URLSearchParams(state.legacy)) p.set(k, v);
  if (state.range.kind === "absolute") {
    p.set("from", String(state.range.from));
    p.set("to", String(state.range.to));
  } else if (state.range.preset !== DEFAULT_PRESET) {
    p.set("range", state.range.preset);
  }
  if (!state.live) p.set("live", "0");
  return p;
}
