/**
 * Time-range math for every time-scoped view.
 *
 * A range is either *relative* ("the last 1h", which slides forward as time
 * passes) or *absolute* (two fixed instants). Relative ranges are what make a
 * view "live": they are stored as the preset name, never as resolved numbers,
 * and are resolved against the clock only at the moment a request is made. If
 * the URL held resolved from/to instead, auto-refresh would re-fetch the same
 * frozen window forever and a shared link would show stale data.
 *
 * Everything here is pure: the current time is always an argument, so tests
 * pin it instead of mocking the clock.
 */

/** Relative presets offered by the picker, shortest first. */
export const RANGE_PRESETS = ["5m", "15m", "1h", "4h", "1d"] as const;

/** Name of one relative preset, e.g. "1h". */
export type RangePreset = (typeof RANGE_PRESETS)[number];

/** Length of each preset in seconds. */
const PRESET_SECONDS: Record<RangePreset, number> = {
  "5m": 5 * 60,
  "15m": 15 * 60,
  "1h": 3600,
  "4h": 4 * 3600,
  "1d": 86400,
};

/** Human label for each preset, as the picker shows it. */
export const PRESET_LABELS: Record<RangePreset, string> = {
  "5m": "Past 5 minutes",
  "15m": "Past 15 minutes",
  "1h": "Past 1 hour",
  "4h": "Past 4 hours",
  "1d": "Past 1 day",
};

/**
 * A time range as the user chose it: a sliding preset or a fixed window.
 * Absolute bounds are unix seconds, the unit the query API speaks.
 */
export type TimeRange = { kind: "relative"; preset: RangePreset } | { kind: "absolute"; from: number; to: number };

/** A resolved window in unix seconds, `from < to`. */
export interface ResolvedRange {
  from: number;
  to: number;
}

/** Reports whether a string names a known preset. */
export function isRangePreset(s: string): s is RangePreset {
  return (RANGE_PRESETS as readonly string[]).includes(s);
}

/**
 * Turns a range into concrete unix-second bounds. `nowMs` is injected (pass
 * `Date.now()`), and is floored to a whole second so two resolutions within
 * the same second produce the same request.
 */
export function resolveTimeRange(range: TimeRange, nowMs: number): ResolvedRange {
  if (range.kind === "absolute") return { from: range.from, to: range.to };
  const to = Math.floor(nowMs / 1000);
  return { from: to - PRESET_SECONDS[range.preset], to };
}

/** Length of a preset in seconds. */
export function presetSeconds(preset: RangePreset): number {
  return PRESET_SECONDS[preset];
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

/**
 * Formats unix seconds as the value of an `<input type="datetime-local">`
 * ("2026-09-19T14:05") in the browser's local time zone — the zone the user
 * reads and types in. Seconds are dropped; the input has minute precision.
 */
export function toLocalInputValue(unixSeconds: number): string {
  const d = new Date(unixSeconds * 1000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * Parses a `datetime-local` value (local time) back to unix seconds, or
 * returns undefined for anything that isn't one. The inverse of
 * toLocalInputValue for minute-aligned instants.
 */
export function fromLocalInputValue(value: string): number | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(value);
  if (!m) return undefined;
  const [, y, mo, d, h, mi, s] = m.map(Number) as [number, number, number, number, number, number, number];
  const date = new Date(y, mo - 1, d, h, mi, Number.isNaN(s) ? 0 : s);
  // Date rolls "Feb 31" over into March; treat that as invalid, not as a date.
  if (date.getMonth() !== mo - 1 || date.getDate() !== d) return undefined;
  return Math.floor(date.getTime() / 1000);
}
