import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { routes } from "../../app/routes";
import type { TimeseriesChartProps } from "../../charts/TimeseriesChart";

// uPlot needs canvas and matchMedia, which jsdom lacks; the chart wrapper is
// exercised in the browser. Here it's a stand-in that shows what it was given.
vi.mock("../../charts/TimeseriesChart", () => ({
  TimeseriesChart: ({ labels, data, xRange }: TimeseriesChartProps) => (
    <div data-testid="chart" data-points={JSON.stringify(data)} data-xrange={JSON.stringify(xRange)}>
      {labels.join(" | ")}
    </div>
  ),
}));

const queryBody = {
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
    { metric: "http.request.count", tags: { route: "/api/me" }, points: [[1_790_000_020_000, 1]] },
  ],
};

type Api = (url: URL) => { status?: number; body: unknown };

const defaultApi: Api = (url) => {
  switch (url.pathname) {
    case "/api/v1/metrics": {
      const prefix = url.searchParams.get("prefix") ?? "";
      const all = ["http.request.count", "http.request.duration", "system.cpu.user"];
      return { body: { metrics: all.filter((m) => m.startsWith(prefix)) } };
    }
    case "/api/v1/tags":
      return { body: { keys: ["env", "route"] } };
    case "/api/v1/tags/values":
      return { body: { values: url.searchParams.get("key") === "env" ? ["dev", "prod"] : ["/api/comics", "/api/me"] } };
    case "/api/v1/query":
      return { body: queryBody };
    default:
      return { status: 404, body: { status: "error", error: "not found" } };
  }
};

