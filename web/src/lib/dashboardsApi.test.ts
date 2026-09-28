import { describe, expect, it } from "vitest";
import { ApiError } from "./metricsApi";
import {
  fetchBatch,
  fetchDashboard,
  fetchDashboards,
  fetchServiceDashboards,
  fetchServices,
  fetchSketch,
} from "./dashboardsApi";

function fakeFetch(body: unknown, status = 200) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const f = ((url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return Promise.resolve(new Response(JSON.stringify(body), { status }));
  }) as unknown as typeof fetch;
  return { f, calls };
}

const definition = {
  title: "Checkout",
  widgets: [{ id: "w1", type: "timeseries", layout: { x: 0, y: 0, w: 6, h: 3 }, queries: [{ q: "sum:x{*}" }] }],
};
const stored = { ...definition, id: 2, provisioned: true, created_at: "t", updated_at: "t" };

describe("fetchDashboards", () => {
  it("returns the rows and the ones that could not be read", async () => {
    const { f } = fakeFetch({ status: "ok", count: 1, dashboards: [stored], unreadable: [5] });
    await expect(fetchDashboards(f)).resolves.toEqual({ dashboards: [stored], unreadable: [5] });
  });

  it("tolerates a server too old to send unreadable", async () => {
    const { f } = fakeFetch({ status: "ok", dashboards: [] });
    await expect(fetchDashboards(f)).resolves.toEqual({ dashboards: [], unreadable: [] });
  });

  // The contract, not whatever arrived: a drifted server is a sentence in the
  // page rather than a crash inside a chart.
  it("refuses a row that is not a dashboard", async () => {
    const { f } = fakeFetch({ dashboards: [{ id: 1, title: "no widgets" }] });
    await expect(fetchDashboards(f)).rejects.toBeInstanceOf(ApiError);
  });
});

describe("fetchDashboard", () => {
  it("fetches one by id", async () => {
    const { f, calls } = fakeFetch(stored);
    await expect(fetchDashboard(2, f)).resolves.toEqual(stored);
    expect(calls[0]?.url).toBe("/api/v1/dashboards/2");
  });

  it("turns a 404 into its message", async () => {
    const { f } = fakeFetch({ error: "no dashboard with id 9" }, 404);
    await expect(fetchDashboard(9, f)).rejects.toThrow("no dashboard with id 9");
  });
});

describe("fetchServices", () => {
  it("reads the names and whether the answer was partial", async () => {
    const { f } = fakeFetch({ status: "ok", services: ["a", "b"], truncated: true, unreadable: [] });
    await expect(fetchServices(f)).resolves.toEqual({ services: ["a", "b"], truncated: true });
  });

  it("treats a missing truncated flag as complete", async () => {
    const { f } = fakeFetch({ services: [] });
    await expect(fetchServices(f)).resolves.toEqual({ services: [], truncated: false });
  });
});

describe("fetchServiceDashboards", () => {
  it("reads every template instantiated for the service", async () => {
    const { f, calls } = fakeFetch({
      status: "ok",
      service: "checkout",
      dashboards: [{ template_id: 3, template_uid: "svc", service: "checkout", dashboard: definition }],
    });
    await expect(fetchServiceDashboards("checkout", f)).resolves.toEqual([
      { template_id: 3, template_uid: "svc", service: "checkout", dashboard: definition },
    ]);
    expect(calls[0]?.url).toBe("/api/v1/dashboards/service/checkout");
  });

  // A tag value is free text, so a service can be named "a/b" — and an
  // unescaped slash would be two path segments and a router 404.
  it("escapes a service name that needs it", async () => {
    const { f, calls } = fakeFetch({ dashboards: [] });
    await fetchServiceDashboards("a/b", f);
    expect(calls[0]?.url).toBe("/api/v1/dashboards/service/a%2Fb");
  });
});

describe("fetchBatch", () => {
  const range = { from: 10, to: 20 };

  it("posts the queries, the window and the variables", async () => {
    const { f, calls } = fakeFetch({ status: "ok", from: 10, to: 20, results: [] });
    await fetchBatch(["sum:x{*}"], range, { env: ["env:prod"] }, f);
    expect(calls[0]?.url).toBe("/api/v1/query/batch");
    expect(calls[0]?.init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
      queries: [{ q: "sum:x{*}" }],
      from: 10,
      to: 20,
      vars: { env: ["env:prod"] },
    });
  });

  // The rule the whole endpoint exists for: one typo in one widget must not
  // blank the other eleven, so a failed *query* is a successful *request*.
  it("resolves when a query failed but the request did not", async () => {
    const { f } = fakeFetch({
      status: "ok",
      from: 10,
      to: 20,
      results: [
        { index: 0, status: "ok", query: "sum:x{*}", interval: 10, series: [], warnings: [] },
        { index: 1, status: "error", query: "bad", interval: 0, series: [], warnings: [], code: 400, error: "nope" },
      ],
    });
    const res = await fetchBatch(["sum:x{*}", "bad"], range, {}, f);
    expect(res.results[1]).toMatchObject({ status: "error", code: 400, error: "nope" });
  });

  it("rejects when the request itself was refused", async () => {
    const { f } = fakeFetch({ error: "a batch takes at most 50 queries" }, 400);
    await expect(fetchBatch([], range, {}, f)).rejects.toThrow("a batch takes at most 50 queries");
  });

  it("refuses a response whose results are not results", async () => {
    const { f } = fakeFetch({ from: 1, to: 2, results: [{ index: 0 }] });
    await expect(fetchBatch(["x"], range, {}, f)).rejects.toBeInstanceOf(ApiError);
  });

  it("reports an unreachable server rather than throwing a TypeError", async () => {
    const f = (() => Promise.reject(new TypeError("Failed to fetch"))) as unknown as typeof fetch;
    await expect(fetchBatch(["x"], range, {}, f)).rejects.toThrow(/unreachable/);
  });
});

