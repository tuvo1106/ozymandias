import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../../app/routes";
import type { TimeseriesChartProps } from "../../charts/TimeseriesChart";
import type { HeatmapChartProps } from "../../charts/HeatmapChart";

// uPlot needs a canvas and matchMedia, which jsdom has neither of. Both chart
// wrappers are exercised in the browser; here they stand in and show what they
// were handed, which is what these tests are actually about — the page's job
// is to work out what to draw, not to draw it.
vi.mock("../../charts/TimeseriesChart", () => ({
  TimeseriesChart: ({ labels, data, xRange, syncKey }: TimeseriesChartProps) => (
    <div data-testid="chart" data-points={JSON.stringify(data)} data-xrange={JSON.stringify(xRange)} data-sync={syncKey}>
      {labels.join(" | ")}
    </div>
  ),
}));

vi.mock("../../charts/HeatmapChart", () => ({
  HeatmapChart: ({ buckets, interval, xRange, yRange, log, syncKey }: HeatmapChartProps) => (
    <div
      data-testid="heatmap"
      data-buckets={JSON.stringify(buckets)}
      data-interval={interval}
      data-xrange={JSON.stringify(xRange)}
      data-yrange={JSON.stringify(yRange)}
      data-log={String(log)}
      data-sync={syncKey}
    />
  ),
}));

const widgets = [
  {
    id: "req",
    type: "timeseries",
    title: "Throughput",
    layout: { x: 0, y: 0, w: 8, h: 3 },
    queries: [{ q: "sum:http.request.count{$env}.as_rate()" }],
  },
  {
    id: "errs",
    type: "query_value",
    title: "5xx rate",
    layout: { x: 8, y: 0, w: 4, h: 3 },
    queries: [{ q: "sum:http.request.count{status:5*}.as_rate()", reducer: "avg" }],
    precision: 2,
    conditional_formats: [{ op: ">", value: 1, color: "red" }],
  },
  {
    id: "lat",
    type: "heatmap",
    title: "Latency distribution",
    layout: { x: 0, y: 3, w: 8, h: 3 },
    queries: [{ q: "dist:http.request.duration{*}" }],
    yaxis: { unit: "ms", scale: "log" },
  },
  {
    id: "runbook",
    type: "note",
    layout: { x: 8, y: 3, w: 4, h: 3 },
    markdown: "Escalate to **oncall**",
  },
];

const dashboard = {
  id: 2,
  provisioned: false,
  created_at: "t",
  updated_at: "t",
  title: "Checkout",
  description: "the money path",
  template_vars: [{ name: "env", tag: "env", default: "prod" }],
  widgets,
};

const sketch = {
  status: "ok",
  query: "dist:http.request.duration{*}",
  from: 1_790_000_000,
  to: 1_790_003_600,
  interval: 60,
  bins: 2,
  series: [
    {
      metric: "http.request.duration",
      tags: {},
      scope: "*",
      buckets: [
        {
          t: 1_790_000_000_000,
          gamma: 1.02,
          count: 44,
          sum: 6.1,
          min: 0.004,
          max: 1.9,
          bins: [
            [0.0039, 0.004, 43],
            [1.86, 1.9, 1],
          ],
        },
      ],
    },
  ],
  warnings: [],
};

const batch = {
  status: "ok",
  from: 1_790_000_000,
  to: 1_790_003_600,
  results: [
    {
      index: 0,
      status: "ok",
      query: "sum:http.request.count{env:prod}.as_rate()",
      interval: 60,
      series: [{ metric: "http.request.count", tags: {}, points: [[1_790_000_000_000, 12]] }],
      warnings: [],
    },
    {
      index: 1,
      status: "ok",
      query: "sum:http.request.count{status:5*}.as_rate()",
      interval: 60,
      series: [{ metric: "http.request.count", tags: {}, points: [[1_790_000_000_000, 3.5]] }],
      warnings: [],
    },
  ],
};

interface Reply {
  status?: number;
  body: unknown;
}
type Api = (url: URL, init: RequestInit | undefined) => Reply;

const defaultApi: Api = (url) => {
  switch (url.pathname) {
    case "/api/v1/dashboards":
      return { body: { status: "ok", count: 1, dashboards: [dashboard], unreadable: [] } };
    case "/api/v1/dashboards/2":
      return { body: dashboard };
    case "/api/v1/query/batch":
      return { body: batch };
    case "/api/v1/query/sketch":
      return { body: sketch };
    case "/api/v1/tags/values":
      return { body: { values: ["dev", "prod"] } };
    case "/api/v1/health":
      return { body: { status: "ok" } };
    default:
      return { status: 404, body: { status: "error", error: `no route for ${url.pathname}` } };
  }
};

