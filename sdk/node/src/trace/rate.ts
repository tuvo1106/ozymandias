/**
 * Sampling-rate parsing, kept apart from the tracer so configuration can use
 * it without importing the tracer (and its state) back.
 *
 * @module
 */

function clamp01(n: number): number {
  return Math.min(1, Math.max(0, n));
}

/**
 * Parses a sampling rate: a finite number is clamped into [0, 1]; anything
 * else gives the fallback. Clamping, not rejecting, because `OZY_TRACE_SAMPLE_RATE=5`
 * most plausibly means "everything" and `-1` "nothing".
 *
 * @param arg - the `init()` option.
 * @param envValue - the `OZY_TRACE_SAMPLE_RATE` value.
 * @param fallback - used when neither is valid.
 * @returns the rate.
 */
export function parseSampleRate(arg: number | undefined, envValue: string | undefined, fallback = 1): number {
  if (typeof arg === "number" && Number.isFinite(arg)) return clamp01(arg);
  if (envValue !== undefined && envValue.trim() !== "") {
    const n = Number(envValue);
    if (Number.isFinite(n)) return clamp01(n);
  }
  return fallback;
}
