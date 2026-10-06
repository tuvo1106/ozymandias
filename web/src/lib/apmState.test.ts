import { apmWindow, DEFAULT_APM_STATE, parseApmState, serializeApmState, toTraceFilter, traceLink, traceSearchLink, type ApmState } from "./apmState";

const parse = (s: string) => parseApmState(new URLSearchParams(s));

describe("apm state", () => {
  it("defaults, and serializes the default to nothing", () => {
    expect(parse("")).toEqual(DEFAULT_APM_STATE);
    expect(serializeApmState(DEFAULT_APM_STATE).toString()).toBe("");
  });
  it("round-trips every field", () => {
    const states: ApmState[] = [
      { env: "dev", service: "shop", resource: "get /items/:id", error: true, minMs: 5, maxMs: 250.5, range: { kind: "relative", preset: "4h" } },
      { ...DEFAULT_APM_STATE, range: { kind: "absolute", from: 10, to: 20 } },
      { ...DEFAULT_APM_STATE, range: { kind: "relative", preset: "5m" } },
    ];
    for (const s of states) expect(parse(serializeApmState(s).toString())).toEqual(s);
  });
  it("degrades hand-edited values to defaults", () => {
    expect(parse("range=9y").range).toEqual(DEFAULT_APM_STATE.range);
    expect(parse("from=20&to=10").range).toEqual(DEFAULT_APM_STATE.range);
    expect(parse("error=true").error).toBe(false);
    expect(parse("min=-3&max=abc")).toMatchObject({ minMs: undefined, maxMs: undefined });
    expect(parse("min=0").minMs).toBeUndefined();
    expect(parse("env=%20dev%20").env).toBe("dev");
  });
  it("drops an inverted duration range's max rather than matching nothing", () => {
    expect(parse("min=50&max=10")).toMatchObject({ minMs: 50, maxMs: undefined });
  });
  it("resolves the window in ms, covering the whole last second", () => {
    expect(apmWindow({ kind: "relative", preset: "5m" }, 1_000_500)).toEqual({ from: 700_000, to: 1_000_999 });
    expect(apmWindow({ kind: "absolute", from: 10, to: 20 }, 0)).toEqual({ from: 10_000, to: 20_999 });
  });
  it("maps state to a search filter, leaving unset fields out", () => {
    expect(toTraceFilter(DEFAULT_APM_STATE)).toEqual({ env: undefined, service: undefined, resource: undefined, error: undefined, minDurationMs: undefined, maxDurationMs: undefined });
    expect(toTraceFilter({ ...DEFAULT_APM_STATE, service: "s", error: true, minMs: 4 })).toMatchObject({ service: "s", error: true, minDurationMs: 4 });
  });
  it("links into trace search keeping env and window but not the other filters", () => {
    const from: ApmState = { ...DEFAULT_APM_STATE, env: "dev", service: "old", error: true, minMs: 3, range: { kind: "relative", preset: "4h" } };
    const url = traceSearchLink(from, { service: "shop", resource: "get /x" });
    expect(url.startsWith("/apm/traces?")).toBe(true);
    expect(parse(url.split("?")[1]!)).toMatchObject({ env: "dev", service: "shop", resource: "get /x", error: false, minMs: undefined, range: { preset: "4h" } });
    expect(traceSearchLink(DEFAULT_APM_STATE, {})).toBe("/apm/traces");
  });
  it("encodes a trace id in its link", () => {
    expect(traceLink("a/b")).toBe("/apm/traces/a%2Fb");
  });
});