/** A reply of `HANG` never resolves, which is what "still in flight" is. */
const HANG = Symbol("hang");

function mockApi(api: Api = defaultApi) {
  const f = vi.fn(async (input: string, init?: RequestInit) => {
    const reply = api(new URL(input, "http://localhost"), init);
    if (reply.body === HANG) return new Promise<Response>(() => {});
    return new Response(JSON.stringify(reply.body), { status: reply.status ?? 200 });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const bodies = (f: ReturnType<typeof mockApi>, pathname: string) =>
  f.mock.calls
    .filter(([u]) => new URL(u, "http://localhost").pathname === pathname)
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)) as Record<string, unknown>);

afterEach(() => vi.unstubAllGlobals());

describe("a stored dashboard", () => {
  it("draws every widget type from one batch and one sketch", async () => {
    const f = mockApi();
    renderAt("/dashboards/2");

    expect(await screen.findByRole("heading", { name: "Checkout" })).toBeInTheDocument();
    expect(screen.getByText("the money path")).toBeInTheDocument();

    // The line chart got its query's series; the query_value reduced its own.
    expect(await screen.findByTestId("chart")).toHaveTextContent("http.request.count{*}");
    expect(within(screen.getByRole("region", { name: "5xx rate" })).getByText("3.50")).toBeInTheDocument();
    expect(screen.getByText("Escalate to **oncall**")).toBeInTheDocument();

    // One batch for the four batchable queries, one sketch for the heatmap.
    await waitFor(() => expect(bodies(f, "/api/v1/query/sketch")).toHaveLength(1));
    expect(bodies(f, "/api/v1/query/batch")).toHaveLength(1);
    expect(bodies(f, "/api/v1/query/batch")[0]?.queries).toEqual([
      { q: "sum:http.request.count{$env}.as_rate()" },
      { q: "sum:http.request.count{status:5*}.as_rate()" },
    ]);
    // A note has no query, and the heatmap's is not in the batch: the two
    // endpoints refuse each other's queries on purpose.
    expect(bodies(f, "/api/v1/query/sketch")[0]?.q).toBe("dist:http.request.duration{*}");
  });

  it("hands the heatmap its buckets, its interval and the axis the author asked for", async () => {
    mockApi();
    renderAt("/dashboards/2");
    const heatmap = await screen.findByTestId("heatmap");
    expect(heatmap.dataset.interval).toBe("60");
    expect(heatmap.dataset.log).toBe("true");
    // The axis spans the bins, since the definition pinned neither end.
    expect(JSON.parse(heatmap.dataset.yrange!)).toEqual([0.0039, 1.9]);
    expect(JSON.parse(heatmap.dataset.buckets!)).toHaveLength(1);
    // The window the server evaluated, so every chart on the page lines up.
    expect(JSON.parse(heatmap.dataset.xrange!)).toEqual([1_790_000_000, 1_790_003_600]);
  });

  it("puts every chart on the dashboard in one cursor group", async () => {
    mockApi();
    renderAt("/dashboards/2");
    const heatmap = await screen.findByTestId("heatmap");
    expect(screen.getByTestId("chart").dataset.sync).toBe("dashboard-2");
    expect(heatmap.dataset.sync).toBe("dashboard-2");
  });

  it("says how accurate the bands are, because a sketch is not exact", async () => {
    mockApi();
    renderAt("/dashboards/2");
    expect(await screen.findByText("Bands are accurate to ±1.0%")).toBeInTheDocument();
  });

  it("draws one heatmap's failure without blanking the widgets beside it", async () => {
    mockApi((url) =>
      url.pathname === "/api/v1/query/sketch"
        ? { status: 400, body: { error: "http.request.duration is not a distribution" } }
        : defaultApi(url, undefined),
    );
    renderAt("/dashboards/2");
    const widget = await screen.findByRole("region", { name: "Latency distribution" });
    expect(await within(widget).findByText(/is not a distribution/)).toBeInTheDocument();
    expect(screen.getByTestId("chart")).toBeInTheDocument();
    expect(screen.queryByTestId("heatmap")).not.toBeInTheDocument();
  });

  it("says no data rather than nothing when the sketch came back empty", async () => {
    mockApi((url) => (url.pathname === "/api/v1/query/sketch" ? { body: { ...sketch, series: [] } } : defaultApi(url, undefined)));
    renderAt("/dashboards/2");
    const widget = await screen.findByRole("region", { name: "Latency distribution" });
    expect(await within(widget).findByText("No data")).toBeInTheDocument();
  });

  it("counts out the groups it is not drawing, and the observations the axis cannot hold", async () => {
    const zeroes = {
      ...sketch,
      series: [
        {
          ...sketch.series[0],
          tags: { route: "/checkout" },
          buckets: [{ ...sketch.series[0]!.buckets[0]!, bins: [[0, 0, 7], [1.86, 1.9, 1]] }],
        },
        { ...sketch.series[0], tags: { route: "/cart" } },
      ],
    };
    mockApi((url) => (url.pathname === "/api/v1/query/sketch" ? { body: zeroes } : defaultApi(url, undefined)));
    renderAt("/dashboards/2");
    expect(await screen.findByText(/Showing route:\/checkout only — 1 other group matched/)).toBeInTheDocument();
    expect(screen.getByText("7 observations at or below zero, which a log axis cannot show")).toBeInTheDocument();
  });

  it("re-asks both endpoints when a template variable changes", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/dashboards/2");
    await screen.findByTestId("heatmap");
    await user.selectOptions(screen.getByRole("combobox", { name: "env (env)" }), "dev");
    await waitFor(() => expect(new URLSearchParams(router.state.location.search).get("var.env")).toBe("dev"));
    await waitFor(() => expect(bodies(f, "/api/v1/query/sketch")).toHaveLength(2));
    expect(bodies(f, "/api/v1/query/sketch")[1]?.vars).toEqual({ env: ["env:dev"] });
    expect(bodies(f, "/api/v1/query/batch")[1]?.vars).toEqual({ env: ["env:dev"] });
  });

  // The page keeps the previous answer rather than emptying, and dims it to say
  // that is what you are looking at. Not on an auto-refresh, which asks the
  // same question again and would otherwise blink every ten seconds.
  it("dims what is on screen while it answers a new window", async () => {
    let hang = false;
    mockApi((url, init) => (hang && url.pathname.startsWith("/api/v1/query") ? { body: HANG } : defaultApi(url, init)));
    const user = userEvent.setup();
    renderAt("/dashboards/2");
    await screen.findByTestId("chart");
    const body = () => screen.getByTestId("dashboard-grid").parentElement;
    expect(body()).not.toHaveClass("opacity-50");

    hang = true;
    await user.selectOptions(screen.getByRole("combobox", { name: "Time range" }), "4h");
    await waitFor(() => expect(body()).toHaveClass("opacity-50"));
    // And the previous answer is still there to look at.
    expect(screen.getByTestId("chart")).toBeInTheDocument();
  });

  it("explains a dashboard id that is not one, without asking the server", async () => {
    const f = mockApi();
    renderAt("/dashboards/nope");
    expect(await screen.findByRole("alert")).toHaveTextContent('"nope" is not a dashboard id');
    expect(f.mock.calls.some(([u]) => String(u).includes("/api/v1/dashboards/nope"))).toBe(false);
  });
});

describe("a service dashboard", () => {
  const instance = {
    template_id: 7,
    template_uid: "service-overview",
    dashboard: { ...dashboard, title: "Service overview: api", template: false, uid: undefined },
  };

  it("draws every template instantiated for the service", async () => {
    mockApi((url) =>
      url.pathname === "/api/v1/dashboards/service/api"
        ? { body: { status: "ok", service: "api", dashboards: [instance] } }
        : defaultApi(url, undefined),
    );
    renderAt("/dashboards/service/api");
    expect(await screen.findByRole("heading", { name: "Service overview: api" })).toBeInTheDocument();
    expect(screen.getByText("from the service-overview template")).toBeInTheDocument();
    expect((await screen.findByTestId("heatmap")).dataset.sync).toBe("service-api-7");
  });

  it("says so when no template covers the service, rather than drawing an empty grid", async () => {
    mockApi((url) =>
      url.pathname === "/api/v1/dashboards/service/ghost"
        ? { body: { status: "ok", service: "ghost", dashboards: [] } }
        : defaultApi(url, undefined),
    );
    renderAt("/dashboards/service/ghost");
    expect(await screen.findByText("No template dashboard covers ghost.")).toBeInTheDocument();
  });
});
