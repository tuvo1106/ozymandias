/**
 * Configuration: `init()` arguments layered over `OZY_*` environment
 * variables, over defaults.
 *
 * The precedence rule is "an argument that is set wins, otherwise the
 * environment, otherwise the default" — the rule both SDKs follow (M1 spec), so a
 * service can be configured entirely from its deployment (env) while tests
 * and scripts can pin values in code. An empty string counts as *unset* in
 * both layers: container orchestrators commonly render `FOO=` for a variable
 * that has no value, and treating that as "the host is the empty string"
 * would turn a disabled SDK into a broken one.
 *
 * The single most important decision made here is {@link ResolvedConfig.enabled}:
 * without an agent host the SDK is inert — no socket, timer or exit hook is
 * ever created. That is what lets a library or app ship with the SDK wired in
 * and still behave identically where no agent runs.
 *
 * @module
 */
import type { ClientHooks } from "./client.js";

/** Default extended StatsD port (wire-protocol hop A). */
export const DEFAULT_STATSD_PORT = 8125;
/**
 * Default maximum datagram payload: one Ethernet MTU (1500) minus IPv4 (20)
 * and UDP (8) headers, minus 40 bytes of slack for IP options/tunnels — the
 * extended StatsD convention. Bigger datagrams would be fragmented, and losing any
 * fragment loses the whole datagram.
 */
export const DEFAULT_MAX_PAYLOAD_BYTES = 1432;
/** Default upper bound on how long a message sits in the buffer. */
export const DEFAULT_FLUSH_INTERVAL_MS = 100;

/**
 * Options accepted by `init()`. Every field is optional; unset fields fall
 * back to the matching `OZY_*` environment variable, then the default.
 */
export interface InitOptions {
  /** Service name, sent as the `service:` tag. Env: `OZY_SERVICE`. */
  service?: string;
  /** Environment (`dev`, `prod`…), sent as `env:`. Env: `OZY_ENV`. */
  env?: string;
  /** Service version, sent as `version:`. Env: `OZY_VERSION`. */
  version?: string;
  /**
   * Tags added to every message, after the call's own tags. Replaces (does
   * not merge with) `OZY_TAGS`, a comma-separated list.
   */
  tags?: string[];
  /**
   * Hostname or IP of the ozymandias agent. Env: `OZY_AGENT_HOST`.
   * **Unset means the SDK is a no-op.**
   */
  agentHost?: string;
  /** Agent extended StatsD UDP port. Env: `OZY_STATSD_PORT`. Default 8125. */
  statsdPort?: number;
  /** Log SDK-internal events to stderr. Env: `OZY_DEBUG` (`1`/`true`). */
  debug?: boolean;
  /**
   * Maximum bytes per datagram; messages are coalesced up to this size.
   * Default 1432. The agent reads datagrams of up to 8192 bytes, so values up
   * to that are safe on loopback, where there is no MTU to respect.
   */
  maxPayloadBytes?: number;
  /** Maximum time a message waits in the buffer, in ms. Default 100. */
  flushIntervalMs?: number;
  /**
   * Test and embedding seams (random source, clock, socket factory, resolver,
   * timers, log sink). Applications leave this unset.
   */
  hooks?: ClientHooks;
}

/** The fully resolved configuration a client is built from. */
export interface ResolvedConfig {
  /** False when no agent host is configured: the SDK then does nothing. */
  enabled: boolean;
  agentHost: string;
  statsdPort: number;
  service: string | undefined;
  env: string | undefined;
  version: string | undefined;
  tags: string[];
  debug: boolean;
  maxPayloadBytes: number;
  flushIntervalMs: number;
}

/** The environment shape read by {@link resolveConfig}; `process.env` fits. */
export type Env = Readonly<Record<string, string | undefined>>;

function str(arg: string | undefined, envValue: string | undefined): string | undefined {
  if (typeof arg === "string" && arg !== "") return arg;
  if (typeof envValue === "string" && envValue !== "") return envValue;
  return undefined;
}

function positiveInt(arg: number | undefined, envValue: string | undefined, fallback: number, max: number): number {
  const valid = (n: number) => Number.isInteger(n) && n > 0 && n <= max;
  if (typeof arg === "number" && valid(arg)) return arg;
  if (envValue !== undefined && envValue.trim() !== "") {
    const n = Number(envValue);
    if (valid(n)) return n;
  }
  return fallback;
}

/**
 * Parses a boolean-ish environment value. Only explicit truthy spellings
 * enable a flag, so a typo leaves the default (off) rather than turning it on.
 *
 * @param value - the raw variable value.
 * @returns true for `1`, `true`, `yes`, `on` (any case).
 */
export function parseBool(value: string | undefined): boolean {
  return value !== undefined && /^(1|true|yes|on)$/i.test(value.trim());
}

/**
 * Parses `OZY_TAGS`: comma-separated, whitespace-trimmed, empty entries
 * dropped (so a trailing comma is harmless).
 *
 * @param value - the raw variable value.
 * @returns the tag list, possibly empty.
 */
export function parseTags(value: string | undefined): string[] {
  if (!value) return [];
  return value
    .split(",")
    .map((t) => t.trim())
    .filter((t) => t !== "");
}

/**
 * Layers `init()` options over the environment over defaults.
 *
 * Invalid numeric settings (a port of `abc`, a negative payload size) fall
 * back to the default instead of throwing: configuration mistakes must not be
 * able to crash the host app at startup. Debug mode reports what was chosen.
 *
 * @param options - the `init()` arguments.
 * @param env - the environment to read, normally `process.env`.
 * @returns the resolved configuration.
 */
export function resolveConfig(options: InitOptions, env: Env): ResolvedConfig {
  // An explicit `agentHost: ""` means "off", and must not fall through to the
  // environment: passing it is how an app opts out of metrics on a host where
  // OZY_AGENT_HOST is set for everything else. (The Python SDK does the
  // same; `str()` treats an empty *env* value as unset, which is the opposite
  // case — an exported-but-blank variable was never a deliberate choice.)
  const agentHost =
    typeof options.agentHost === "string" ? options.agentHost : (str(undefined, env.OZY_AGENT_HOST) ?? "");
  return {
    enabled: agentHost !== "",
    agentHost,
    statsdPort: positiveInt(options.statsdPort, env.OZY_STATSD_PORT, DEFAULT_STATSD_PORT, 65535),
    service: str(options.service, env.OZY_SERVICE),
    env: str(options.env, env.OZY_ENV),
    version: str(options.version, env.OZY_VERSION),
    tags: Array.isArray(options.tags) ? options.tags.filter((t) => typeof t === "string" && t !== "") : parseTags(env.OZY_TAGS),
    debug: typeof options.debug === "boolean" ? options.debug : parseBool(env.OZY_DEBUG),
    maxPayloadBytes: positiveInt(options.maxPayloadBytes, undefined, DEFAULT_MAX_PAYLOAD_BYTES, 65507),
    flushIntervalMs: positiveInt(options.flushIntervalMs, undefined, DEFAULT_FLUSH_INTERVAL_MS, 60_000),
  };
}
