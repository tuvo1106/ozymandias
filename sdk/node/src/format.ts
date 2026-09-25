/**
 * extended StatsD line formatting — the only part of the SDK that decides bytes.
 *
 * The normative spec is docs/wire-protocol.md §A ("What a client sends") and
 * the byte-exact goldens in pkg/wire/testdata/statsd/sdk-cases.json, which
 * the Go parser tests, this SDK and the Python SDK all load. Keeping every
 * byte decision in these few pure functions is what makes that contract
 * testable: no socket, clock or randomness is involved here.
 *
 * Why the client does so little normalization: the agent is the normalizer of
 * record (lowercasing, length limits, character sets). A client only has to
 * make sure one bad value cannot corrupt the *framing* of a datagram — `|`
 * separates sections, `,` separates tags, `\n` separates messages, and `:`
 * ends a name. Doing more here would mean three implementations (Go, Python,
 * Node) of the same rules that could drift apart.
 *
 * @module
 */

/** The extended StatsD metric types this SDK emits (wire-protocol §A table). */
export type MetricType = "c" | "g" | "h" | "d" | "ms" | "s";

/**
 * Formats a numeric value in the canonical number form of wire-protocol §A:
 * the shortest decimal that round-trips a float64, written positionally when
 * the decimal exponent is in `[-4, 16)` and in exponent form outside it, with
 * a signed exponent of at least two digits. Integral values carry no decimal
 * point (`1`, not `1.0`) and `-0` prints as `0`.
 *
 * `String(value)` is *not* that form, which is what this used to be. Its
 * thresholds are different — plain digits up to `1e21` and down to `1e-7`,
 * and an unpadded exponent — so `1e16` came out as `10000000000000000` here
 * and `1e+16` from the Python SDK, and `1e-7` here against `1e-07` there.
 * Every one of those parses back to the same float64, so nothing was ever
 * corrupted; what was untrue is the promise the two SDKs make each other,
 * that the same call produces the same bytes. The shared goldens compare
 * bytes, so the promise has to hold to be testable at all.
 *
 * `toExponential()` with no argument is the shortest round-tripping digits
 * (ECMA-262: "as many digits as necessary to uniquely specify the Number"),
 * which is where the digits come from; only the placement is decided here.
 *
 * @param value - the number to format.
 * @returns the decimal string, or `null` for NaN and ±Infinity, which the
 *   agent would reject as a parse error — they are dropped client-side.
 */
export function formatNumber(value: number): string | null {
  if (!Number.isFinite(value)) return null;
  if (value === 0) return "0"; // normalizes -0, which String would print as "0" anyway
  const [mantissa, exponent] = value.toExponential().split("e");
  const exp = Number(exponent);
  if (exp >= -4 && exp < 16) {
    // String is always positional over this range (it only reaches for an
    // exponent below 1e-6 or at 1e21), so it is the digits and the point.
    return String(value);
  }
  const abs = Math.abs(exp);
  return `${mantissa}e${exp < 0 ? "-" : "+"}${abs < 10 ? `0${abs}` : abs}`;
}

const NAME_UNSAFE = /[|:,\n]/g;
const TAG_UNSAFE = /[|,\n]/g;

/**
 * Makes a metric name safe for the datagram: `|`, `:`, `,` and newline become
 * `_`. `:` is included only for names because it is what ends the name
 * section (`name:value|type`); inside a tag it separates key from value and
 * must survive.
 *
 * @param name - the metric name as the caller gave it.
 * @returns the name with framing characters replaced.
 */
export function sanitizeName(name: string): string {
  return name.replace(NAME_UNSAFE, "_");
}

/**
 * Makes a tag or a set member safe: `|`, `,` and newline become `_`; `:` is
 * kept because it separates a tag's key from its value.
 *
 * @param tag - a `key:value` tag, a bare key, or a set member.
 * @returns the string with framing characters replaced.
 */
export function sanitizeTag(tag: string): string {
  return tag.replace(TAG_UNSAFE, "_");
}

/**
 * Builds the global tag list appended to every message: the configured tags,
 * then `service:`, `env:` and `version:` for whichever of those are set, in
 * that order (wire-protocol §A). It is computed once at `init()` so the hot
 * path only concatenates.
 *
 * @param tags - the `init()` / `OZY_TAGS` tags.
 * @param service - the service name, if any.
 * @param env - the environment, if any.
 * @param version - the version, if any.
 * @returns sanitized tags in wire order.
 */
export function globalTags(
  tags: readonly string[],
  service: string | undefined,
  env: string | undefined,
  version: string | undefined,
): string[] {
  const out = tags.map(sanitizeTag);
  if (service) out.push(`service:${sanitizeTag(service)}`);
  if (env) out.push(`env:${sanitizeTag(env)}`);
  if (version) out.push(`version:${sanitizeTag(version)}`);
  return out;
}

/**
 * Assembles one extended StatsD line in the canonical section order:
 * `name:value|type`, then `|@rate` only when the rate is below 1, then
 * `|#tags` (call tags first, then global tags). The agent accepts any section
 * order; a fixed order is what lets the goldens compare bytes.
 *
 * The sampling *decision* is not made here — the caller has already decided
 * to send. This function only records the rate so the agent can scale counts
 * back up by `1/rate`.
 *
 * @param name - the metric name (sanitized here).
 * @param value - an already-formatted value (a number via {@link formatNumber}
 *   or a sanitized set member).
 * @param type - the metric type.
 * @param sampleRate - the rate the message was sampled at; omitted when ≥ 1.
 * @param callTags - the call's tags (sanitized here).
 * @param global - pre-sanitized global tags from {@link globalTags}.
 * @returns the line, without a trailing newline.
 */
export function formatLine(
  name: string,
  value: string,
  type: MetricType,
  sampleRate: number,
  callTags: readonly string[] | undefined,
  global: readonly string[],
): string {
  let line = `${sanitizeName(name)}:${value}|${type}`;
  if (sampleRate < 1) line += `|@${String(sampleRate)}`;
  // Array.isArray, not a truthiness check: the CJS build ships to untyped
  // callers, and `{ tags: "env:dev" }` is the mistake they make. Treating a
  // bare string as one tag keeps the metric rather than throwing a
  // `.map is not a function` that `withClient` would swallow, losing the call.
  const call = typeof callTags === "string" ? [callTags] : Array.isArray(callTags) ? callTags : [];
  const tags = call.length > 0 ? call.map((t) => sanitizeTag(String(t))).concat(global) : global;
  if (tags.length > 0) line += `|#${tags.join(",")}`;
  return line;
}