function mockApi(api: Api = defaultApi) {
  const f = vi.fn(async (input: string) => {
    const { status = 200, body } = api(new URL(input, "http://localhost"));
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const params = (router: ReturnType<typeof renderAt>) => new URLSearchParams(router.state.location.search);

const queryCalls = (f: ReturnType<typeof mockApi>) =>
  f.mock.calls.map(([u]) => new URL(u, "http://localhost")).filter((u) => u.pathname === "/api/v1/query");

afterEach(() => vi.unstubAllGlobals());

describe("MetricsExplorer", () => {
  it("is where the Metrics section leads, and starts with an empty state", async () => {
    mockApi();
    const router = renderAt("/metrics");
    expect(await screen.findByRole("heading", { name: "Metrics Explorer" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/metrics/explorer");
    expect(screen.getByText("Choose a metric to chart it.")).toBeInTheDocument();
  });

  it("autocompletes the metric name and writes the choice to the URL", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer");
    await user.type(await screen.findByRole("combobox", { name: "Metric" }), "http.request.d");
    // Until the debounced prefix settles, the previous (unfiltered) list stays up.
    await waitFor(() => expect(screen.queryByRole("option", { name: "system.cpu.user" })).not.toBeInTheDocument());
    const option = screen.getByRole("option", { name: "http.request.duration" });
    // Debounced: one request for the settled prefix, not one per keystroke.
    const prefixes = f.mock.calls
      .map(([u]) => new URL(u, "http://localhost"))
      .filter((u) => u.pathname === "/api/v1/metrics")
      .map((u) => u.searchParams.get("prefix"));
    expect(prefixes.length).toBeLessThan(5);
    await user.click(option);
    await waitFor(() => expect(params(router).get("metric")).toBe("http.request.duration"));
    expect(await screen.findByTestId("chart")).toBeInTheDocument();
  });

  it("charts a deep link, labelling each series by its group tags", async () => {
    const f = mockApi();
    renderAt("/metrics/explorer?metric=http.request.count&by=route&agg=sum&filter=env:dev&range=15m");
    const chart = await screen.findByTestId("chart");
    expect(chart).toHaveTextContent("http.request.count{route:/api/comics} | http.request.count{route:/api/me}");
    expect(JSON.parse(chart.dataset.points!)).toEqual([
      [1_790_000_000, 1_790_000_020],
      [4, null],
      [null, 1],
    ]);
    expect(JSON.parse(chart.dataset.xrange!)).toEqual([1_790_000_000, 1_790_003_600]);
    expect(screen.getByText("2 series · 20s buckets")).toBeInTheDocument();
    const [call] = queryCalls(f);
    expect(Object.fromEntries(call!.searchParams)).toMatchObject({
      metric: "http.request.count",
      by: "route",
      agg: "sum",
      filter: "env:dev",
    });
    expect(Number(call!.searchParams.get("to")) - Number(call!.searchParams.get("from"))).toBe(900);
    expect(screen.getByRole("combobox", { name: "Metric" })).toHaveValue("http.request.count");
    expect(screen.getByRole("combobox", { name: "Aggregator" })).toHaveValue("sum");
  });

  it("adds a filter chip from tag key and value suggestions, then removes it", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer?metric=http.request.count");
    await screen.findByTestId("chart");

    await user.click(screen.getByRole("button", { name: "+ Add filter" }));
    await user.click(await screen.findByRole("option", { name: "env" }));
    await user.click(await screen.findByRole("option", { name: "prod" }));

    await waitFor(() => expect(params(router).get("filter")).toBe("env:prod"));
    const filters = screen.getByRole("group", { name: "Filters" });
    expect(within(filters).getByText("env:prod")).toBeInTheDocument();
    await waitFor(() => expect(queryCalls(f).at(-1)?.searchParams.get("filter")).toBe("env:prod"));

    await user.click(screen.getByRole("button", { name: "Remove filter env:prod" }));
    await waitFor(() => expect(params(router).has("filter")).toBe(false));
  });

  it("makes a negated wildcard filter from typed text", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer?metric=http.request.count");
    await user.click(await screen.findByRole("button", { name: "+ Add filter" }));
    await user.type(await screen.findByRole("combobox", { name: "Tag key" }), "!route{Enter}");
    expect(screen.getByRole("button", { name: /^Not equal/ })).toBeInTheDocument();
    await user.type(await screen.findByRole("combobox", { name: "Tag value" }), "/api/*{Enter}");
    await waitFor(() => expect(params(router).get("filter")).toBe("!route:/api/*"));
  });

  it("cancels an unfinished filter", async () => {
    mockApi();
    const user = userEvent.setup();
    renderAt("/metrics/explorer?metric=http.request.count");
    await user.click(await screen.findByRole("button", { name: "+ Add filter" }));
    await user.click(screen.getByRole("button", { name: "Cancel filter" }));
    expect(screen.queryByRole("combobox", { name: "Tag key" })).not.toBeInTheDocument();
  });

  it("groups by tag keys, changes the aggregator and range, all through the URL", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer?metric=http.request.count");
    await screen.findByTestId("chart");

    await user.click(screen.getByText("by everything"));
    await user.click(await screen.findByRole("checkbox", { name: "route" }));
    await waitFor(() => expect(params(router).get("by")).toBe("route"));
    await user.click(screen.getByRole("checkbox", { name: "env" }));
    await waitFor(() => expect(params(router).get("by")).toBe("route,env"));

    await user.selectOptions(screen.getByRole("combobox", { name: "Aggregator" }), "max");
    await waitFor(() => expect(params(router).get("agg")).toBe("max"));

    await user.selectOptions(screen.getByRole("combobox", { name: "Time range" }), "4h");
    await waitFor(() => expect(params(router).get("range")).toBe("4h"));

    await user.click(screen.getByRole("checkbox", { name: /Auto-refresh/ }));
    await waitFor(() => expect(params(router).get("live")).toBe("0"));
  });

  it("applies a custom absolute range and turns auto-refresh off", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer?metric=http.request.count");
    await screen.findByTestId("chart");
    await user.selectOptions(screen.getByRole("combobox", { name: "Time range" }), "custom");
    const from = screen.getByLabelText("From");
    const to = screen.getByLabelText("To");
    await user.clear(from);
    await user.type(from, "2026-09-19T10:00");
    await user.clear(to);
    await user.type(to, "2026-09-19T09:00");
    expect(screen.getByRole("button", { name: "Apply" })).toBeDisabled();
    await user.clear(to);
    await user.type(to, "2026-09-19T11:30");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(params(router).get("from")).toBe(String(new Date(2026, 8, 19, 10).getTime() / 1000)));
    expect(params(router).get("to")).toBe(String(new Date(2026, 8, 19, 11, 30).getTime() / 1000));
    expect(screen.getByRole("checkbox", { name: /Auto-refresh/ })).toBeDisabled();
  });

  it("shows the API's error message", async () => {
    mockApi((url) =>
      url.pathname === "/api/v1/query"
        ? { status: 400, body: { status: "error", error: 'unknown tag key "rout" in by' } }
        : defaultApi(url),
    );
    renderAt("/metrics/explorer?metric=http.request.count&by=rout");
    expect(await screen.findByRole("alert")).toHaveTextContent('unknown tag key "rout" in by');
    expect(screen.queryByTestId("chart")).not.toBeInTheDocument();
  });

  it("shows loading, then an empty state when nothing matched", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: string) => {
        const url = new URL(input, "http://localhost");
        if (url.pathname === "/api/v1/query") {
          await gate;
          return new Response(JSON.stringify({ ...queryBody, series: [] }));
        }
        return new Response(JSON.stringify(defaultApi(url).body));
      }),
    );
    renderAt("/metrics/explorer?metric=nothing.here");
    expect(await screen.findByRole("status")).toHaveTextContent("Loading nothing.here…");
    release();
    expect(await screen.findByText("No data for this query in the selected time range.")).toBeInTheDocument();
  });
});
