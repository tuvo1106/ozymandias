/**
 * A dashboard view's state and its URL encoding.
 *
 * Same rule as the explorer (docs/plan/ui.md §1): the URL is the only copy, so
 * reload, back/forward and a pasted link all show the same thing. What a
 * dashboard adds over the explorer is the **variable bar** — one selector per
 * `template_vars` entry — and its encoding has to survive a definition that
 * changes underneath a saved link.
 *
 * Variables are spelled `var.<name>=<value>` to match the query API's own
 * parameters, and a *missing* parameter is not the same as an empty one:
 *
 *   - absent → the variable's `default` from the definition;
 *   - `var.env=` (empty) → explicitly every value, which resolves to no
 *     filter at all and is what clearing a selector means;
 *   - `var.env=prod` → that one value.
 *
 * The distinction matters because "I cleared this" and "I have not chosen"
 * differ once a definition ships a default: without it, clearing a selector
 * would snap back to the default on the next render and the selector would be
 * unusable. The explorer has the same shape of bug in its `live` flag, which
 * is why that one is `live=0` rather than an absent parameter.
 */
import type { TemplateVar } from "./dashboard";
import { isRangePreset, type RangePreset, type TimeRange } from "./timeRange";

const DEFAULT_PRESET: RangePreset = "1h";

/** Everything that determines what a dashboard view shows. */
export interface DashboardViewState {
  range: TimeRange;
  /** Whether a relative range re-queries on the refresh interval. */
  live: boolean;
  /**
   * The chosen value per variable name. A name absent from the map has not
   * been chosen; a name mapped to "" is explicitly "all".
   */
  vars: Record<string, string>;
}

/** A fresh view: the last hour, live, nothing chosen. */
export const DEFAULT_VIEW_STATE: DashboardViewState = {
  range: { kind: "relative", preset: DEFAULT_PRESET },
  live: true,
  vars: {},
};

/** The prefix a variable takes in the URL, matching the query API's own. */
export const VAR_PREFIX = "var.";

function parseRange(params: URLSearchParams): TimeRange {
  // Number("") is 0, so a blank ?from= would otherwise be a valid absolute
  // range starting at the epoch — the same trap explorerState documents.
  const int = (name: string): number => {
    const raw = params.get(name);
    return raw === null || raw.trim() === "" ? NaN : Number(raw);
  };
  const from = int("from");
  const to = int("to");
  if (Number.isInteger(from) && Number.isInteger(to) && from < to) return { kind: "absolute", from, to };
  const preset = params.get("range") ?? "";
  return isRangePreset(preset) ? { kind: "relative", preset } : DEFAULT_VIEW_STATE.range;
}

/**
 * Reads view state from URL search params. Anything malformed falls back to
 * the default for that field alone, so a stale link degrades rather than
 * fails.
 *
 * Variable names are lower-cased, because the lexer lower-cases a `$name` as
 * it reads one and the API lower-cases `var.<name>` to match — so `var.Env`
 * and `var.env` are one variable at every layer, and a link that shouts still
 * works.
 */
export function parseViewState(params: URLSearchParams): DashboardViewState {
  const vars: Record<string, string> = {};
  for (const [key, value] of params) {
    if (key.startsWith(VAR_PREFIX) && key.length > VAR_PREFIX.length) {
      vars[key.slice(VAR_PREFIX.length).toLowerCase()] = value.trim();
    }
  }
  return { range: parseRange(params), live: params.get("live") !== "0", vars };
}

/**
 * Writes view state as search params, omitting anything at its default so an
 * ordinary link stays short. An explicitly-cleared variable is *not* omitted:
 * that is the case the empty value exists to express.
 */
export function serializeViewState(state: DashboardViewState): URLSearchParams {
  const params = new URLSearchParams();
  if (state.range.kind === "absolute") {
    params.set("from", String(state.range.from));
    params.set("to", String(state.range.to));
  } else if (state.range.preset !== DEFAULT_PRESET) {
    params.set("range", state.range.preset);
  }
  if (!state.live) params.set("live", "0");
  for (const name of Object.keys(state.vars).sort()) params.set(VAR_PREFIX + name, state.vars[name] ?? "");
  return params;
}

/** The keys [[serializeViewState]] owns: the range, `live`, and every `var.*`. */
function isViewStateKey(key: string): boolean {
  return key === "range" || key === "from" || key === "to" || key === "live" || key.startsWith(VAR_PREFIX);
}

/**
 * `params` with its view state replaced by `state`, and every other
 * parameter kept as it was.
 *
 * For a page whose query string holds more than the view — the editor's
 * `?copy=` seed, say — where writing [[serializeViewState]]'s output
 * wholesale would drop the rest. It removes what it owns rather than keeping
 * a list of what it does not, so a parameter a page adds later survives a
 * time-range change without anyone remembering to list it here.
 */
export function withViewState(params: URLSearchParams, state: DashboardViewState): URLSearchParams {
  const next = new URLSearchParams();
  for (const [k, v] of params) if (!isViewStateKey(k)) next.append(k, v);
  for (const [k, v] of serializeViewState(state)) next.append(k, v);
  return next;
}

/**
 * The value a selector should show: the URL's choice if there is one, else
 * the definition's default, else "all".
 *
 * "*" and "" both mean every value in a definition, and the UI has one way of
 * saying that — the empty string — so the two collapse here rather than
 * everywhere downstream.
 */
export function selectedValue(v: TemplateVar, state: DashboardViewState): string {
  const chosen = state.vars[v.name.toLowerCase()];
  if (chosen !== undefined) return chosen;
  const fallback = (v.default ?? "").trim();
  return fallback === "*" ? "" : fallback;
}

/**
 * Binds every declared variable for `/api/v1/query/batch`.
 *
 * Each name maps to zero or more `key:value` tags: zero means "every value",
 * which the evaluator turns into no filter and a warning rather than an
 * error. Every *declared* variable is bound even when it resolves to nothing,
 * because an unbound `$var` is an error — the evaluator refuses it rather
 * than silently widening the query, which is the wrong answer people believe.
 */
export function bindVars(vars: readonly TemplateVar[] | undefined, state: DashboardViewState): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const v of vars ?? []) {
    const value = selectedValue(v, state);
    out[v.name.toLowerCase()] = value === "" ? [] : [`${v.tag}:${value}`];
  }
  return out;
}
