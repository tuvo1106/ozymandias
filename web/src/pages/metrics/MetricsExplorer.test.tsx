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

const Q = "sum:http.request.count{*} by {route}";

const queryBody = (q = Q, over: Record<string, unknown> = {}) => ({
  status: "ok",
  query: q,
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
  warnings: [],
  ...over,
});

type Reply = { status?: number; body: unknown } | Promise<{ status?: number; body: unknown }>;
type Api = (url: URL) => Reply;

const dashboardList = {
  dashboards: [
    { id: 4, provisioned: false, created_at: "t", updated_at: "t", title: "Checkout", widgets: [] },
    { id: 5, provisioned: true, created_at: "t", updated_at: "t", title: "Home", widgets: [] },
  ],
  unreadable: [],
};

const defaultApi: Api = (url) => {
  switch (url.pathname) {
    case "/api/v1/metrics":
      return { body: { metrics: ["http.request.count", "system.cpu.user"] } };
    case "/api/v1/tags":
      return { body: { keys: ["env", "route"] } };
    case "/api/v1/tags/values":
      return { body: { values: ["dev", "prod"] } };
    case "/api/v1/query/validate":
      return { body: { ok: true, query: "x" } };
    case "/api/v1/query": {
      // An M1 request answers with its translation in `query`, as ozyd does.
      const metric = url.searchParams.get("metric");
      return { body: queryBody(metric ? `${url.searchParams.get("agg") ?? "avg"}:${metric}{*}` : (url.searchParams.get("q") ?? "")) };
    }
    case "/api/v1/dashboards":
      return { body: dashboardList };
    default:
      return { status: 404, body: { status: "error", error: "not found" } };
  }
};

/**
 * The request as one URL: a POST's JSON fields are folded into its search
 * params, so a handler asks `u.searchParams.get("q")` whichever verb was used.
 */
function asked(input: string, init?: RequestInit): URL {
  const url = new URL(input, "http://localhost");
  if (init?.body) for (const [k, v] of Object.entries(JSON.parse(String(init.body)) as Record<string, unknown>)) url.searchParams.set(k, String(v));
  return url;
}

