/**
 * The pure parts of the trace wire protocol (docs/wire-protocol.md §B): span
 * shape, limits, head sampling, the path normalizer and propagation parsing.
 *
 * Everything here is a port of `pkg/wire/traces.go` and is checked against the
 * same vectors the Go and Python suites load (`pkg/wire/testdata/traces/`).
 * The reason these live in one module with no I/O is exactly that: a decision
 * (keep this trace? what is this route called?) that two services make
 * differently is a bug no single service can see, so the functions are small,
 * pure and pinned by shared data instead of by shared code.
 *
 * @module
 */

/** One span on the wire (§B). `parent_id` is null for a trace root. */
export interface WireSpan {
  trace_id: string;
  span_id: string;
  parent_id: string | null;
  service: string;
  name: string;
  resource: string;
  type: string;
  /** Unix microseconds. */
  start: number;
  /** Microseconds, never negative. */
  duration: number;
  error: 0 | 1;
  meta: Record<string, string>;
  metrics: Record<string, number>;
}

/** Limits from wire-protocol.md §B; the agent refuses or truncates beyond them. */
export const LIMITS = {
  maxServiceBytes: 100,
  maxNameBytes: 100,
  maxResourceBytes: 5000,
  maxMetaValueBytes: 5000,
  maxMetaKeyBytes: 100,
  maxMetaEntries: 100,
  maxMetricEntries: 50,
  maxChunksPerRequest: 1000,
  maxSpansPerChunk: 5000,
} as const;

/** The span types the agent accepts; anything else becomes `custom`. */
export const SPAN_TYPES: ReadonlySet<string> = new Set(["web", "db", "cache", "queue", "http", "worker", "custom"]);

/** Knuth's multiplicative hash constant, the same one `wire.SamplingMultiplier` uses. */
const SAMPLING_MULTIPLIER = 1111111111111111111n;
const MASK64 = (1n << 64n) - 1n;
const TWO_POW_64 = 2 ** 64;
const LOWER_HEX = /^[0-9a-f]+$/;

/**
 * The head-sampling decision for a trace at `rate`: keep when
 * `(low64(trace_id) * 1111111111111111111 mod 2^64) < rate * 2^64`.
 *
 * It is a pure function of `(traceId, rate)`, so every service in a trace,
 * whatever its language, reaches the same verdict without talking to the
 * others. BigInt is what makes the 64-bit wrap-around exact; doing this in
 * doubles would silently lose the low bits and disagree with Go and Python
 * on some ids. `rate * 2^64` is exact in a double (a power-of-two scaling),
 * and is floored to match Go's truncating `uint64()` conversion.
 *
 * @param traceId - 32 lowercase hex characters; anything else never keeps.
 * @param rate - in [0, 1]; `<= 0` (or NaN) never keeps, `>= 1` always does.
 * @returns whether the trace is kept.
 */
export function sampleKeep(traceId: string, rate: number): boolean {
  if (!(rate > 0)) return false;
  if (rate >= 1) return true;
  if (traceId.length !== 32) return false;
  const lowHex = traceId.slice(16);
  if (!LOWER_HEX.test(lowHex)) return false;
  const low = BigInt("0x" + lowHex);
  const threshold = BigInt(Math.floor(rate * TWO_POW_64));
  return ((low * SAMPLING_MULTIPLIER) & MASK64) < threshold;
}

function isDigit(c: number): boolean {
  return c >= 48 && c <= 57;
}
function isHexLetter(c: number): boolean {
  return (c >= 97 && c <= 102) || (c >= 65 && c <= 70);
}
function isAlpha(c: number): boolean {
  return (c >= 97 && c <= 122) || (c >= 65 && c <= 90);
}

function isUUID(s: string): boolean {
  if (s.length !== 36) return false;
  for (let i = 0; i < 36; i++) {
    const c = s.charCodeAt(i);
    if (i === 8 || i === 13 || i === 18 || i === 23) {
      if (c !== 45) return false;
    } else if (!isDigit(c) && !isHexLetter(c)) {
      return false;
    }
  }
  return true;
}

function isIDSegment(s: string): boolean {
  if (s === "") return false;
  let digits = true;
  let hex = true;
  let nano = true;
  let hasDigit = false;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    const d = isDigit(c);
    if (d) hasDigit = true;
    else digits = false;
    if (!d && !isHexLetter(c)) hex = false;
    if (!d && !isAlpha(c) && c !== 95 && c !== 45) nano = false;
  }
  return digits || isUUID(s) || (hex && s.length >= 12) || (nano && s.length >= 16 && hasDigit);
}

/**
 * Turns a URL path into a low-cardinality route resource (§B "Path
 * normalizer"): all-digit, UUID, 12+ hex and nanoid-like segments become
 * `:id`, the query and fragment are dropped, and at most 8 segments survive.
 * Identical to `wire.NormalizePath`: a route has one spelling in every service.
 *
 * @param path - a raw path, possibly with `?query` or `#fragment`.
 * @returns the normalized path, `"/"` for an empty one.
 */
