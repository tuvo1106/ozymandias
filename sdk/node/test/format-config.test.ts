// L1 unit tests for the pure parts: line formatting and config resolution.
import { describe, expect, it } from "vitest";
import {
  DEFAULT_FLUSH_INTERVAL_MS,
  DEFAULT_MAX_PAYLOAD_BYTES,
  DEFAULT_STATSD_PORT,
  parseBool,
  parseTags,
  resolveConfig,
} from "../src/config.js";
import { formatLine, formatNumber, globalTags, sanitizeName, sanitizeTag } from "../src/format.js";

describe("formatNumber", () => {
  it.each([
    [1, "1"],
    [1.0, "1"],
    [-2, "-2"],
    [0.25, "0.25"],
    [0.1 + 0.2, "0.30000000000000004"],
    [-0, "0"],
    // Exponent forms: valid for the agent's strconv.ParseFloat.
    [1e21, "1e+21"],
    [1e-7, "1e-7"],
  ])("%s -> %s", (n, want) => {
    expect(formatNumber(n)).toBe(want);
  });

  it("drops non-finite values", () => {
    expect(formatNumber(NaN)).toBeNull();
    expect(formatNumber(Infinity)).toBeNull();
    expect(formatNumber(-Infinity)).toBeNull();
  });
});

describe("sanitation", () => {
  it("replaces framing characters in names, including ':'", () => {
    expect(sanitizeName("a|b:c,d\ne")).toBe("a_b_c_d_e");
  });
  it("keeps ':' in tags and set members", () => {
    expect(sanitizeTag("k:v|w,x\ny")).toBe("k:v_w_x_y");
  });
});

describe("globalTags / formatLine", () => {
  it("orders init tags, then service, env, version, skipping unset ones", () => {
    expect(globalTags(["team:core"], "svc", undefined, "1.0")).toEqual(["team:core", "service:svc", "version:1.0"]);
    expect(globalTags([], undefined, undefined, undefined)).toEqual([]);
  });
  it("sanitizes global tag values", () => {
    expect(globalTags(["a|b"], "s,x", "e\n", "v|1")).toEqual(["a_b", "service:s_x", "env:e_", "version:v_1"]);
  });
  it("writes rate before tags and omits a rate of 1", () => {
    expect(formatLine("m", "1", "c", 0.5, ["a:b"], ["g:1"])).toBe("m:1|c|@0.5|#a:b,g:1");
    expect(formatLine("m", "1", "c", 1, [], [])).toBe("m:1|c");
  });
  it("coerces non-string tags instead of throwing", () => {
    expect(formatLine("m", "1", "c", 1, [42 as unknown as string], [])).toBe("m:1|c|#42");
  });
});

describe("config", () => {
  it("defaults to disabled with standard settings", () => {
    const c = resolveConfig({}, {});
    expect(c).toEqual({
      enabled: false,
      agentHost: "",
      statsdPort: DEFAULT_STATSD_PORT,
      service: undefined,
      env: undefined,
      version: undefined,
      tags: [],
      debug: false,
      maxPayloadBytes: DEFAULT_MAX_PAYLOAD_BYTES,
      flushIntervalMs: DEFAULT_FLUSH_INTERVAL_MS,
    });
  });

  it("reads every OZY_* variable", () => {
    const c = resolveConfig(
      {},
      {
        OZY_AGENT_HOST: "agent",
        OZY_STATSD_PORT: "9125",
        OZY_SERVICE: "svc",
        OZY_ENV: "prod",
        OZY_VERSION: "2.0",
        OZY_TAGS: " a:b , c ,,",
        OZY_DEBUG: "true",
      },
    );
    expect(c).toMatchObject({
      enabled: true,
      agentHost: "agent",
      statsdPort: 9125,
      service: "svc",
      env: "prod",
      version: "2.0",
      tags: ["a:b", "c"],
      debug: true,
    });
  });

  it("lets arguments override the environment", () => {
    const c = resolveConfig(
      { agentHost: "h", statsdPort: 1, service: "s", env: "e", version: "v", tags: ["x", "", 5 as unknown as string], debug: false },
      { OZY_AGENT_HOST: "env", OZY_STATSD_PORT: "2", OZY_SERVICE: "es", OZY_TAGS: "y", OZY_DEBUG: "1" },
    );
    expect(c).toMatchObject({ agentHost: "h", statsdPort: 1, service: "s", env: "e", version: "v", tags: ["x"], debug: false });
  });

  it("treats empty strings as unset", () => {
    const c = resolveConfig({ agentHost: "", service: "" }, { OZY_AGENT_HOST: "", OZY_SERVICE: "fromenv" });
    expect(c.enabled).toBe(false);
    expect(c.service).toBe("fromenv");
  });

  it("falls back to defaults for invalid numbers instead of throwing", () => {
    const c = resolveConfig(
      { statsdPort: 70000, maxPayloadBytes: -1, flushIntervalMs: 1.5 },
      { OZY_STATSD_PORT: "abc" },
    );
    expect(c.statsdPort).toBe(DEFAULT_STATSD_PORT);
    expect(c.maxPayloadBytes).toBe(DEFAULT_MAX_PAYLOAD_BYTES);
    expect(c.flushIntervalMs).toBe(DEFAULT_FLUSH_INTERVAL_MS);
    expect(resolveConfig({}, { OZY_STATSD_PORT: " " }).statsdPort).toBe(DEFAULT_STATSD_PORT);
    expect(resolveConfig({ maxPayloadBytes: 512, flushIntervalMs: 5 }, {})).toMatchObject({
      maxPayloadBytes: 512,
      flushIntervalMs: 5,
    });
  });

  it("parses booleans and tag lists", () => {
    for (const v of ["1", "true", "TRUE", "yes", " on "]) expect(parseBool(v)).toBe(true);
    for (const v of [undefined, "", "0", "false", "tru"]) expect(parseBool(v)).toBe(false);
    expect(parseTags(undefined)).toEqual([]);
    expect(parseTags("")).toEqual([]);
  });
});

describe("tags given as a bare string", () => {
  // The CJS build ships to untyped callers, and `{ tags: "env:dev" }` is the
  // shape they reach for. It used to throw `.map is not a function` inside
  // the client, where the wrapper swallowed it — the metric vanished and only
  // `errors` moved.
  it("is treated as one tag rather than losing the metric", () => {
    const line = formatLine("app.requests", "1", "c", 1, "env:dev" as unknown as string[], ["service:svc"]);
    expect(line).toBe("app.requests:1|c|#env:dev,service:svc");
  });

  it("survives other non-arrays by ignoring them", () => {
    const line = formatLine("app.requests", "1", "c", 1, 42 as unknown as string[], ["service:svc"]);
    expect(line).toBe("app.requests:1|c|#service:svc");
  });
});