function mockApi(api: Api = defaultApi) {
  const f = vi.fn(async (input: string, init?: RequestInit) => {
    const { status = 200, body } = await api(asked(input, init));
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

/** An api that overrides some routes and falls through to the default. */
const over =
  (f: (url: URL) => Reply | undefined): Api =>
  (url) =>
    f(url) ?? defaultApi(url);

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const params = (router: ReturnType<typeof renderAt>) => new URLSearchParams(router.state.location.search);

const queryCalls = (f: ReturnType<typeof mockApi>) =>
  f.mock.calls.map(([u, init]) => asked(u, init)).filter((u) => u.pathname === "/api/v1/query");

const box = () => screen.getByRole("combobox", { name: "Query" }) as HTMLTextAreaElement;

const at = (q: string, rest = "") => `/metrics/explorer?${new URLSearchParams({ q })}${rest}`;

afterEach(() => vi.unstubAllGlobals());

describe("MetricsExplorer", () => {
  it("is where the Metrics section leads, and starts by asking for a query", async () => {
    const f = mockApi();
    const router = renderAt("/metrics");
    expect(await screen.findByRole("heading", { name: "Metrics Explorer" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/metrics/explorer");
    expect(screen.getByText("Write a query and run it (Ctrl+Enter) to chart it.")).toBeInTheDocument();
    expect(queryCalls(f)).toHaveLength(0);
  });

  it("runs the typed query on Ctrl+Enter, writing it to the URL", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer");
    await user.click(await screen.findByRole("combobox", { name: "Query" }));
    await user.paste(Q);
    expect(queryCalls(f)).toHaveLength(0);
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(params(router).get("q")).toBe(Q));
    expect(await screen.findByTestId("chart")).toBeInTheDocument();
    expect(queryCalls(f)[0]?.searchParams.get("q")).toBe(Q);
  });

  it("charts a link, labelling each series, with a legend of its numbers", async () => {
    const f = mockApi();
    renderAt(at(Q, "&range=15m"));
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
    expect(Number(call!.searchParams.get("to")) - Number(call!.searchParams.get("from"))).toBe(900);
    expect(box()).toHaveValue(Q);

    const rows = within(screen.getByRole("table", { name: "Series" })).getAllByRole("row");
    expect(rows.map((r) => r.textContent)).toEqual([
      "Serieslastavgminmax",
      "http.request.count{route:/api/comics}4444",
      "http.request.count{route:/api/me}1111",
    ]);
  });

  // A line with nothing in the window measured nothing; zeros would say it
  // measured zero.
  it("shows dashes, not zeros, for a line with no values", async () => {
    mockApi(over((u) => (u.pathname === "/api/v1/query" ? { body: queryBody(Q, { series: [{ metric: "m", tags: {}, points: [[1, null]] }] }) } : undefined)));
    renderAt(at(Q));
    const table = await screen.findByRole("table", { name: "Series" });
    expect(within(table).getAllByRole("row")[1]).toHaveTextContent("m{*}————");
  });

  // M1 wrote structured parameters. The server is the one translator: its
  // answer's `query` replaces them, and that answer draws the chart.
  it("asks ozyd what an M1 link means, and becomes that query without asking twice", async () => {
    const f = mockApi(
      over((u) =>
        u.searchParams.get("metric") ? { body: queryBody("sum:http.request.count{env:dev} by {route}") } : undefined,
      ),
    );
    const router = renderAt("/metrics/explorer?metric=http.request.count&by=route&agg=sum&filter=env:dev&range=4h");
    await screen.findByTestId("chart");
    await waitFor(() => expect(params(router).get("q")).toBe("sum:http.request.count{env:dev} by {route}"));
    expect(params(router).has("metric")).toBe(false);
    expect(params(router).get("range")).toBe("4h");
    expect(router.state.historyAction).toBe("REPLACE");
    expect(box()).toHaveValue("sum:http.request.count{env:dev} by {route}");
    const calls = queryCalls(f);
    expect(calls).toHaveLength(1);
    expect(Object.fromEntries(calls[0]!.searchParams)).toMatchObject({ metric: "http.request.count", by: "route", agg: "sum", filter: "env:dev" });
  });

  // What ozyd refuses, the page says it refused, in its words; nothing is
  // charted in its place.
  it("says ozyd refused an M1 link's parameters, and keeps them in the URL", async () => {
    mockApi(over((u) => (u.searchParams.get("metric") ? { status: 400, body: { error: 'agg "sum:other{*} + avg": want one of avg, sum' } } : undefined)));
    const user = userEvent.setup();
    const router = renderAt(`/metrics/explorer?${new URLSearchParams({ metric: "m", agg: "sum:other{*} + avg" })}`);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("ozyd refused this link's M1 parameters, metric=m&agg=sum:other{*} + avg:");
    expect(alert).toHaveTextContent('want one of avg, sum');
    expect(screen.queryByTestId("chart")).not.toBeInTheDocument();
    expect(box()).toHaveValue("");
    await user.selectOptions(screen.getByRole("combobox", { name: "Time range" }), "4h");
    await waitFor(() => expect(params(router).get("range")).toBe("4h"));
    expect(params(router).get("agg")).toBe("sum:other{*} + avg");
    // And Clear is the way out of a link that cannot be charted.
    await user.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(params(router).has("metric")).toBe(false));
    expect(params(router).has("agg")).toBe(false);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("says so when ozyd answers an M1 link without saying what it ran", async () => {
    mockApi(over((u) => (u.searchParams.get("metric") ? { body: queryBody("") } : undefined)));
    const router = renderAt("/metrics/explorer?metric=m");
    expect(await screen.findByRole("alert")).toHaveTextContent(/without saying what query they mean/);
    expect(params(router).get("metric")).toBe("m");
  });

  it("says it is asking while the translation is on its way", async () => {
    mockApi(over((u) => (u.searchParams.get("metric") ? new Promise(() => {}) : undefined)));
    renderAt("/metrics/explorer?metric=m");
    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: "Chart" })).getByRole("status")).toHaveTextContent(
        "Asking ozyd what this link's M1 parameters, metric=m, mean as a query…",
      ),
    );
  });

  // Nothing to ask, and "run" must not mean "clear the chart".
  it("runs nothing from a blank box, by button or by key", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/explorer");
    await user.click(await screen.findByRole("combobox", { name: "Query" }));
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    // The empty page's own case: Ctrl+Enter must not refetch a query of "".
    await new Promise((r) => setTimeout(r, 50));
    expect(queryCalls(f)).toHaveLength(0);

    await router.navigate(at(Q));
    await screen.findByTestId("chart");
    await user.clear(box());
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(params(router).get("q")).toBe(Q);
    expect(screen.getByTestId("chart")).toBeInTheDocument();
    expect(queryCalls(f).filter((u) => u.searchParams.get("q") === "")).toHaveLength(0);
    // It says what the empty box means, rather than "edited, not run".
    expect(screen.getByText(/The box is empty; the chart is still the query in the URL/)).toBeInTheDocument();
    expect(screen.queryByText(/Edited, not run/)).not.toBeInTheDocument();
    // An empty box is not an edit that Save to dashboard would be passing over.
    expect(screen.queryByText(/Saves the charted query/)).not.toBeInTheDocument();
  });

  // Clearing is its own action, now that Run never does it.
  it("clears the chart with Clear", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt(at(Q, "&range=4h"));
    await screen.findByTestId("chart");
    await user.click(screen.getByRole("button", { name: "Clear" }));
    await waitFor(() => expect(params(router).has("q")).toBe(false));
    expect(params(router).get("range")).toBe("4h");
    expect(box()).toHaveValue("");
    expect(screen.getByText("Write a query and run it (Ctrl+Enter) to chart it.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clear" })).not.toBeInTheDocument();
  });

  it("says an edit has not been run, and keeps charting the URL's query until it is", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt(at(Q));
    await screen.findByTestId("chart");
    await user.type(box(), "x");
    expect(screen.getByText(/Edited, not run/)).toBeInTheDocument();
    expect(params(router).get("q")).toBe(Q);
    expect(queryCalls(f)).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(params(router).get("q")).toBe(`${Q}x`));
    expect(screen.queryByText(/Edited, not run/)).not.toBeInTheDocument();
  });

  // Back and forward step through runs; the box has to follow, or it would
  // show one query over another's chart.
  it("follows the URL when it changes under the box", async () => {
    mockApi();
    const router = renderAt(at(Q));
    await screen.findByTestId("chart");
    await router.navigate(at("avg:system.cpu.user{*}"));
    await waitFor(() => expect(box()).toHaveValue("avg:system.cpu.user{*}"));
    await router.navigate(-1);
    await waitFor(() => expect(box()).toHaveValue(Q));
  });

  it("says whose answer is on screen while the next query runs", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    mockApi(
      over((u) =>
        u.pathname === "/api/v1/query" && u.searchParams.get("q") === "avg:slow{*}"
          ? gate.then(() => ({ body: queryBody("avg:slow{*}", { series: [] }) }))
          : undefined,
      ),
    );
    const router = renderAt(at(Q));
    await screen.findByTestId("chart");
    await router.navigate(at("avg:slow{*}"));
    const status = await screen.findByText(/the chart below is the previous query's answer/);
    expect(status).toHaveTextContent(Q);
    expect(screen.getByTestId("chart").closest(".opacity-40")).not.toBeNull();
    release();
    expect(await screen.findByText("No data for this query in the selected time range.")).toBeInTheDocument();
    expect(screen.queryByTestId("chart")).not.toBeInTheDocument();
  });

  it("tells a refused query from one that was not answered", async () => {
    mockApi(
      over((u) =>
        u.pathname === "/api/v1/query"
          ? u.searchParams.get("q") === "bad"
            ? { status: 400, body: { error: 'unknown aggregator "bad"' } }
            : { status: 503, body: { error: "the query ran out of time" } }
          : undefined,
      ),
    );
    const router = renderAt(at("bad"));
    const refused = await screen.findByRole("alert");
    expect(refused).toHaveTextContent('ozyd refused this query:unknown aggregator "bad"');
    expect(refused).not.toHaveTextContent(/retry/);
    await router.navigate(at("sum:big{*}"));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("ozyd did not answer this query:the query ran out of time"));
    expect(screen.getByRole("alert")).toHaveTextContent("Reload or run it again to retry.");
  });

  // A previous query's lines under a failing query's text would read as its answer.
  it("does not draw the previous answer under a query that failed", async () => {
    mockApi(over((u) => (u.pathname === "/api/v1/query" && u.searchParams.get("q") === "bad" ? { status: 400, body: { error: "no" } } : undefined)));
    const router = renderAt(at(Q));
    await screen.findByTestId("chart");
    await router.navigate(at("bad"));
    await screen.findByText(/ozyd refused this query/);
    expect(screen.queryByTestId("chart")).not.toBeInTheDocument();
  });

  it("keeps the last answer when a refresh fails, and says when it is from", async () => {
    let calls = 0;
    mockApi(over((u) => (u.pathname === "/api/v1/query" && ++calls > 1 ? { status: 503, body: { error: "out of time" } } : undefined)));
    const user = userEvent.setup();
    renderAt(at(Q));
    await screen.findByTestId("chart");
    // Run on an unedited box asks the same question again.
    await user.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/The last refresh failed \(out of time\)\. Showing the answer from/);
    expect(screen.getByTestId("chart")).toBeInTheDocument();
  });

  it("draws two identical warnings as two", async () => {
    const complain = vi.spyOn(console, "error").mockImplementation(() => {});
    mockApi(over((u) => (u.pathname === "/api/v1/query" ? { body: queryBody(Q, { warnings: ["same", "same"] }) } : undefined)));
    renderAt(at(Q));
    await screen.findByTestId("chart");
    expect(within(screen.getByRole("list", { name: "Warnings" })).getAllByRole("listitem")).toHaveLength(2);
    expect(complain.mock.calls.flat().join(" ")).not.toMatch(/same key/);
    complain.mockRestore();
  });

  it("shows the answer's warnings, and the canonical spelling when it differs", async () => {
    mockApi(over((u) => (u.pathname === "/api/v1/query" ? { body: queryBody("sum:x{a:b}", { warnings: ["a group was dropped"] }) } : undefined)));
    renderAt(at("SUM:x{A:b}"));
    await screen.findByTestId("chart");
    expect(within(screen.getByRole("list", { name: "Warnings" })).getByText("a group was dropped")).toBeInTheDocument();
    expect(screen.getByText("sum:x{a:b}")).toBeInTheDocument();
  });

  it("shows running, then an empty state when nothing matched", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    mockApi(over((u) => (u.pathname === "/api/v1/query" ? gate.then(() => ({ body: queryBody("avg:nothing{*}", { series: [] }) })) : undefined)));
    renderAt(at("avg:nothing{*}"));
    expect(await screen.findByText("Running…")).toBeInTheDocument();
    release();
    expect(await screen.findByText("No data for this query in the selected time range.")).toBeInTheDocument();
  });

  it("changes the range and auto-refresh through the URL", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt(at(Q));
    await screen.findByTestId("chart");
    await user.selectOptions(screen.getByRole("combobox", { name: "Time range" }), "4h");
    await waitFor(() => expect(params(router).get("range")).toBe("4h"));
    await user.click(screen.getByRole("checkbox", { name: /Auto-refresh/ }));
    await waitFor(() => expect(params(router).get("live")).toBe("0"));
    expect(params(router).get("q")).toBe(Q);
  });

  it("applies a custom absolute range and turns auto-refresh off", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt(at(Q));
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
});

