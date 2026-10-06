/**
 * The APM pages' state and its URL encoding. As in the Log Explorer
 * (logsState.ts) the URL is the single source of truth, so a trace search, a
 * service page or the map is a shareable link, defaults are omitted, and a
 * hand-edited value degrades to its default instead of failing.
 *
 * One state serves every APM page: the service list and map read `env` and
 * the range; trace search reads all of it. A service page takes its service
 * from the path, and links to trace search by writing `service` and `resource`
 * here. The default window is an hour, not the logs' 15 minutes: a service
 * sees far fewer traces than logs, and a sparkline over 15 minutes of a quiet
 * dev service is mostly zeros.
 */
import type { ApmWindow, TraceFilter } from "./apmApi";
import { isRangePreset, resolveTimeRange, type RangePreset, type TimeRange } from "./timeRange";

/** Everything that determines an APM view. */
export interface ApmState {
  env: string;
  service: string;
  resource: string;
  /** Only traces with a failed span. */
  error: boolean;
  /** Entry-span duration bounds in ms; undefined is unbounded. */
  minMs: number | undefined;
  maxMs: number | undefined;
  range: TimeRange;
}

const DEFAULT_PRESET: RangePreset = "1h";

/** No filters, the last hour. */
export const DEFAULT_APM_STATE: ApmState = {
  env: "",
  service: "",
  resource: "",
  error: false,
  minMs: undefined,
  maxMs: undefined,
  range: { kind: "relative", preset: DEFAULT_PRESET },
};

function nonNegative(raw: string | null): number | undefined {
  if (raw === null || raw.trim() === "") return undefined;
  const n = Number(raw);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
}

/** Reads state from URL search params. */
export function parseApmState(params: URLSearchParams): ApmState {
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
        : DEFAULT_APM_STATE.range;
  let minMs = nonNegative(params.get("min"));
  let maxMs = nonNegative(params.get("max"));
  // An inverted range matches nothing, which reads as "no traces": drop the max instead.
  if (minMs !== undefined && maxMs !== undefined && maxMs < minMs) maxMs = undefined;
  if (minMs === 0) minMs = undefined;
  return {
    env: (params.get("env") ?? "").trim(),
    service: (params.get("service") ?? "").trim(),
    resource: (params.get("resource") ?? "").trim(),
    error: params.get("error") === "1",
    minMs,
    maxMs,
    range,
  };
}

/** Writes state as search params; the inverse of [[parseApmState]]. */
export function serializeApmState(s: ApmState): URLSearchParams {
  const p = new URLSearchParams();
  if (s.env) p.set("env", s.env);
  if (s.service) p.set("service", s.service);
  if (s.resource) p.set("resource", s.resource);
  if (s.error) p.set("error", "1");
  if (s.minMs !== undefined) p.set("min", String(s.minMs));
  if (s.maxMs !== undefined) p.set("max", String(s.maxMs));
  if (s.range.kind === "absolute") {
    p.set("from", String(s.range.from));
    p.set("to", String(s.range.to));
  } else if (s.range.preset !== DEFAULT_PRESET) {
    p.set("range", s.range.preset);
  }
  return p;
}

/** The request window in unix ms, resolved against the clock now: `to` covers its whole last second. */
export function apmWindow(range: TimeRange, nowMs: number): ApmWindow {
  const r = resolveTimeRange(range, nowMs);
  return { from: r.from * 1000, to: r.to * 1000 + 999 };
}

/** The trace-search filter a state asks for. */
export function toTraceFilter(s: ApmState): TraceFilter {
  return {
    env: s.env || undefined,
    service: s.service || undefined,
    resource: s.resource || undefined,
    error: s.error || undefined,
    minDurationMs: s.minMs,
    maxDurationMs: s.maxMs,
  };
}

/** A link into trace search for a scope, keeping the env and window of the page it comes from. */
export function traceSearchLink(from: ApmState, scope: Partial<Pick<ApmState, "service" | "resource" | "error">>): string {
  const q = serializeApmState({ ...from, service: "", resource: "", error: false, minMs: undefined, maxMs: undefined, ...scope });
  const s = q.toString();
  return `/apm/traces${s ? `?${s}` : ""}`;
}

/** Link to a trace's view. */
export function traceLink(traceId: string): string {
  return `/apm/traces/${encodeURIComponent(traceId)}`;
}
