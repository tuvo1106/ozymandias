import { DEFAULT_EXPLORER_STATE, type ExplorerState } from "./explorerState";
import {
  ApiError,
  buildQueryParams,
  fetchMetricNames,
  fetchQuery,
  fetchTagKeys,
  fetchTagValues,
  getJSON,
  isQueryResult,
} from "./metricsApi";

type Handler = (url: string) => Promise<Response>;

function fakeFetch(handler: Handler) {
  const calls: string[] = [];
  const f = ((url: string) => {
    calls.push(url);
    return handler(url);
  }) as unknown as typeof fetch;
  return { f, calls };
}

const json = (body: unknown, status = 200) => async () => new Response(JSON.stringify(body), { status });

const result = {
  status: "ok",
  from: 1_790_000_000,
  to: 1_790_003_600,
  interval: 20,
  series: [
    {
      metric: "http.request.count",
      tags: { route: "/api/comics" },
      points: [
        [1_790_000_000_000, 4],
        [1_790_000_020_000, null],
      ],
    },
  ],
};

const state: ExplorerState = {
  ...DEFAULT_EXPLORER_STATE,
  metric: "http.request.count",
  filters: [
    { key: "env", value: "dev", negate: false },
    { key: "route", value: "/api/*", negate: true },
  ],
  by: ["route", "env"],
  agg: "sum",
};

describe("getJSON", () => {
  it("returns the parsed body", async () => {
    await expect(getJSON("/x", fakeFetch(json({ a: 1 })).f)).resolves.toEqual({ a: 1 });
  });

  it("shows the server's own error message", async () => {
    const { f } = fakeFetch(json({ status: "error", error: "unknown aggregator \"median\"" }, 400));
    const err = await getJSON("/x", f).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ message: 'unknown aggregator "median"', status: 400 });
  });

  it("falls back to the status line when the error body isn't the API's", async () => {
    const { f } = fakeFetch(async () => new Response("<html>", { status: 502, statusText: "Bad Gateway" }));
    await expect(getJSON("/x", f)).rejects.toMatchObject({ message: "ozyd answered 502 Bad Gateway", status: 502 });
    const { f: f2 } = fakeFetch(json({ status: "error", error: "" }, 500));
    await expect(getJSON("/x", f2)).rejects.toMatchObject({ message: "ozyd answered 500", status: 500 });
  });

  it("explains an unreachable server, without a status", async () => {
    const { f } = fakeFetch(() => Promise.reject(new TypeError("Failed to fetch")));
    await expect(getJSON("/x", f)).rejects.toMatchObject({
      message: "ozyd is unreachable: Failed to fetch",
      status: undefined,
    });
    const { f: f2 } = fakeFetch(() => Promise.reject("boom"));
    await expect(getJSON("/x", f2)).rejects.toThrow("unreachable: boom");
  });

  it("passes aborts through untouched", async () => {
    const { f } = fakeFetch(() => Promise.reject(new DOMException("aborted", "AbortError")));
    await expect(getJSON("/x", f)).rejects.toMatchObject({ name: "AbortError" });
  });
});

describe("autocomplete endpoints", () => {
  it("lists metric names by prefix", async () => {
    const { f, calls } = fakeFetch(json({ metrics: ["http.request.count"] }));
    await expect(fetchMetricNames("http.", 50, f)).resolves.toEqual(["http.request.count"]);
    expect(calls).toEqual(["/api/v1/metrics?prefix=http.&limit=50"]);
  });

  it("lists tag keys of a metric", async () => {
    const { f, calls } = fakeFetch(json({ keys: ["env", "route"] }));
    await expect(fetchTagKeys("a b", f)).resolves.toEqual(["env", "route"]);
    expect(calls).toEqual(["/api/v1/tags?metric=a+b"]);
  });

  it("lists tag values of a key", async () => {
    const { f, calls } = fakeFetch(json({ values: ["dev"] }));
    await expect(fetchTagValues("m", "env", 10, f)).resolves.toEqual(["dev"]);
    expect(calls).toEqual(["/api/v1/tags/values?metric=m&key=env&limit=10"]);
  });

  it.each([
    ["missing field", {}],
    ["non-strings", { metrics: [1] }],
    ["not an object", ["a"]],
  ])("rejects a body with %s", async (_, body) => {
    await expect(fetchMetricNames("", 5, fakeFetch(json(body)).f)).rejects.toThrow(
      "ozyd sent an unexpected /api/v1/metrics response",
    );
  });
});

describe("buildQueryParams", () => {
  it("spells every parameter as the API expects", () => {
    const q = buildQueryParams(state, { from: 100, to: 200 }, 10);
    expect(Object.fromEntries(q)).toEqual({
      metric: "http.request.count",
      filter: "env:dev,!route:/api/*",
      by: "route,env",
      agg: "sum",
      from: "100",
      to: "200",
      interval: "10",
    });
  });

  it("omits empty filter and by, and leaves the interval to the server", () => {
    const q = buildQueryParams({ ...DEFAULT_EXPLORER_STATE, metric: "m" }, { from: 1, to: 2 });
    expect(q.toString()).toBe("metric=m&agg=avg&from=1&to=2");
  });
});

describe("fetchQuery", () => {
  it("requests the query and returns a valid result", async () => {
    const { f, calls } = fakeFetch(json(result));
    await expect(fetchQuery(state, { from: 1, to: 2 }, f)).resolves.toEqual(result);
    expect(calls[0]).toMatch(/^\/api\/v1\/query\?metric=http\.request\.count&filter=/);
  });

  it("rejects a malformed result", async () => {
    const { f } = fakeFetch(json({ ...result, series: [{ metric: "m", tags: {}, points: [[1, "x"]] }] }));
    await expect(fetchQuery(state, { from: 1, to: 2 }, f)).rejects.toThrow("unexpected /api/v1/query response");
  });
});

describe("isQueryResult", () => {
  it("accepts the documented example", () => {
    expect(isQueryResult(result)).toBe(true);
    expect(isQueryResult({ ...result, series: [] })).toBe(true);
  });

  it.each([
    ["an error body", { status: "error", error: "x" }],
    ["no interval", { ...result, interval: undefined }],
    ["series not an array", { ...result, series: {} }],
    ["numeric tag values", { ...result, series: [{ metric: "m", tags: { a: 1 }, points: [] }] }],
    ["a short point", { ...result, series: [{ metric: "m", tags: {}, points: [[1]] }] }],
    ["a string timestamp", { ...result, series: [{ metric: "m", tags: {}, points: [["1", 2]] }] }],
    ["null", null],
  ])("rejects %s", (_, v) => {
    expect(isQueryResult(v)).toBe(false);
  });
});