describe("Save to dashboard", () => {
  const link = () => screen.getByRole("link", { name: "Open in the editor" });

  it("opens a new dashboard, or a chosen one, with the charted query to add", async () => {
    mockApi();
    const user = userEvent.setup();
    renderAt(at(Q));
    await screen.findByTestId("chart");
    expect(link()).toHaveAttribute("href", `/dashboards/new?${new URLSearchParams({ add: Q })}`);
    const select = screen.getByRole("combobox", { name: "Save to dashboard" });
    await within(select).findByRole("option", { name: "Checkout" });
    await user.selectOptions(select, "4");
    expect(link()).toHaveAttribute("href", `/dashboards/4/edit?${new URLSearchParams({ add: Q })}`);
  });

  // An edit saved to a provisioned dashboard is undone at the next restart.
  it("lists a provisioned dashboard as not choosable, rather than hiding it", async () => {
    mockApi();
    renderAt(at(Q));
    const option = await screen.findByRole("option", { name: "Home (from a file, edit the file)" });
    expect(option).toBeDisabled();
  });

  it("has nothing to save before a query is run", async () => {
    mockApi();
    renderAt("/metrics/explorer");
    expect(await screen.findByText(/Run a query first/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open in the editor" })).not.toBeInTheDocument();
  });

  it("saves the charted query, and says so when the box holds an unrun edit", async () => {
    mockApi();
    const user = userEvent.setup();
    renderAt(at(Q));
    await screen.findByTestId("chart");
    await user.type(box(), " + 1");
    expect(screen.getByText(/Saves the charted query/)).toHaveTextContent(Q);
    expect(link()).toHaveAttribute("href", `/dashboards/new?${new URLSearchParams({ add: Q })}`);
  });

  it("still offers a new dashboard when the list cannot be loaded", async () => {
    mockApi(over((u) => (u.pathname === "/api/v1/dashboards" ? { status: 403, body: { error: "boom" } } : undefined)));
    renderAt(at(Q));
    expect(await screen.findByText(/Could not list the dashboards \(boom\); a new one still works/)).toBeInTheDocument();
    expect(link()).toHaveAttribute("href", `/dashboards/new?${new URLSearchParams({ add: Q })}`);
  });
});
