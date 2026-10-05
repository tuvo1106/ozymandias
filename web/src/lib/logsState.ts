/**
 * The Log Explorer's state and its URL encoding. As in the metrics explorer
 * (explorerState.ts) the URL is the single source of truth for what is shown,
 * so any view is a shareable link: the query that was run, the time range,
 * whether it is tailing, and which attribute columns are on. Defaults are
 * omitted, and a hand-edited value degrades to its default instead of failing.
 */
import { isRangePreset, type RangePreset, type TimeRange } from "./timeRange";

/** Everything that determines the Log Explorer's view. */
export interface LogsState {
  q: string;
  range: TimeRange;
  /** Live tail on: new logs stream in above the list. */
  tail: boolean;
  /** Extra columns, as attribute paths, in order. */
  cols: string[];
}

const DEFAULT_PRESET: RangePreset = "15m";

/** A fresh explorer: no query, the last 15 minutes, not tailing. */
export const DEFAULT_LOGS_STATE: LogsState = { q: "", range: { kind: "relative", preset: DEFAULT_PRESET }, tail: false, cols: [] };

/** Reads state from URL search params. */
export function parseLogsState(params: URLSearchParams): LogsState {
  const int = (name: string): number => {
    const raw = params.get(name);
    return raw === null || raw.trim() === "" ? NaN : Number(raw);
  };
  const from = int("from");
  const to = int("to");
  const preset = params.get("range") ?? "";
  const range: TimeRange =
    Number.isInteger(from) && Number.isInteger(to) && from < to
      ? { kind: "absolute", from, to }
      : isRangePreset(preset)
        ? { kind: "relative", preset }
        : DEFAULT_LOGS_STATE.range;
  const cols = (params.get("cols") ?? "")
    .split(",")
    .map((c) => c.trim())
    .filter((c, i, all) => c !== "" && all.indexOf(c) === i);
  return { q: (params.get("q") ?? "").trim(), range, tail: params.get("tail") === "1", cols };
}

/** Writes state as search params; the inverse of [[parseLogsState]]. */
export function serializeLogsState(s: LogsState): URLSearchParams {
  const p = new URLSearchParams();
  if (s.q) p.set("q", s.q);
  if (s.range.kind === "absolute") {
    p.set("from", String(s.range.from));
    p.set("to", String(s.range.to));
  } else if (s.range.preset !== DEFAULT_PRESET) {
    p.set("range", s.range.preset);
  }
  if (s.tail) p.set("tail", "1");
  if (s.cols.length) p.set("cols", s.cols.join(","));
  return p;
}