describe("fetchSketch", () => {
  const range = { from: 1_790_000_000, to: 1_790_003_600 };
  const body = {
    status: "ok",
    query: "dist:lat{*}",
    from: 1_790_000_000,
    to: 1_790_003_600,
    interval: 20,
    bins: 3,
    series: [
      {
        metric: "lat",
        tags: {},
        scope: "*",
        buckets: [
          { t: 1_790_000_000_000, gamma: 1.02, count: 51, sum: 6.13, min: 0.004, max: 1.9, bins: [[0.0039, 0.004, 43], [1.86, 1.9, 1]] },
        ],
      },
    ],
    warnings: [],
  };

  it("POSTs the query, the window and the variables", async () => {
    const { f, calls } = fakeFetch(body);
    await fetchSketch("dist:lat{$env}", range, { env: ["env:prod"] }, f);
    expect(calls[0]?.url).toBe("/api/v1/query/sketch");
    expect(calls[0]?.init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
      q: "dist:lat{$env}",
      from: range.from,
      to: range.to,
      vars: { env: ["env:prod"] },
    });
  });

  it("returns the buckets and their bins", async () => {
    const { f } = fakeFetch(body);
    const res = await fetchSketch("dist:lat{*}", range, {}, f);
    expect(res.interval).toBe(20);
    expect(res.series[0]?.buckets[0]?.bins).toEqual([[0.0039, 0.004, 43], [1.86, 1.9, 1]]);
    expect(res.series[0]?.buckets[0]?.gamma).toBe(1.02);
  });

  // An empty sketch reports its min as +Inf, which the server writes as null.
  // Read as a number that would become a silent 0 — a tooltip claiming the
  // fastest request took no time at all.
  it("keeps a null min as null rather than as zero", async () => {
    const bucket = { t: 1, gamma: 1.02, count: 0, sum: null, min: null, max: null, bins: [] };
    const { f } = fakeFetch({ ...body, series: [{ ...body.series[0], buckets: [bucket] }] });
    const res = await fetchSketch("dist:lat{*}", range, {}, f);
    expect(res.series[0]?.buckets[0]?.min).toBeNull();
    expect(res.series[0]?.buckets[0]?.max).toBeNull();
  });

  it("tolerates a server that sends no warnings and no bin total", async () => {
    const { f } = fakeFetch({ from: 1, to: 2, interval: 10, series: [] });
    await expect(fetchSketch("dist:lat{*}", range, {}, f)).resolves.toMatchObject({ bins: 0, warnings: [] });
  });

  // Unlike the batch, a refused query here is a refused request: there is no
  // per-query status to put the failure in.
  it("rejects with the server's own explanation", async () => {
    const { f } = fakeFetch({ error: "http.request.count is not a distribution" }, 400);
    await expect(fetchSketch("dist:http.request.count{*}", range, {}, f)).rejects.toThrow("is not a distribution");
  });

  // The server writes a bound it cannot express as a float64 as null, and says
  // so in its own comment: a bucket index near the wire format's limit
  // overflows gamma^k. Refusing the bin would lose the whole distribution over
  // one bound nobody can plot anyway.
  it("reads a null bound as NaN rather than refusing the response", async () => {
    const bins = [[null, 1e308, 3], [1, 2, 4]];
    const { f } = fakeFetch({ ...body, series: [{ ...body.series[0], buckets: [{ t: 1, gamma: 1.02, bins }] }] });
    const res = await fetchSketch("dist:lat{*}", range, {}, f);
    const decoded = res.series[0]?.buckets[0]?.bins[0];
    expect(decoded?.[0]).toBeNaN();
    expect(decoded?.[2]).toBe(3);
  });

  it("still refuses a bin with no count, which says nothing at all", async () => {
    const bins = [[1, 2, null]];
    const { f } = fakeFetch({ ...body, series: [{ ...body.series[0], buckets: [{ t: 1, gamma: 1.02, bins }] }] });
    await expect(fetchSketch("dist:lat{*}", range, {}, f)).rejects.toBeInstanceOf(ApiError);
  });

  it("refuses a bin that is not three numbers", async () => {
    const bad = { ...body, series: [{ ...body.series[0], buckets: [{ t: 1, gamma: 1.02, bins: [[1, 2]] }] }] };
    const { f } = fakeFetch(bad);
    await expect(fetchSketch("dist:lat{*}", range, {}, f)).rejects.toBeInstanceOf(ApiError);
  });

  it("refuses a response with no interval, which every column's width comes from", async () => {
    const { f } = fakeFetch({ from: 1, to: 2, series: [] });
    await expect(fetchSketch("dist:lat{*}", range, {}, f)).rejects.toBeInstanceOf(ApiError);
  });
});
