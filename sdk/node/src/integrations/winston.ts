/**
 * winston log correlation: a format that stamps `trace_id` / `span_id` of the
 * active span on every log entry, so a log line links to its trace and back.
 *
 * winston is deliberately not a dependency. A winston format is just an object
 * with `transform(info, opts)`, so the format here is built structurally; apps
 * that want a `logform`-made one pass `winston.format` in.
 *
 * @module
 */
import { tracer } from "../trace/tracer.js";

/** An entry flowing through a winston format pipeline. */
export type LogInfo = Record<string | symbol, unknown>;

/** The structural shape of a winston format. */
export interface LogFormat {
  transform(info: LogInfo, opts?: unknown): LogInfo | false;
}

/** winston's `format` factory: `format((info, opts) => info)(options?)` returns a {@link LogFormat}. */
export type FormatFactory = (fn: (info: LogInfo, opts?: unknown) => LogInfo | false) => (opts?: unknown) => LogFormat;

function addTrace(info: LogInfo): LogInfo {
  try {
    const span = tracer.scope().active();
    if (span) {
      info["trace_id"] = span.traceId;
      info["span_id"] = span.spanId;
    }
  } catch {
    // Logging must never fail because of tracing.
  }
  return info;
}

/**
 * Returns a winston format adding `trace_id` and `span_id` when a span is
 * active (and leaving the entry untouched when none is).
 *
 * @param formatFactory - optional `winston.format`; when given, the result is built with it.
 * @returns a format usable in `winston.format.combine(...)`.
 */
export function traceFormat(formatFactory?: FormatFactory): LogFormat {
  if (formatFactory) return formatFactory((info) => addTrace(info))();
  return { transform: (info: LogInfo) => addTrace(info) };
}

/** The `winston` integration has nothing to patch: the format is added explicitly. */
export const winstonIntegration = {
  name: "winston",
  isAvailable: () => true,
  patch() {},
  unpatch() {},
};
