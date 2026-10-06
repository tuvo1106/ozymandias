import { fetchResources, fetchServiceMap, fetchServices, fetchTrace, fetchTraces, searchParams } from "./apmApi";

const reply = (body: unknown, status = 200) => vi.fn(async () => new Response(JSON.stringify(body), { status })) as unknown as typeof fetch;
const urlOf = (f: typeof fetch) => new URL((f as unknown as { mock: { calls: string[][] } }).mock.calls[0]![0]!, "http://x");
const W = { from: 1000, to: 2000 };
const summary = { trace_id: "t", span_id: "s", env: "dev", service: "shop", name: "http.request", resource: "GET /x", start: 5, duration: 9, error: false, trace_error: true };
const span = { trace_id: "t", span_id: "s", service: "shop", name: "n", resource: "r", type: "web", start: 5, duration: 9, error: 0 };
const red = { service: "shop", name: "http.request", requests: 10, requests_per_second: 1, errors: 1, error_pct: 10, p50_ms: 1, p95_ms: null, p99_ms: null, sparkline: [1, 2] };

describe("searchParams", () => {
  it("sends only the filters that are set, the window in ms, and a default limit", () => {
    expect(Object.fromEntries(searchParams({}, W))).toEqual({ from: "1000", to: "2000", limit: "50" });
    const p = searchParams(
      { env: "dev", service: "shop", resource: "get /x", name: "http.request", error: true, minDurationMs: 0, maxDurationMs: 90, statusCode: 503 },
      W,
      { limit: 10, cursor: "c" },
    );
    expect(Object.fromEntries(p)).toEqual({
      from: "1000", to: "2000", limit: "10", env: "dev", service: "shop", resource: "get /x", name: "http.request", error: "true",
      min_duration_ms: "0", max_duration_ms: "90", status_code: "503", cursor: "c",
    });
  });
  it("omits error when false", () => {
    expect(searchParams({ error: false }, W).has("error")).toBe(false);
  });
});

describe("fetchTraces", () => {
  it("reads a page with its cursor and examined count", async () => {
    const f = reply({ traces: [summary], cursor: "next", examined: 37 });
    expect(await fetchTraces({ service: "shop" }, W, {}, f)).toEqual({ traces: [summary], cursor: "next", examined: 37 });
    expect(urlOf(f).pathname).toBe("/api/v1/traces");
  });
  it("reads a last page: no cursor", async () => {
    expect((await fetchTraces({}, W, {}, reply({ traces: [] }))).cursor).toBeUndefined();
  });
  it.each([[{}], [{ traces: [{ ...summary, duration: "9" }] }], [{ traces: [{ ...summary, error: 0 }] }], [[]]])("rejects a drifted body %j", async (body) => {
    await expect(fetchTraces({}, W, {}, reply(body))).rejects.toThrow(/unexpected \/api\/v1\/traces response/);
  });
  it("surfaces the server's own error", async () => {
    await expect(fetchTraces({}, W, {}, reply({ error: "bad cursor" }, 400))).rejects.toThrow("bad cursor");
  });
});

describe("fetchTrace", () => {
  it("reads a trace, with defaults for what the server may omit", async () => {
    const t = await fetchTrace("a/b", reply({ trace_id: "t", spans: [span, { ...span, span_id: "c", parent_id: "s", meta: { a: "b" } }], start: 5, duration: 9 }));
    expect(t.spans).toHaveLength(2);
    expect(t).toMatchObject({ services: [], orphans: [], errors: 0, span_count: 2 });
  });
  it("encodes the id in the path", async () => {
    const f = reply({ spans: [], start: 0, duration: 0 });
    await fetchTrace("a/b", f);
    expect(urlOf(f).pathname).toBe("/api/v1/traces/a%2Fb");
  });
  it("reports a 404 with its status, so the page can say 'not stored' rather than 'broken'", async () => {
    await expect(fetchTrace("x", reply({ error: "trace not found" }, 404))).rejects.toMatchObject({ status: 404, message: "trace not found" });
  });
  it.each([[{ spans: [{ ...span, start: "1" }], start: 1, duration: 1 }], [{ spans: [], duration: 1 }], [{ spans: [{ ...span, meta: [] }], start: 1, duration: 1 }]])("rejects %j", async (body) => {
    await expect(fetchTrace("x", reply(body))).rejects.toThrow(/unexpected/);
  });
});

describe("fetchServiceMap", () => {
  it("reads nodes and edges and sends env", async () => {
    const f = reply({ nodes: [{ service: "a", calls_in: 1, errors_in: 0 }], edges: [{ parent: "a", child: "b", calls: 2, errors: 1, avg_duration: 5 }] });
    const m = await fetchServiceMap("dev", W, f);
    expect(m.edges[0]!.child).toBe("b");
    expect(urlOf(f).searchParams.get("env")).toBe("dev");
  });
  it("treats an empty map (null arrays) as empty, and rejects junk", async () => {
    expect(await fetchServiceMap("", W, reply({ nodes: null, edges: null }))).toEqual({ nodes: [], edges: [] });
    await expect(fetchServiceMap("", W, reply({ nodes: [{ service: 1 }] }))).rejects.toThrow(/service-map/);
    await expect(fetchServiceMap("", W, reply([]))).rejects.toThrow(/service-map/);
  });
});

describe("fetchServices and fetchResources", () => {
  it("keeps a null percentile null: no sketches is not 0 ms", async () => {
    const t = await fetchServices("", W, reply({ services: [red], from: 1, to: 2 }));
    expect(t.services[0]!.p95_ms).toBeNull();
    expect(t.services[0]!.p50_ms).toBe(1);
  });
  it("tolerates a missing sparkline and rejects a missing percentile field", async () => {
    const noSpark: Partial<typeof red> = { ...red };
    delete noSpark.sparkline;
    expect((await fetchServices("", W, reply({ services: [noSpark], from: 1, to: 2 }))).services[0]!.sparkline).toEqual([]);
    const noP95: Partial<typeof red> = { ...red };
    delete noP95.p95_ms;
    await expect(fetchServices("", W, reply({ services: [noP95], from: 1, to: 2 }))).rejects.toThrow(/unexpected/);
  });
  it("asks for one service's resources by path, with env and window", async () => {
    const f = reply({ services: [{ ...red, resource: "get /x" }], from: 1, to: 2 });
    await fetchResources("my svc", "dev", W, f);
    const u = urlOf(f);
    expect(u.pathname).toBe("/api/v1/services/my%20svc/resources");
    expect(Object.fromEntries(u.searchParams)).toEqual({ from: "1000", to: "2000", env: "dev" });
  });
  it("rejects a body without its window", async () => {
    await expect(fetchResources("s", "", W, reply({ services: [] }))).rejects.toThrow(/unexpected/);
  });
});