export function normalizePath(path: string): string {
  const cut = path.search(/[?#]/);
  if (cut >= 0) path = path.slice(0, cut);
  if (path === "" || path === "/") return "/";
  let segs = (path.startsWith("/") ? path.slice(1) : path).split("/");
  if (segs.length > 8) segs = segs.slice(0, 8);
  return "/" + segs.map((s) => (isIDSegment(s) ? ":id" : s)).join("/");
}

/** Propagation header carrying the trace id (§B). */
export const HEADER_TRACE_ID = "x-ozy-trace-id";
/** Propagation header carrying the caller's span id. */
export const HEADER_PARENT_ID = "x-ozy-parent-id";
/** Propagation header carrying the sampling priority. */
export const HEADER_PRIORITY = "x-ozy-sampling-priority";

/** What the headers of an inbound call say: the caller's span and its priority. */
export interface ParsedPropagation {
  traceId: string;
  parentId: string;
  priority: number;
}

function validId(id: string, len: number): boolean {
  return id.length === len && LOWER_HEX.test(id) && !/^0+$/.test(id);
}

/**
 * Reads the three propagation header values (§B). Anything malformed (wrong
 * length, a zero id, a priority outside -1..2) yields `null`: the receiver
 * starts a fresh trace rather than continuing a corrupt one. Never throws.
 *
 * @param traceId - raw `x-ozy-trace-id` value.
 * @param parentId - raw `x-ozy-parent-id` value.
 * @param priority - raw `x-ozy-sampling-priority` value, absent meaning keep.
 * @returns the parsed context, or null.
 */
export function parsePropagation(traceId: unknown, parentId: unknown, priority: unknown): ParsedPropagation | null {
  if (typeof traceId !== "string" || typeof parentId !== "string") return null;
  const tid = traceId.trim().toLowerCase();
  const pid = parentId.trim().toLowerCase();
  if (!validId(tid, 32) || !validId(pid, 16)) return null;
  let prio = 1;
  if (priority !== undefined && priority !== null) {
    const p = String(priority).trim();
    if (p !== "") {
      if (p !== "-1" && p !== "0" && p !== "1" && p !== "2") return null;
      prio = Number(p);
    }
  }
  return { traceId: tid, parentId: pid, priority: prio };
}

/**
 * Cuts `s` to at most `n` UTF-8 bytes without splitting a code point, as
 * `truncRunes` does in Go. The fast path (`s.length * 3 <= n` cannot exceed
 * the limit) avoids encoding most strings at all.
 *
 * @param s - the string.
 * @param n - byte limit.
 * @returns `s` or its longest prefix within `n` bytes.
 */
export function truncBytes(s: string, n: number): string {
  if (s.length * 3 <= n) return s;
  const buf = Buffer.from(s, "utf8");
  if (buf.length <= n) return s;
  let cut = n;
  while (cut > 0 && (buf[cut]! & 0xc0) === 0x80) cut--;
  return buf.subarray(0, cut).toString("utf8");
}

function limitRecord<V>(src: Record<string, V>, maxEntries: number, fix: (v: V) => V | undefined): Record<string, V> {
  const keys = Object.keys(src).filter((k) => k !== "" && Buffer.byteLength(k) <= LIMITS.maxMetaKeyBytes);
  keys.sort();
  const out: Record<string, V> = {};
  let n = 0;
  for (const k of keys) {
    if (n >= maxEntries) break;
    const v = fix(src[k] as V);
    if (v === undefined) continue;
    out[k] = v;
    n++;
  }
  return out;
}

/**
 * Applies, in the SDK, every rule `wire.NormalizeSpan` / `ValidateSpan` would
 * apply in the agent, so the SDK never sends a span the agent would refuse
 * whole. Mutates and returns `sp`. Refusal rules that cannot arise from the
 * tracer's own construction (ids, start) are not re-checked; the ones driven
 * by user input (service, name, error, non-finite metrics, sizes) are.
 *
 * Over-long `service` and `name` are truncated rather than refused: losing the
 * tail of a name beats losing the span. Surplus entries are dropped in sorted
 * key order, the agent's order, so the same span always trims the same way.
 *
 * @param sp - the span to normalize.
 * @returns the same span.
 */
export function normalizeWireSpan(sp: WireSpan): WireSpan {
  if (!SPAN_TYPES.has(sp.type)) sp.type = "custom";
  sp.service = truncBytes(sp.service, LIMITS.maxServiceBytes);
  sp.name = truncBytes(sp.name, LIMITS.maxNameBytes);
  if (sp.service === "") sp.service = "unknown";
  if (sp.name === "") sp.name = "unnamed";
  sp.resource = truncBytes(sp.resource, LIMITS.maxResourceBytes);
  if (sp.error !== 1) sp.error = 0;
  if (!(sp.duration >= 0)) sp.duration = 0;
  if (Object.keys(sp.meta).length > 0) {
    sp.meta = limitRecord(sp.meta, LIMITS.maxMetaEntries, (v) => truncBytes(String(v), LIMITS.maxMetaValueBytes));
  }
  if (Object.keys(sp.metrics).length > 0) {
    sp.metrics = limitRecord(sp.metrics, LIMITS.maxMetricEntries, (v) => (Number.isFinite(v) ? v : undefined));
  }
  return sp;
}
